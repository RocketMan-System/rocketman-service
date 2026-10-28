package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
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

// PID_FILE lets a restarted service kill a sing-box orphaned by a crashed predecessor
// (Setsid moves sing-box out of the daemon's process group, so launchd won't).
const PID_FILE = "/var/run/rocketman-tunnel-singbox.pid"

// GRACEFUL_STOP_TIMEOUT keeps /stop well inside the app's 5s HTTP timeout.
const GRACEFUL_STOP_TIMEOUT = 2500 * time.Millisecond

// TunnelManager manages the sing-box process on macOS.
type TunnelManager struct {
	mu          sync.Mutex
	singboxPath string
	configPath  string
	logWriter   *shared.SingboxLogWriter
	pidFile     shared.PidFile
	supervisor  *shared.Supervisor
}

// NewTunnelManager creates a new tunnel manager.
func NewTunnelManager() *TunnelManager {
	tm := &TunnelManager{
		logWriter: &shared.SingboxLogWriter{Store: recentLogs},
		pidFile:   shared.PidFile{Path: PID_FILE},
	}
	tm.supervisor = shared.NewSupervisor(shared.SupervisorHooks{
		Launch:          tm.launch,
		Interrupt:       shared.SignalTerm,
		Kill:            shared.ForceKill,
		OnStarted:       tm.onStarted,
		OnExited:        tm.pidFile.Remove,
		GracefulTimeout: GRACEFUL_STOP_TIMEOUT,
	})
	return tm
}

// resolveBasePath finds the app's .sing-box directory for username.
// The daemon runs as root, whose home is not the user's; the first candidate
// that actually contains sing-box wins.
func resolveBasePath(username, appname string) string {
	var homes []string

	if homeDir, err := os.UserHomeDir(); err == nil {
		// Kept first for compatibility with older releases.
		homes = append(homes, homeDir)
	}
	if username != "" {
		if u, err := user.Lookup(username); err == nil && u.HomeDir != "" {
			homes = append(homes, u.HomeDir)
		}
		homes = append(homes, filepath.Join("/Users", username))
	}

	for _, home := range homes {
		basePath := filepath.Join(home, "Library", "Application Support", appname, ".sing-box")
		if _, err := os.Stat(filepath.Join(basePath, "sing-box")); err == nil {
			return basePath
		}
	}

	fallback := filepath.Join("/Users", username)
	if len(homes) > 0 {
		fallback = homes[0]
	}
	return filepath.Join(fallback, "Library", "Application Support", appname, ".sing-box")
}

// Start starts the tunnel with given parameters.
func (tm *TunnelManager) Start(username, appname string) map[string]interface{} {
	if pid, ok := tm.supervisor.RunningPID(); ok {
		return map[string]interface{}{
			"status": "already_running",
			"pid":    pid,
		}
	}

	basePath := resolveBasePath(username, appname)
	singboxPath := filepath.Join(basePath, "sing-box")
	configPath := filepath.Join(basePath, "sing-box-auto.json")

	if _, err := os.Stat(singboxPath); os.IsNotExist(err) {
		return map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("sing-box not found: %s", singboxPath),
		}
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Config not found: %s", configPath),
		}
	}

	tm.mu.Lock()
	tm.singboxPath = singboxPath
	tm.configPath = configPath
	tm.mu.Unlock()

	pid, already, err := tm.supervisor.Start()
	if err != nil {
		log.Printf("Tunnel start failed: %v", err)
		return map[string]interface{}{
			"status":  "error",
			"message": err.Error(),
		}
	}

	if already {
		return map[string]interface{}{
			"status": "already_running",
			"pid":    pid,
		}
	}

	log.Printf("Tunnel started: PID=%d, singbox=%s, config=%s", pid, singboxPath, configPath)

	return map[string]interface{}{
		"status":       "started",
		"pid":          pid,
		"singbox_path": singboxPath,
		"config_path":  configPath,
	}
}

// launch builds the sing-box command for the current paths.
func (tm *TunnelManager) launch() (*exec.Cmd, func(), error) {
	tm.mu.Lock()
	singboxPath := tm.singboxPath
	configPath := tm.configPath
	tm.mu.Unlock()

	if killed, err := tm.pidFile.KillStale(singboxPath); err != nil {
		log.Printf("Warning: %v", err)
	} else if killed {
		log.Println("Killed sing-box left over from a previous service instance")
	}

	cmd := exec.Command(singboxPath, "run", "-c", configPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // new session / process group
	}
	cmd.Stdout = io.MultiWriter(os.Stdout, tm.logWriter)
	cmd.Stderr = io.MultiWriter(os.Stderr, tm.logWriter)

	return cmd, nil, nil
}

// onStarted runs after every (re)start of sing-box.
func (tm *TunnelManager) onStarted(p *os.Process) {
	if err := tm.pidFile.Write(p.Pid); err != nil {
		log.Printf("Warning: failed to write pid file: %v", err)
	}
}

// Stop stops the tunnel.
func (tm *TunnelManager) Stop() map[string]interface{} {
	if !tm.supervisor.Stop() {
		return map[string]interface{}{"status": "not_running"}
	}

	log.Println("Tunnel stopped")
	return map[string]interface{}{"status": "stopped"}
}

// IsActive reports whether the tunnel is running or about to be restarted.
func (tm *TunnelManager) IsActive() bool {
	return tm.supervisor.IsActive()
}

// GetStatus returns tunnel status.
func (tm *TunnelManager) GetStatus() map[string]interface{} {
	pid, ok := tm.supervisor.RunningPID()
	if !ok {
		return map[string]interface{}{"status": "stopped"}
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

	return map[string]interface{}{
		"status":       "running",
		"pid":          pid,
		"singbox_path": tm.singboxPath,
		"config_path":  tm.configPath,
		"restarts":     tm.supervisor.Restarts(),
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
	server := shared.NewServer(handler)

	go shared.ServeWithRetry(server, func(_ bool, msg string) { log.Println(msg) })

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
