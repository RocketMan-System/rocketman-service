//go:build windows

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
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/xanderwp/proxybridgeservice/src/shared"
)

const (
	SERVICE_NAME         = "RocketMan_Tun_Service"
	SERVICE_DISPLAY_NAME = "RocketMan Tunnel Service"
	SERVICE_DESCRIPTION  = "Manages RocketMan tunnel via HTTP API"
)

var elog *eventlog.Log

// recentLogs is the process-wide log store (shared with the HTTP /logs endpoint).
var recentLogs = shared.NewRecentLogStore(shared.LOG_RETENTION, shared.LOG_MAX_ITEMS)

// ---------------------------------------------------------------------------
// Platform-specific logging (Windows Event Log integration)
// ---------------------------------------------------------------------------

func logInfo(msg string) {
	log.Println(msg)
	recentLogs.Add("INFO", "main", msg)
	if elog != nil {
		elog.Info(1, msg)
	}
}

func logError(msg string) {
	log.Println("ERROR:", msg)
	recentLogs.Add("ERROR", "main", msg)
	if elog != nil {
		elog.Error(1, msg)
	}
}

// ---------------------------------------------------------------------------
// TunnelManager — Windows-specific (Windows API for process lifecycle)
// ---------------------------------------------------------------------------

// TunnelManager manages the sing-box process on Windows.
type TunnelManager struct {
	process         *os.Process
	mu              sync.Mutex
	singboxPath     string
	configPath      string
	logPath         string
	logFile         *os.File
	logWriter       *shared.SingboxLogWriter
	isRunning       bool
	pgid            uint32
	restartAttempts int
	lastStartTime   time.Time
	stopMonitorChan chan struct{}
	monitorWg       sync.WaitGroup
	intentionalStop bool
	wantRunning     bool
	lastUsername    string
	lastAppname     string
}

// NewTunnelManager creates a new tunnel manager.
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		stopMonitorChan: make(chan struct{}),
	}
}

// Start starts the tunnel with given parameters.
func (tm *TunnelManager) Start(username, appname string) map[string]interface{} {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	logInfo(fmt.Sprintf("Tunnel start requested: username=%s appname=%s", username, appname))

	tm.lastUsername = username
	tm.lastAppname = appname
	tm.intentionalStop = false
	tm.wantRunning = true

	if tm.isRunning {
		logInfo("Tunnel is already running")
		return map[string]interface{}{
			"status": "already_running",
			"pid":    tm.process.Pid,
		}
	}

	basePath := filepath.Join("C:\\Users", username, "AppData", "Roaming", appname, ".sing-box")
	tm.singboxPath = filepath.Join(basePath, "sing-box.exe")
	tm.configPath = filepath.Join(basePath, "sing-box-auto.json")
	tm.logPath = filepath.Join(basePath, "sing-box-service.log")

	if _, err := os.Stat(tm.singboxPath); os.IsNotExist(err) {
		logError(fmt.Sprintf("sing-box not found: %s", tm.singboxPath))
		return map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("sing-box not found: %s", tm.singboxPath),
		}
	}

	if _, err := os.Stat(tm.configPath); os.IsNotExist(err) {
		logError(fmt.Sprintf("Config not found: %s", tm.configPath))
		return map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Config not found: %s", tm.configPath),
		}
	}

	cmd := exec.Command(tm.singboxPath, "run", "-c", tm.configPath)
	cmd.Dir = basePath
	cmd.Env = buildChildEnv(username)

	logFile, err := os.OpenFile(tm.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		logError(fmt.Sprintf("Failed to open log file: %v", err))
		return map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Failed to open log file: %v", err),
		}
	}
	tm.logFile = logFile
	tm.logWriter = &shared.SingboxLogWriter{Store: recentLogs}

	cmd.Stdout = io.MultiWriter(logFile, tm.logWriter)
	cmd.Stderr = io.MultiWriter(logFile, tm.logWriter)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		tm.logFile = nil
		tm.logWriter = nil
		logError(fmt.Sprintf("Failed to start process: %v", err))
		return map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Failed to start process: %v", err),
		}
	}

	tm.process = cmd.Process
	tm.pgid = uint32(tm.process.Pid)
	tm.isRunning = true

	time.Sleep(500 * time.Millisecond)

	if !tm.checkAliveNoLock() {
		tm.isRunning = false
		if tm.logFile != nil {
			_ = tm.logFile.Close()
			tm.logFile = nil
		}
		tm.logWriter = nil
		logError("Process exited immediately")
		return map[string]interface{}{
			"status":  "error",
			"message": "Process exited immediately",
		}
	}

	logInfo(fmt.Sprintf("Tunnel started: PID=%d, singbox=%s, config=%s",
		tm.process.Pid, tm.singboxPath, tm.configPath))

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
		"log_path":     tm.logPath,
	}
}

