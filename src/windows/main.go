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

// GRACEFUL_STOP_TIMEOUT is short because a service has no console, so CTRL_BREAK
// usually can't reach sing-box; waiting longer only pushes /stop past the app's 5s timeout.
const GRACEFUL_STOP_TIMEOUT = 1500 * time.Millisecond

// TunnelManager manages the sing-box process on Windows.
type TunnelManager struct {
	mu           sync.Mutex
	singboxPath  string
	configPath   string
	logPath      string
	profilePath  string
	lastUsername string
	lastAppname  string
	pidFile      shared.PidFile
	job          *shared.KillOnCloseJob
	supervisor   *shared.Supervisor
}

// NewTunnelManager creates a new tunnel manager.
func NewTunnelManager() *TunnelManager {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}

	tm := &TunnelManager{
		pidFile: shared.PidFile{Path: filepath.Join(programData, "RocketMan", "tun-singbox.pid")},
	}

	job, err := shared.NewKillOnCloseJob()
	if err != nil {
		logError(fmt.Sprintf("Failed to create job object, sing-box may outlive a crashed service: %v", err))
	}
	tm.job = job

	tm.supervisor = shared.NewSupervisor(shared.SupervisorHooks{
		Launch:          tm.launch,
		Interrupt:       sendCtrlBreak,
		Kill:            func(p *os.Process) { _ = p.Kill() },
		OnStarted:       tm.onStarted,
		OnExited:        tm.pidFile.Remove,
		GracefulTimeout: GRACEFUL_STOP_TIMEOUT,
		Log: func(isError bool, msg string) {
			if isError {
				logError(msg)
			} else {
				logInfo(msg)
			}
		},
	})
	return tm
}

// sendCtrlBreak asks the process group to exit gracefully.
func sendCtrlBreak(p *os.Process) {
	dll := syscall.NewLazyDLL("kernel32.dll")
	genCtrl := dll.NewProc("GenerateConsoleCtrlEvent")
	genCtrl.Call(uintptr(1), uintptr(p.Pid)) // 1 = CTRL_BREAK_EVENT, group id = pid (CREATE_NEW_PROCESS_GROUP)
}

// resolveProfilePath returns the user's profile directory that contains the app's sing-box.
// The profile folder name may differ from the username, so the registered profile is tried first.
func resolveProfilePath(username, appname string) string {
	var candidates []string

	if u, err := user.Lookup(username); err == nil && u.HomeDir != "" {
		candidates = append(candidates, u.HomeDir)
	}
	candidates = append(candidates, filepath.Join("C:\\Users", username))

	for _, profile := range candidates {
		exe := filepath.Join(profile, "AppData", "Roaming", appname, ".sing-box", "sing-box.exe")
		if _, err := os.Stat(exe); err == nil {
			return profile
		}
	}

	return filepath.Join("C:\\Users", username)
}

// Start starts the tunnel with given parameters.
func (tm *TunnelManager) Start(username, appname string) map[string]interface{} {
	logInfo(fmt.Sprintf("Tunnel start requested: username=%s appname=%s", username, appname))

	if pid, ok := tm.supervisor.RunningPID(); ok {
		logInfo("Tunnel is already running")
		return map[string]interface{}{
			"status": "already_running",
			"pid":    pid,
		}
	}

	profilePath := resolveProfilePath(username, appname)
	basePath := filepath.Join(profilePath, "AppData", "Roaming", appname, ".sing-box")
	singboxPath := filepath.Join(basePath, "sing-box.exe")
	configPath := filepath.Join(basePath, "sing-box-auto.json")
	logPath := filepath.Join(basePath, "sing-box-service.log")

	if _, err := os.Stat(singboxPath); os.IsNotExist(err) {
		logError(fmt.Sprintf("sing-box not found: %s", singboxPath))
		return map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("sing-box not found: %s", singboxPath),
		}
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		logError(fmt.Sprintf("Config not found: %s", configPath))
		return map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Config not found: %s", configPath),
		}
	}

	tm.mu.Lock()
	tm.singboxPath = singboxPath
	tm.configPath = configPath
	tm.logPath = logPath
	tm.profilePath = profilePath
	tm.lastUsername = username
	tm.lastAppname = appname
	tm.mu.Unlock()

	pid, already, err := tm.supervisor.Start()
	if err != nil {
		logError(fmt.Sprintf("Tunnel start failed: %v", err))
		return map[string]interface{}{
			"status":  "error",
			"message": err.Error(),
		}
	}

	if already {
		logInfo("Tunnel is already running")
		return map[string]interface{}{
			"status": "already_running",
			"pid":    pid,
		}
	}

	logInfo(fmt.Sprintf("Tunnel started: PID=%d, singbox=%s, config=%s", pid, singboxPath, configPath))

	return map[string]interface{}{
		"status":       "started",
		"pid":          pid,
		"singbox_path": singboxPath,
		"config_path":  configPath,
		"log_path":     logPath,
	}
}

