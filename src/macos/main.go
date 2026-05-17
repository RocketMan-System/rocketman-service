package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/xanderwp/proxybridgeservice/src/shared"
)

// Configuration
const (
	APP_PATH                   = "/Applications/RocketMan Proxy Bridge.app"
	QUARANTINE_CHECK_INTERVAL  = 5 * time.Second
	QUARANTINE_REMOVAL_ENABLED = true
)

// recentLogs is the process-wide log store (shared with the HTTP /logs endpoint).
var recentLogs = shared.NewRecentLogStore(shared.LOG_RETENTION, shared.LOG_MAX_ITEMS)

// ---------------------------------------------------------------------------
// TunnelManager — macOS-specific (Setsid + unix signals)
// ---------------------------------------------------------------------------

// TunnelManager manages the sing-box process on macOS.
type TunnelManager struct {
	process            *os.Process
	mu                 sync.Mutex
	singboxPath        string
	configPath         string
	logWriter          *shared.SingboxLogWriter
	isRunning          bool
	restartAttempts    int
	lastStartTime      time.Time
	stopMonitorChan    chan struct{}
	monitorWg          sync.WaitGroup
}

// NewTunnelManager creates a new tunnel manager.
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		logWriter:       &shared.SingboxLogWriter{Store: recentLogs},
		stopMonitorChan: make(chan struct{}),
	}
}

// Start starts the tunnel with given parameters.
func (tm *TunnelManager) Start(username, appname string) map[string]interface{} {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.isRunning {
		return map[string]interface{}{
			"status": "already_running",
			"pid":    tm.process.Pid,
		}
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		homeDir = filepath.Join("/Users", username)
	}

	basePath := filepath.Join(homeDir, "Library", "Application Support", appname, ".sing-box")
	tm.singboxPath = filepath.Join(basePath, "sing-box")
	tm.configPath = filepath.Join(basePath, "sing-box-auto.json")

	if _, err := os.Stat(tm.singboxPath); os.IsNotExist(err) {
		return map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("sing-box not found: %s", tm.singboxPath),
		}
	}

	if _, err := os.Stat(tm.configPath); os.IsNotExist(err) {
		return map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Config not found: %s", tm.configPath),
		}
	}

	cmd := exec.Command(tm.singboxPath, "run", "-c", tm.configPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // new session / process group
	}
	cmd.Stdout = io.MultiWriter(os.Stdout, tm.logWriter)
	cmd.Stderr = io.MultiWriter(os.Stderr, tm.logWriter)

	if err := cmd.Start(); err != nil {
		return map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Failed to start process: %v", err),
		}
	}

	tm.process = cmd.Process
	tm.isRunning = true

	time.Sleep(500 * time.Millisecond)

	if !shared.IsProcessAlive(tm.process) {
		tm.isRunning = false
		return map[string]interface{}{
			"status":  "error",
			"message": "Process exited immediately",
		}
	}

	log.Printf("Tunnel started: PID=%d, singbox=%s, config=%s",
		tm.process.Pid, tm.singboxPath, tm.configPath)

	// Reset restart attempts and start monitoring
	tm.restartAttempts = 0
	tm.lastStartTime = time.Now()
	tm.monitorWg.Add(1)
	go tm.monitor()

	return map[string]interface{}{
		"status":       "started",
		"pid":          tm.process.Pid,
		"singbox_path": tm.singboxPath,
		"config_path":  tm.configPath,
	}
}

// Stop stops the tunnel.
func (tm *TunnelManager) Stop() map[string]interface{} {
	tm.mu.Lock()

	if !tm.isRunning {
		tm.mu.Unlock()
		return map[string]interface{}{"status": "not_running"}
	}

	shared.GracefulStop(tm.process, 5*time.Second)

	tm.process = nil
	tm.isRunning = false

	// Stop monitor goroutine
	close(tm.stopMonitorChan)
	tm.stopMonitorChan = make(chan struct{})
	tm.mu.Unlock()

	tm.monitorWg.Wait()

	log.Println("Tunnel stopped")
	return map[string]interface{}{"status": "stopped"}
}

