package shared

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ITunnelManager is the interface for tunnel management
type ITunnelManager interface {
	IsRunning() bool
	Stop() map[string]interface{}
}

// AppMonitor monitors the main application
type AppMonitor struct {
	tunnelManager       ITunnelManager
	pingURL             string
	checkInterval       time.Duration
	maxFailures         int
	consecutiveFailures int
	stopChan            chan struct{}
	wg                  sync.WaitGroup
}

// NewAppMonitor creates a new app monitor
func NewAppMonitor(tm ITunnelManager, pingURL string, checkInterval time.Duration) *AppMonitor {
	return &AppMonitor{
		tunnelManager:       tm,
		pingURL:             pingURL,
		checkInterval:       checkInterval,
		maxFailures:         3,
		consecutiveFailures: 0,
		stopChan:            make(chan struct{}),
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
	close(am.stopChan)
	am.wg.Wait()
	log.Println("App monitor stopped")
}

// checkAppAlive checks if main app is responding
func (am *AppMonitor) checkAppAlive() bool {
	client := &http.Client{
		Timeout: 2 * time.Second,
	}

	resp, err := client.Get(am.pingURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	var buf [256]byte
	n, _ := resp.Body.Read(buf[:])
	body := strings.ToLower(string(buf[:n]))

	return strings.Contains(body, "pong")
}

// monitorLoop is the main monitoring loop
func (am *AppMonitor) monitorLoop() {
	defer am.wg.Done()

	ticker := time.NewTicker(am.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-am.stopChan:
			return
		case <-ticker.C:
			if am.tunnelManager.IsRunning() {
				appAlive := am.checkAppAlive()

				if appAlive {
					if am.consecutiveFailures > 0 {
						log.Println("Main app reconnected")
					}
					am.consecutiveFailures = 0
				} else {
					am.consecutiveFailures++

					if am.consecutiveFailures >= am.maxFailures {
						log.Printf("Main app not responding (%d checks), stopping tunnel",
							am.consecutiveFailures)

						result := am.tunnelManager.Stop()
						log.Printf("Tunnel stopped due to app disconnection: %v", result)

						am.consecutiveFailures = 0
					}
				}
			}
		}
	}
}
