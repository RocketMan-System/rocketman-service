//go:build windows

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// Configuration
const (
	SERVICE_NAME         = "RocketMan_Tun_Service"
	SERVICE_DISPLAY_NAME = "RocketMan Tunnel Service"
	SERVICE_DESCRIPTION  = "Manages RocketMan tunnel via HTTP API"
	HTTP_PORT            = 5020
	APP_PING_URL         = "http://localhost:8081/ping"
	APP_CHECK_INTERVAL   = 2 * time.Second
)

var elog *eventlog.Log

func logInfo(msg string) {
	log.Println(msg)
	if elog != nil {
		elog.Info(1, msg)
	}
}

func logError(msg string) {
	log.Println("ERROR:", msg)
	if elog != nil {
		elog.Error(1, msg)
	}
}

// TunnelManager manages the sing-box process
type TunnelManager struct {
	process     *os.Process
	mu          sync.Mutex
	singboxPath string
	configPath  string
	isRunning   bool
	pgid        uint32
}

// NewTunnelManager creates a new tunnel manager
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{}
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

	basePath := filepath.Join("C:\\Users", username, "AppData", "Roaming", appname, ".sing-box")
	tm.singboxPath = filepath.Join(basePath, "sing-box.exe")
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
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}

	if err := cmd.Start(); err != nil {
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

// checkAliveNoLock checks process liveness without acquiring the mutex
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

// Stop stops the tunnel
func (tm *TunnelManager) Stop() map[string]interface{} {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if !tm.isRunning {
		return map[string]interface{}{"status": "not_running"}
	}

	// Send CTRL_BREAK_EVENT for graceful shutdown
	dll := syscall.NewLazyDLL("kernel32.dll")
	proc := dll.NewProc("GenerateConsoleCtrlEvent")
	proc.Call(uintptr(1), uintptr(tm.pgid)) // 1 = CTRL_BREAK_EVENT

	done := make(chan error, 1)
	go func() {
		_, err := tm.process.Wait()
		done <- err
	}()

	select {
	case <-done:
		// Process exited gracefully
	case <-time.After(5 * time.Second):
		log.Println("Process didn't exit gracefully, killing")
		tm.process.Kill()
		tm.process.Wait()
	}

	tm.process = nil
	tm.isRunning = false
	log.Println("Tunnel stopped")

	return map[string]interface{}{"status": "stopped"}
}

// IsRunning checks if tunnel is running
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

	return map[string]interface{}{"status": "stopped"}
}

// AppMonitor monitors the main application
type AppMonitor struct {
	tunnelManager       *TunnelManager
	pingURL             string
	checkInterval       time.Duration
	maxFailures         int
	consecutiveFailures int
	stopChan            chan struct{}
	wg                  sync.WaitGroup
}

// NewAppMonitor creates a new app monitor
func NewAppMonitor(tm *TunnelManager, pingURL string, checkInterval time.Duration) *AppMonitor {
	return &AppMonitor{
		tunnelManager: tm,
		pingURL:       pingURL,
		checkInterval: checkInterval,
		maxFailures:   3,
		stopChan:      make(chan struct{}),
	}
}

// Start starts monitoring
func (am *AppMonitor) Start() {
	am.wg.Add(1)
	go am.monitorLoop()
	logInfo("App monitor started")
}

// Stop stops monitoring
func (am *AppMonitor) Stop() {
	close(am.stopChan)
	am.wg.Wait()
	logInfo("App monitor stopped")
}

// checkAppAlive checks if main app is responding
func (am *AppMonitor) checkAppAlive() bool {
	client := &http.Client{Timeout: 2 * time.Second}

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
						logInfo("Main app reconnected")
					}
					am.consecutiveFailures = 0
				} else {
					am.consecutiveFailures++

					if am.consecutiveFailures >= am.maxFailures {
						logError(fmt.Sprintf("Main app not responding (%d checks), stopping tunnel",
							am.consecutiveFailures))

						result := am.tunnelManager.Stop()
						logInfo(fmt.Sprintf("Tunnel stopped due to app disconnection: %v", result))

						am.consecutiveFailures = 0
					}
				}
			}
		}
	}
}

