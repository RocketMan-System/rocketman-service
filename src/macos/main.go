package main

import (
	"context"
	"flag"
	"fmt"
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
	APP_PATH                    = "/Applications/RocketMan Proxy Bridge.app"
	QUARANTINE_CHECK_INTERVAL   = 5 * time.Second
	QUARANTINE_REMOVAL_ENABLED  = true
)

// TunnelManager manages the sing-box process
type TunnelManager struct {
	process     *os.Process
	mu          sync.Mutex
	singboxPath string
	configPath  string
	isRunning   bool
}

// NewTunnelManager creates a new tunnel manager
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		isRunning: false,
	}
}

// Start starts the tunnel with given parameters
func (tm *TunnelManager) Start(username, appname string) map[string]interface{} {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.isRunning {
		return map[string]interface{}{
			"status": "already_running",
			"pid":    tm.process.Pid,
		}
	}

	// Build paths
	homeDir, err := os.UserHomeDir()
	if err != nil {
		// Fallback to /Users/{username}
		homeDir = filepath.Join("/Users", username)
	}

	basePath := filepath.Join(homeDir, "Library", "Application Support", appname, ".sing-box")
	tm.singboxPath = filepath.Join(basePath, "sing-box")
	tm.configPath = filepath.Join(basePath, "sing-box-auto.json")

	// Check if files exist
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

	// Start sing-box process
	cmd := exec.Command(tm.singboxPath, "run", "-c", tm.configPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // Create new process group
	}

	// Start the process
	if err := cmd.Start(); err != nil {
		return map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Failed to start process: %v", err),
		}
	}

	tm.process = cmd.Process
	tm.isRunning = true

	// Give process time to start
	time.Sleep(500 * time.Millisecond)

	// Check if process is still running
	if err := tm.process.Signal(syscall.Signal(0)); err != nil {
		tm.isRunning = false
		return map[string]interface{}{
			"status":  "error",
			"message": "Process exited immediately",
		}
	}

	log.Printf("Tunnel started: PID=%d, singbox=%s, config=%s", 
		tm.process.Pid, tm.singboxPath, tm.configPath)

	return map[string]interface{}{
		"status":       "started",
		"pid":          tm.process.Pid,
		"singbox_path": tm.singboxPath,
		"config_path":  tm.configPath,
	}
}

// Stop stops the tunnel
func (tm *TunnelManager) Stop() map[string]interface{} {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if !tm.isRunning {
		return map[string]interface{}{
			"status": "not_running",
		}
	}

	// Send SIGTERM
	if err := tm.process.Signal(syscall.SIGTERM); err != nil {
		log.Printf("Error sending SIGTERM: %v", err)
	}

	// Wait for process to exit (with timeout)
	done := make(chan error, 1)
	go func() {
		_, err := tm.process.Wait()
		done <- err
	}()

	select {
	case <-done:
		// Process exited gracefully
	case <-time.After(5 * time.Second):
		// Timeout - force kill
		log.Println("Process didn't exit gracefully, sending SIGKILL")
		tm.process.Signal(syscall.SIGKILL)
		tm.process.Wait()
	}

	tm.process = nil
	tm.isRunning = false

	log.Println("Tunnel stopped")

	return map[string]interface{}{
		"status": "stopped",
	}
}

// IsRunning checks if tunnel is running
func (tm *TunnelManager) IsRunning() bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if !tm.isRunning {
		return false
	}

	// Check if process is still alive
	if err := tm.process.Signal(syscall.Signal(0)); err != nil {
		tm.isRunning = false
		return false
	}

	return true
}

// GetStatus returns tunnel status
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

	return map[string]interface{}{
		"status": "stopped",
	}
}

// PlatformHTTPHandler wraps TunnelManager to implement shared.ITunnelOperations
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

// QuarantineMonitor automatically checks and removes quarantine from app
type QuarantineMonitor struct {
	appPath       string
	checkInterval time.Duration
	enabled       bool
	stopChan      chan struct{}
	wg            sync.WaitGroup
}

