package shared

import (
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ITunnelManager is the interface for tunnel management
type ITunnelManager interface {
	// IsActive reports whether the tunnel is running or about to be restarted.
	IsActive() bool
	Stop() map[string]interface{}
}

// AppMonitor stops the tunnel when the main application is gone, so the
// system isn't left routed through a TUN nobody manages.
//
// A verdict needs both maxFailures consecutive failed pings and
// AppUnresponsiveTimeout without a successful one, so a briefly busy app
// doesn't lose its tunnel. Right after a system sleep the app gets
// AppResumeGrace to come back.
type AppMonitor struct {
	tunnelManager       ITunnelManager
	pingURL             string
	checkInterval       time.Duration
	maxFailures         int
	consecutiveFailures int
	lastAlive           time.Time
	graceUntil          time.Time
	client              *http.Client
	stopChan            chan struct{}
	stopOnce            sync.Once
	wg                  sync.WaitGroup
}

// NewAppMonitor creates a new app monitor
func NewAppMonitor(tm ITunnelManager, pingURL string, checkInterval time.Duration) *AppMonitor {
	return &AppMonitor{
		tunnelManager: tm,
		pingURL:       pingURL,
		checkInterval: checkInterval,
		maxFailures:   3,
		client: &http.Client{
			Timeout:   2 * time.Second,
			Transport: &http.Transport{DisableKeepAlives: true},
		},
		stopChan: make(chan struct{}),
	}
}

// Start starts monitoring
func (am *AppMonitor) Start() {
	am.wg.Add(1)
	go am.monitorLoop()
	log.Println("App monitor started")
}

// Stop stops monitoring
func (am *AppMonitor) Stop() {
	am.stopOnce.Do(func() { close(am.stopChan) })
	am.wg.Wait()
	log.Println("App monitor stopped")
}

// checkAppAlive checks if main app is responding
func (am *AppMonitor) checkAppAlive() bool {
	resp, err := am.client.Get(am.pingURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return strings.Contains(strings.ToLower(string(body)), "pong")
}

// sleptBetween reports whether the machine was suspended (or the service frozen)
// between two ticks: the wall clock keeps running during sleep while the
// monotonic clock does not, and a frozen process sees a huge monotonic gap.
func (am *AppMonitor) sleptBetween(prev, now time.Time) bool {
	mono := now.Sub(prev)
	wall := now.Round(0).Sub(prev.Round(0))
	return wall-mono > 5*time.Second || mono > 5*am.checkInterval
}

// monitorLoop is the main monitoring loop
func (am *AppMonitor) monitorLoop() {
	defer am.wg.Done()

	ticker := time.NewTicker(am.checkInterval)
	defer ticker.Stop()

	prevTick := time.Now()
	am.lastAlive = prevTick

	for {
		select {
		case <-am.stopChan:
			return
		case <-ticker.C:
		}

		now := time.Now()
		if am.sleptBetween(prevTick, now) {
			log.Printf("System resume detected, app liveness checks paused for %v", AppResumeGrace)
			am.graceUntil = now.Add(AppResumeGrace)
			am.consecutiveFailures = 0
			am.lastAlive = now
		}
		prevTick = now

		if !am.tunnelManager.IsActive() {
			am.consecutiveFailures = 0
			am.lastAlive = now
			continue
		}

		if am.checkAppAlive() {
			if am.consecutiveFailures > 0 {
				log.Println("Main app reconnected")
			}
			am.consecutiveFailures = 0
			am.lastAlive = time.Now()
			continue
		}

		am.consecutiveFailures++

		if now.Before(am.graceUntil) {
			continue
		}

		if am.consecutiveFailures >= am.maxFailures && time.Since(am.lastAlive) >= AppUnresponsiveTimeout {
			log.Printf("Main app not responding (%d checks, %v), stopping tunnel",
				am.consecutiveFailures, time.Since(am.lastAlive).Round(time.Second))

			result := am.tunnelManager.Stop()
			log.Printf("Tunnel stopped due to app disconnection: %v", result)

			am.consecutiveFailures = 0
			am.lastAlive = time.Now()
		}
	}
}