// HTTPHandler handles HTTP control requests
type HTTPHandler struct {
	tunnelManager *TunnelManager
}

// ServeHTTP handles incoming HTTP requests
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch r.URL.Path {
	case "/start":
		username := r.URL.Query().Get("username")
		appname := r.URL.Query().Get("appname")

		if username == "" || appname == "" {
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error": "Missing required parameters: username, appname",
			})
			return
		}

		result := h.tunnelManager.Start(username, appname)
		respondJSON(w, http.StatusOK, result)

	case "/stop":
		result := h.tunnelManager.Stop()
		respondJSON(w, http.StatusOK, result)

	case "/status":
		result := h.tunnelManager.GetStatus()
		respondJSON(w, http.StatusOK, result)

	case "/ping":
		respondJSON(w, http.StatusOK, map[string]interface{}{"status": "ok"})

	default:
		respondJSON(w, http.StatusNotFound, map[string]interface{}{"error": "Not found"})
	}
}

// respondJSON sends a JSON response
func respondJSON(w http.ResponseWriter, code int, data interface{}) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

// rmService implements svc.Handler
type rmService struct {
	tunnelManager *TunnelManager
	appMonitor    *AppMonitor
	httpServer    *http.Server
}

func (s *rmService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}

	handler := &HTTPHandler{tunnelManager: s.tunnelManager}
	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", HTTP_PORT),
		Handler: handler,
	}

	go func() {
		logInfo(fmt.Sprintf("HTTP server listening on port %d", HTTP_PORT))
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logError(fmt.Sprintf("HTTP server error: %v", err))
		}
	}()

	s.appMonitor.Start()

	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	logInfo(fmt.Sprintf("Service started successfully on port %d", HTTP_PORT))

loop:
	for {
		c := <-r
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			logInfo("Stop signal received")
			break loop
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

	tunnelManager := NewTunnelManager()
	appMonitor := NewAppMonitor(tunnelManager, APP_PING_URL, APP_CHECK_INTERVAL)

	s := &rmService{
		tunnelManager: tunnelManager,
		appMonitor:    appMonitor,
	}

	if err = svc.Run(name, s); err != nil {
		logError(fmt.Sprintf("Service %s failed: %v", name, err))
	}
}

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

func main() {
	versionFlag := flag.Bool("version", false, "Print version and exit")
	installFlag := flag.Bool("install", false, "Install the service")
	removeFlag := flag.Bool("remove", false, "Remove the service")
	startFlag := flag.Bool("start", false, "Start the service")
	stopFlag := flag.Bool("stop", false, "Stop the service")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if *versionFlag {
		fmt.Println("RocketMan Tunnel Service v1.0.0")
		os.Exit(0)
	}

	if *installFlag {
		if err := installService(SERVICE_NAME, SERVICE_DISPLAY_NAME, SERVICE_DESCRIPTION); err != nil {
			log.Fatalf("Install failed: %v", err)
		}
		fmt.Println("Service installed successfully")
		return
	}

	if *removeFlag {
		if err := removeService(SERVICE_NAME); err != nil {
			log.Fatalf("Remove failed: %v", err)
		}
		fmt.Println("Service removed successfully")
		return
	}

	if *startFlag {
		if err := startService(SERVICE_NAME); err != nil {
			log.Fatalf("Start failed: %v", err)
		}
		fmt.Println("Service started")
		return
	}

	if *stopFlag {
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

	// Interactive mode for debugging
	log.Println("Running interactively (not as Windows service)")
	log.Println("RocketMan Tunnel Service starting...")

	tunnelManager := NewTunnelManager()
	appMonitor := NewAppMonitor(tunnelManager, APP_PING_URL, APP_CHECK_INTERVAL)
	appMonitor.Start()

	handler := &HTTPHandler{tunnelManager: tunnelManager}
	server := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", HTTP_PORT),
		Handler: handler,
	}

	go func() {
		log.Printf("HTTP server listening on port %d", HTTP_PORT)
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