// NewQuarantineMonitor creates a new quarantine monitor
func NewQuarantineMonitor(appPath string, checkInterval time.Duration, enabled bool) *QuarantineMonitor {
	return &QuarantineMonitor{
		appPath:       appPath,
		checkInterval: checkInterval,
		enabled:       enabled,
		stopChan:      make(chan struct{}),
	}
}

// Start starts quarantine monitoring
func (qm *QuarantineMonitor) Start() {
	if !qm.enabled {
		log.Println("Quarantine removal is disabled")
		return
	}

	// Check if app path exists
	if _, err := os.Stat(qm.appPath); os.IsNotExist(err) {
		log.Printf("App path does not exist, quarantine monitor disabled: %s", qm.appPath)
		return
	}

	qm.wg.Add(1)
	go qm.monitorLoop()
	log.Printf("Quarantine monitor started for: %s (check interval: %v)", qm.appPath, qm.checkInterval)
}

// Stop stops quarantine monitoring
func (qm *QuarantineMonitor) Stop() {
	if !qm.enabled {
		return
	}
	close(qm.stopChan)
	qm.wg.Wait()
	log.Println("Quarantine monitor stopped")
}

// hasQuarantine checks if app has quarantine attribute
func (qm *QuarantineMonitor) hasQuarantine() bool {
	cmd := exec.Command("xattr", "-p", "com.apple.quarantine", qm.appPath)
	err := cmd.Run()
	// If command succeeds (exit code 0), attribute exists
	return err == nil
}

// removeQuarantine removes quarantine attribute from app
func (qm *QuarantineMonitor) removeQuarantine() error {
	log.Printf("Removing quarantine from: %s", qm.appPath)
	
	cmd := exec.Command("xattr", "-rd", "com.apple.quarantine", qm.appPath)
	output, err := cmd.CombinedOutput()
	
	if err != nil {
		return fmt.Errorf("failed to remove quarantine: %v, output: %s", err, string(output))
	}
	
	log.Printf("✓ Quarantine removed successfully from: %s", qm.appPath)
	return nil
}

// monitorLoop is the main monitoring loop
func (qm *QuarantineMonitor) monitorLoop() {
	defer qm.wg.Done()

	// Check immediately on start
	if qm.hasQuarantine() {
		log.Printf("⚠ Quarantine detected on startup: %s", qm.appPath)
		if err := qm.removeQuarantine(); err != nil {
			log.Printf("Error removing quarantine: %v", err)
		}
	} else {
		log.Printf("✓ No quarantine detected: %s", qm.appPath)
	}

	ticker := time.NewTicker(qm.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-qm.stopChan:
			return
		case <-ticker.C:
			if qm.hasQuarantine() {
				log.Printf("⚠ Quarantine detected: %s", qm.appPath)
				if err := qm.removeQuarantine(); err != nil {
					log.Printf("Error removing quarantine: %v", err)
				}
			}
		}
	}
}

func main() {
	// Parse flags
	versionFlag := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("RocketMan Tunnel Service v%s\n", shared.VERSION)
		os.Exit(0)
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("RocketMan Tunnel Service starting...")

	// Create tunnel manager
	tunnelManager := NewTunnelManager()

	// Create app monitor using shared
	appMonitor := shared.NewAppMonitor(tunnelManager, shared.APP_PING_URL, shared.APP_CHECK_INTERVAL)
	appMonitor.Start()

	// Create quarantine monitor
	quarantineMonitor := NewQuarantineMonitor(APP_PATH, QUARANTINE_CHECK_INTERVAL, QUARANTINE_REMOVAL_ENABLED)
	quarantineMonitor.Start()

	// Create HTTP server using shared handler
	handler := shared.NewHTTPHandler(&PlatformHTTPHandler{tunnelManager: tunnelManager})
	server := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", shared.HTTP_PORT),
		Handler: handler,
	}

	// Start server in goroutine
	go func() {
		log.Printf("HTTP server listening on port %d", shared.HTTP_PORT)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Println("Shutdown signal received, stopping service...")

	// Graceful shutdown
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