// checkAliveNoLock checks process liveness without acquiring the mutex.
// Uses Windows API (GetExitCodeProcess) instead of signals.
func (tm *TunnelManager) checkAliveNoLock() bool {
	if tm.process == nil {
		return false
	}

	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(tm.process.Pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}

	const STILL_ACTIVE = 259
	return exitCode == STILL_ACTIVE
}

// Stop stops the tunnel. Monitor goroutine is the only Wait() owner (avoids double-Wait races).
func (tm *TunnelManager) Stop() map[string]interface{} {
	return tm.stopInternal(true)
}

func (tm *TunnelManager) signalMonitorStopLocked() {
	select {
	case <-tm.stopMonitorChan:
		// already closed
	default:
		close(tm.stopMonitorChan)
	}
}

func (tm *TunnelManager) resetMonitorChanLocked() {
	select {
	case <-tm.stopMonitorChan:
		// drained closed channel
	default:
		close(tm.stopMonitorChan)
	}
	tm.stopMonitorChan = make(chan struct{})
}

func (tm *TunnelManager) stopInternal(clearWant bool) map[string]interface{} {
	tm.mu.Lock()

	logInfo("Tunnel stop requested")

	if clearWant {
		tm.wantRunning = false
	}
	tm.intentionalStop = true

	proc := tm.process
	pgid := tm.pgid
	wasRunning := tm.isRunning || proc != nil
	tm.signalMonitorStopLocked()
	tm.mu.Unlock()

	if proc != nil {
		dll := syscall.NewLazyDLL("kernel32.dll")
		genCtrl := dll.NewProc("GenerateConsoleCtrlEvent")
		genCtrl.Call(uintptr(1), uintptr(pgid)) // 1 = CTRL_BREAK_EVENT
	}

	done := make(chan struct{})
	go func() {
		tm.monitorWg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(shared.ProcessStopTimeout):
		logError("Process didn't exit gracefully, killing")
		if proc != nil {
			_ = proc.Kill()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			logError("Monitor did not finish after kill")
		}
	}

	tm.mu.Lock()
	if tm.logFile != nil {
		_ = tm.logFile.Close()
		tm.logFile = nil
	}
	tm.logWriter = nil
	tm.process = nil
	tm.isRunning = false
	tm.intentionalStop = false
	tm.resetMonitorChanLocked()
	tm.mu.Unlock()

	if !wasRunning {
		logInfo("Tunnel is not running")
		return map[string]interface{}{"status": "not_running"}
	}

	logInfo("Tunnel stopped")

	return map[string]interface{}{"status": "stopped"}
}

// RecoverAfterResume restarts the tunnel after sleep/hibernate if it was supposed to run.
func (tm *TunnelManager) RecoverAfterResume() {
	tm.mu.Lock()
	want := tm.wantRunning
	username := tm.lastUsername
	appname := tm.lastAppname
	tm.mu.Unlock()

	if !want || username == "" || appname == "" {
		return
	}

	logInfo("System resume detected — recovering tunnel")
	tm.stopInternal(false)
	tm.Start(username, appname)
}

// IsRunning checks if tunnel is running.
func (tm *TunnelManager) IsRunning() bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if !tm.isRunning {
		return false
	}

	alive := tm.checkAliveNoLock()
	if !alive {
		tm.isRunning = false
	}
	return alive
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
			"log_path":     tm.logPath,
		}
	}

	return map[string]interface{}{"status": "stopped"}
}