// IsRunning checks if tunnel is running.
func (tm *TunnelManager) IsRunning() bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if !tm.isRunning {
		return false
	}

	if !shared.IsProcessAlive(tm.process) {
		tm.isRunning = false
		return false
	}

	return true
}

// GetStatus returns tunnel status.
func (tm *TunnelManager) GetStatus() map[string]interface{} {
	if tm.IsRunning() {
		tm.mu.Lock()
		defer tm.mu.Unlock()

		return map[string]interface{}{
			"status":       "running",
			"pid":          tm.process.Pid,
			"singbox_path": tm.singboxPath,
			"config_path":  tm.configPath,
		}
	}

	return map[string]interface{}{"status": "stopped"}
}

// monitor watches the tunnel process and handles restarts on crash.
func (tm *TunnelManager) monitor() {
	defer tm.monitorWg.Done()

	for {
		// Wait for process to exit or stop signal
		select {
		case <-tm.stopMonitorChan:
			return
		default:
		}

		tm.mu.Lock()
		if !tm.isRunning || tm.process == nil {
			tm.mu.Unlock()
			return
		}
		process := tm.process
		tm.mu.Unlock()

		// Wait for process exit
		_, err := process.Wait()

		tm.mu.Lock()
		if !tm.isRunning || tm.process != process {
			tm.mu.Unlock()
			return
		}

		// Process crashed
		log.Printf("Tunnel process crashed: %v", err)

		// Check if 10 seconds have passed since start
		uptime := time.Since(tm.lastStartTime)
		if uptime >= 10*time.Second {
			log.Printf("Process uptime was %v (>= 10s), resetting restart attempts counter", uptime)
			tm.restartAttempts = 0
		}

		// Check if we can retry
		if tm.restartAttempts >= 5 {
			log.Printf("Maximum restart attempts (%d) reached, giving up", tm.restartAttempts)
			tm.process = nil
			tm.isRunning = false
			tm.mu.Unlock()
			return
		}

		tm.restartAttempts++
		log.Printf("Attempting to restart tunnel (attempt %d/5)", tm.restartAttempts)
		tm.process = nil
		tm.isRunning = false
		tm.mu.Unlock()

		// Wait 1 second before retry
		select {
		case <-time.After(1 * time.Second):
		case <-tm.stopMonitorChan:
			return
		}

		// Try to restart
		tm.mu.Lock()
		if !tm.isRunning {
			tm.lastStartTime = time.Now()

			cmd := exec.Command(tm.singboxPath, "run", "-c", tm.configPath)
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Setsid: true,
			}
			cmd.Stdout = io.MultiWriter(os.Stdout, tm.logWriter)
			cmd.Stderr = io.MultiWriter(os.Stderr, tm.logWriter)

			if err := cmd.Start(); err != nil {
				log.Printf("Failed to restart tunnel: %v", err)
				tm.mu.Unlock()
				continue
			}

			tm.process = cmd.Process
			tm.isRunning = true
			log.Printf("Tunnel restarted: PID=%d", tm.process.Pid)
		}
		tm.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// PlatformHTTPHandler — bridges shared.ITunnelOperations ↔ TunnelManager
// ---------------------------------------------------------------------------

type PlatformHTTPHandler struct {
	tunnelManager *TunnelManager
}

func (h *PlatformHTTPHandler) Start(username, appname string) map[string]interface{} {
	return h.tunnelManager.Start(username, appname)
}

func (h *PlatformHTTPHandler) Stop() map[string]interface{} {
	return h.tunnelManager.Stop()
}

func (h *PlatformHTTPHandler) GetStatus() map[string]interface{} {
	return h.tunnelManager.GetStatus()
}

// ---------------------------------------------------------------------------
// macOS-specific: QuarantineMonitor
// ---------------------------------------------------------------------------

// QuarantineMonitor automatically removes com.apple.quarantine from the app bundle.
type QuarantineMonitor struct {
	appPath       string
	checkInterval time.Duration
	enabled       bool
	stopChan      chan struct{}
	wg            sync.WaitGroup
}

// NewQuarantineMonitor creates a new quarantine monitor.
func NewQuarantineMonitor(appPath string, checkInterval time.Duration, enabled bool) *QuarantineMonitor {
	return &QuarantineMonitor{
		appPath:       appPath,
		checkInterval: checkInterval,
		enabled:       enabled,
		stopChan:      make(chan struct{}),
	}
}

// Start starts quarantine monitoring.
func (qm *QuarantineMonitor) Start() {
	if !qm.enabled {
		log.Println("Quarantine removal is disabled")
		return
	}

	if _, err := os.Stat(qm.appPath); os.IsNotExist(err) {
		log.Printf("App path does not exist, quarantine monitor disabled: %s", qm.appPath)
		return
	}

	qm.wg.Add(1)
	go qm.monitorLoop()
	log.Printf("Quarantine monitor started for: %s (check interval: %v)", qm.appPath, qm.checkInterval)
}

// Stop stops quarantine monitoring.
func (qm *QuarantineMonitor) Stop() {
	if !qm.enabled {
		return
	}
	close(qm.stopChan)
	qm.wg.Wait()
	log.Println("Quarantine monitor stopped")
}

func (qm *QuarantineMonitor) hasQuarantine() bool {
	cmd := exec.Command("xattr", "-p", "com.apple.quarantine", qm.appPath)
	return cmd.Run() == nil
}

func (qm *QuarantineMonitor) removeQuarantine() error {
	log.Printf("Removing quarantine from: %s", qm.appPath)

	cmd := exec.Command("xattr", "-rd", "com.apple.quarantine", qm.appPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to remove quarantine: %v, output: %s", err, string(output))
	}

	log.Printf("Quarantine removed successfully from: %s", qm.appPath)
	return nil
}

func (qm *QuarantineMonitor) monitorLoop() {
	defer qm.wg.Done()

	if qm.hasQuarantine() {
		log.Printf("Quarantine detected on startup: %s", qm.appPath)
		if err := qm.removeQuarantine(); err != nil {
			log.Printf("Error removing quarantine: %v", err)
		}
	} else {
		log.Printf("No quarantine detected: %s", qm.appPath)
	}

	ticker := time.NewTicker(qm.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-qm.stopChan:
			return
		case <-ticker.C:
			if qm.hasQuarantine() {
				log.Printf("Quarantine detected: %s", qm.appPath)
				if err := qm.removeQuarantine(); err != nil {
					log.Printf("Error removing quarantine: %v", err)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	versionFlag := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("RocketMan Tunnel Service v%s\n", shared.VERSION)
		os.Exit(0)
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetOutput(io.MultiWriter(os.Stderr, &shared.LogMirrorWriter{Store: recentLogs}))
	log.Println("RocketMan Tunnel Service starting...")

	tunnelManager := NewTunnelManager()
	appMonitor := shared.NewAppMonitor(tunnelManager, shared.APP_PING_URL, shared.APP_CHECK_INTERVAL)
	appMonitor.Start()

	quarantineMonitor := NewQuarantineMonitor(APP_PATH, QUARANTINE_CHECK_INTERVAL, QUARANTINE_REMOVAL_ENABLED)
	quarantineMonitor.Start()

	platformHandler := &PlatformHTTPHandler{tunnelManager: tunnelManager}
	handler := shared.NewHTTPHandlerWithLogs(platformHandler, recentLogs, shared.LOG_RETENTION, shared.LOG_TITLE)
	server := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", shared.HTTP_PORT),
		Handler: handler,
	}

	go func() {
		log.Printf("HTTP server listening on port %d", shared.HTTP_PORT)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutdown signal received, stopping service...")

	quarantineMonitor.Stop()
	appMonitor.Stop()
	tunnelManager.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	log.Println("Service stopped")
}