// launch builds the sing-box command for the current paths. The log file is closed once the process is reaped.
func (tm *TunnelManager) launch() (*exec.Cmd, func(), error) {
	tm.mu.Lock()
	singboxPath := tm.singboxPath
	configPath := tm.configPath
	logPath := tm.logPath
	profilePath := tm.profilePath
	tm.mu.Unlock()

	if killed, err := tm.pidFile.KillStale(singboxPath); err != nil {
		logError(err.Error())
	} else if killed {
		logInfo("Killed sing-box left over from a previous service instance")
	}

	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open log file: %v", err)
	}
	logWriter := &shared.SingboxLogWriter{Store: recentLogs}

	cmd := exec.Command(singboxPath, "run", "-c", configPath)
	cmd.Dir = filepath.Dir(configPath)
	cmd.Env = buildChildEnv(profilePath)
	cmd.Stdout = io.MultiWriter(logFile, logWriter)
	cmd.Stderr = io.MultiWriter(logFile, logWriter)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}

	return cmd, func() { _ = logFile.Close() }, nil
}

// onStarted runs after every (re)start of sing-box.
func (tm *TunnelManager) onStarted(p *os.Process) {
	if err := tm.job.Assign(p.Pid); err != nil {
		logError(fmt.Sprintf("Failed to assign sing-box to job object: %v", err))
	}
	if err := tm.pidFile.Write(p.Pid); err != nil {
		logError(fmt.Sprintf("Failed to write pid file: %v", err))
	}
}

// Stop stops the tunnel.
func (tm *TunnelManager) Stop() map[string]interface{} {
	logInfo("Tunnel stop requested")

	if !tm.supervisor.Stop() {
		logInfo("Tunnel is not running")
		return map[string]interface{}{"status": "not_running"}
	}

	logInfo("Tunnel stopped")
	return map[string]interface{}{"status": "stopped"}
}

// RecoverAfterResume restarts the tunnel after sleep/hibernate if it was supposed to run.
func (tm *TunnelManager) RecoverAfterResume() {
	if !tm.supervisor.IsActive() {
		return
	}

	tm.mu.Lock()
	username := tm.lastUsername
	appname := tm.lastAppname
	tm.mu.Unlock()

	if username == "" || appname == "" {
		return
	}

	logInfo("System resume detected — recovering tunnel")
	tm.supervisor.Stop()
	tm.Start(username, appname)
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
		"log_path":     tm.logPath,
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
// Windows environment helpers
// ---------------------------------------------------------------------------

func buildChildEnv(profilePath string) []string {
	env := append([]string{}, os.Environ()...)

	if profilePath == "" {
		return env
	}

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
	s.httpServer = shared.NewServer(handler)

	go shared.ServeWithRetry(s.httpServer, func(isError bool, msg string) {
		if isError {
			logError(msg)
		} else {
			logInfo(msg)
		}
	})

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

	if err := ensureRecoveryActions(name); err != nil {
		logError(fmt.Sprintf("Failed to configure service recovery actions: %v", err))
	}

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

	if err = ensureRecoveryActions(name); err != nil {
		log.Printf("Warning: failed to configure recovery actions: %v", err)
	}

	if err = eventlog.InstallAsEventCreate(name, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		s.Delete()
		return fmt.Errorf("eventlog install failed: %v", err)
	}

	return nil
}

// ensureRecoveryActions makes SCM restart the service after a crash. ProxyBridge registers
// the service with plain "sc create", which leaves recovery disabled.
func ensureRecoveryActions(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return err
	}
	defer s.Close()

	return s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 3 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 3 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
	}, uint32((24 * time.Hour).Seconds()))
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
	server := shared.NewServer(handler)

	go shared.ServeWithRetry(server, func(_ bool, msg string) { log.Println(msg) })

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