// monitor watches the tunnel process and handles restarts on crash.
// It is the sole owner of process.Wait() to avoid double-Wait races with Stop().
func (tm *TunnelManager) monitor() {
	defer tm.monitorWg.Done()

	for {
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

		_, err := process.Wait()

		tm.mu.Lock()
		intentional := tm.intentionalStop
		sameProcess := tm.process == process

		if intentional || !sameProcess {
			if sameProcess {
				if tm.logFile != nil {
					_ = tm.logFile.Close()
					tm.logFile = nil
				}
				tm.logWriter = nil
				tm.process = nil
				tm.isRunning = false
			}
			tm.mu.Unlock()
			return
		}

		logError(fmt.Sprintf("Tunnel process crashed: %v", err))

		delay, nextAttempts := shared.ComputeRestartDelay(tm.restartAttempts, tm.lastStartTime)
		tm.restartAttempts = nextAttempts

		if delay >= shared.RestartBackoff {
			logError(fmt.Sprintf("Quick restart budget exhausted, backing off for %v", delay))
		} else {
			logInfo(fmt.Sprintf("Attempting to restart tunnel (attempt %d/%d) in %v",
				tm.restartAttempts, shared.MaxQuickRestarts, delay))
		}

		if tm.logFile != nil {
			_ = tm.logFile.Close()
			tm.logFile = nil
		}
		tm.logWriter = nil
		tm.process = nil
		tm.isRunning = false
		username := tm.lastUsername
		appname := tm.lastAppname
		want := tm.wantRunning
		singboxPath := tm.singboxPath
		configPath := tm.configPath
		logPath := tm.logPath
		tm.mu.Unlock()

		if !want {
			return
		}

		select {
		case <-time.After(delay):
		case <-tm.stopMonitorChan:
			return
		}

		tm.mu.Lock()
		if tm.intentionalStop || tm.isRunning || !tm.wantRunning {
			tm.mu.Unlock()
			return
		}

		tm.lastStartTime = time.Now()

		logFile, openErr := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if openErr != nil {
			logError(fmt.Sprintf("Failed to open log file for restart: %v", openErr))
			tm.mu.Unlock()
			continue
		}
		tm.logFile = logFile
		tm.logWriter = &shared.SingboxLogWriter{Store: recentLogs}

		cmd := exec.Command(singboxPath, "run", "-c", configPath)
		cmd.Dir = filepath.Dir(configPath)
		cmd.Env = buildChildEnv(username)
		cmd.Stdout = io.MultiWriter(logFile, tm.logWriter)
		cmd.Stderr = io.MultiWriter(logFile, tm.logWriter)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
			HideWindow:    true,
		}

		if startErr := cmd.Start(); startErr != nil {
			logError(fmt.Sprintf("Failed to restart tunnel: %v", startErr))
			_ = logFile.Close()
			tm.logFile = nil
			tm.logWriter = nil
			tm.mu.Unlock()
			continue
		}

		tm.process = cmd.Process
		tm.pgid = uint32(tm.process.Pid)
		tm.isRunning = true
		logInfo(fmt.Sprintf("Tunnel restarted: PID=%d (app=%s)", tm.process.Pid, appname))
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
// Windows environment helpers
// ---------------------------------------------------------------------------

func buildChildEnv(username string) []string {
	env := append([]string{}, os.Environ()...)

	if username == "" {
		return env
	}

	profilePath := filepath.Join("C:\\Users", username)
	roamingAppData := filepath.Join(profilePath, "AppData", "Roaming")
	localAppData := filepath.Join(profilePath, "AppData", "Local")
	tempPath := filepath.Join(localAppData, "Temp")

	env = setEnvVar(env, "USERPROFILE", profilePath)
	env = setEnvVar(env, "HOME", profilePath)
	env = setEnvVar(env, "APPDATA", roamingAppData)
	env = setEnvVar(env, "LOCALAPPDATA", localAppData)
	env = setEnvVar(env, "TEMP", tempPath)
	env = setEnvVar(env, "TMP", tempPath)

	if u, err := user.Current(); err == nil && u != nil {
		env = setEnvVar(env, "ROCKETMAN_SERVICE_USER", u.Username)
	}

	return env
}

func setEnvVar(env []string, key, value string) []string {
	if key == "" {
		return env
	}

	prefix := strings.ToUpper(key) + "="
	for i, item := range env {
		if strings.HasPrefix(strings.ToUpper(item), prefix) {
			env[i] = key + "=" + value
			return env
		}
	}

	return append(env, key+"="+value)
}

// ---------------------------------------------------------------------------
// Windows Service implementation
// ---------------------------------------------------------------------------

type rmService struct {
	tunnelManager *TunnelManager
	appMonitor    *shared.AppMonitor
	httpServer    *http.Server
}

func (s *rmService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}

	platformHandler := &PlatformHTTPHandler{tunnelManager: s.tunnelManager}
	handler := shared.NewHTTPHandlerWithLogs(platformHandler, recentLogs, shared.LOG_RETENTION, shared.LOG_TITLE)
	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", shared.HTTP_PORT),
		Handler: handler,
	}

	go func() {
		logInfo(fmt.Sprintf("HTTP server listening on port %d", shared.HTTP_PORT))
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logError(fmt.Sprintf("HTTP server error: %v", err))
		}
	}()

	s.appMonitor.Start()

	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPowerEvent}
	logInfo(fmt.Sprintf("Service started successfully on port %d", shared.HTTP_PORT))

loop:
	for {
		c := <-r
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			logInfo("Stop signal received")
			break loop
		case svc.PowerEvent:
			// PBT_APMRESUMEAUTOMATIC = 0x12, PBT_APMRESUMESUSPEND = 0x07
			if c.EventType == 0x12 || c.EventType == 0x07 {
				go s.tunnelManager.RecoverAfterResume()
			}
		default:
			logError(fmt.Sprintf("Unexpected control request: %d", c.Cmd))
		}
	}

	changes <- svc.Status{State: svc.StopPending}

	s.appMonitor.Stop()
	s.tunnelManager.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.httpServer.Shutdown(ctx)

	logInfo("Service stopped")
	return false, 0
}

func runService(name string) {
	var err error
	elog, err = eventlog.Open(name)
	if err != nil {
		log.Printf("Cannot open event log: %v", err)
	}
	if elog != nil {
		defer elog.Close()
	}

	logInfo("Windows service process starting")

	tunnelManager := NewTunnelManager()
	appMonitor := shared.NewAppMonitor(tunnelManager, shared.APP_PING_URL, shared.APP_CHECK_INTERVAL)

	s := &rmService{
		tunnelManager: tunnelManager,
		appMonitor:    appMonitor,
	}

	if err = svc.Run(name, s); err != nil {
		logError(fmt.Sprintf("Service %s failed: %v", name, err))
	}

	logInfo("Windows service process stopped")
}

// ---------------------------------------------------------------------------
// Service install / remove / start / stop helpers
// ---------------------------------------------------------------------------

func installService(name, displayName, desc string) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", name)
	}

	s, err = m.CreateService(name, exePath, mgr.Config{
		DisplayName: displayName,
		Description: desc,
		StartType:   mgr.StartAutomatic,
	})
	if err != nil {
		return err
	}
	defer s.Close()

	if err = eventlog.InstallAsEventCreate(name, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		s.Delete()
		return fmt.Errorf("eventlog install failed: %v", err)
	}

	return nil
}

func removeService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("service %s not found", name)
	}
	defer s.Close()

	if err = s.Delete(); err != nil {
		return err
	}

	if err = eventlog.Remove(name); err != nil {
		return fmt.Errorf("eventlog remove failed: %v", err)
	}

	return nil
}

func startService(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("could not open service: %v", err)
	}
	defer s.Close()

	return s.Start()
}

func controlService(name string, c svc.Cmd, to svc.State) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("could not open service: %v", err)
	}
	defer s.Close()

	status, err := s.Control(c)
	if err != nil {
		return fmt.Errorf("could not send control: %v", err)
	}

	timeout := time.Now().Add(10 * time.Second)
	for status.State != to {
		if time.Now().After(timeout) {
			return fmt.Errorf("timeout waiting for service state=%d", to)
		}
		time.Sleep(300 * time.Millisecond)
		if status, err = s.Query(); err != nil {
			return fmt.Errorf("could not retrieve service status: %v", err)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	versionFlag := flag.Bool("version", false, "Print version and exit")
	installFlag := flag.Bool("install", false, "Install the service")
	removeFlag := flag.Bool("remove", false, "Remove the service")
	startFlag := flag.Bool("start", false, "Start the service")
	stopFlag := flag.Bool("stop", false, "Stop the service")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	logInfo("Logger initialized with 2-hour retention")

	if *versionFlag {
		fmt.Printf("RocketMan Tunnel Service v%s\n", shared.VERSION)
		os.Exit(0)
	}

	if *installFlag {
		logInfo("Service install requested")
		if err := installService(SERVICE_NAME, SERVICE_DISPLAY_NAME, SERVICE_DESCRIPTION); err != nil {
			log.Fatalf("Install failed: %v", err)
		}
		fmt.Println("Service installed successfully")
		return
	}

	if *removeFlag {
		logInfo("Service remove requested")
		if err := removeService(SERVICE_NAME); err != nil {
			log.Fatalf("Remove failed: %v", err)
		}
		fmt.Println("Service removed successfully")
		return
	}

	if *startFlag {
		logInfo("Service start requested")
		if err := startService(SERVICE_NAME); err != nil {
			log.Fatalf("Start failed: %v", err)
		}
		fmt.Println("Service started")
		return
	}

	if *stopFlag {
		logInfo("Service stop requested")
		if err := controlService(SERVICE_NAME, svc.Stop, svc.Stopped); err != nil {
			log.Fatalf("Stop failed: %v", err)
		}
		fmt.Println("Service stopped")
		return
	}

	isService, err := svc.IsWindowsService()
	if err != nil {
		log.Fatalf("Cannot determine if running as service: %v", err)
	}

	if isService {
		runService(SERVICE_NAME)
		return
	}

	// Interactive mode (debugging)
	log.Println("Running interactively (not as Windows service)")
	log.Println("RocketMan Tunnel Service starting...")

	tunnelManager := NewTunnelManager()
	appMonitor := shared.NewAppMonitor(tunnelManager, shared.APP_PING_URL, shared.APP_CHECK_INTERVAL)
	appMonitor.Start()

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
	signal.Notify(sigChan, os.Interrupt)
	<-sigChan

	log.Println("Shutdown signal received, stopping service...")

	appMonitor.Stop()
	tunnelManager.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Shutdown(ctx)

	log.Println("Service stopped")
}
