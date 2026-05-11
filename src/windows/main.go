//go:build windows

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html"
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

const LOG_RETENTION = 2 * time.Hour

type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Source  string    `json:"source"` // "main" or "sing-box"
	Message string    `json:"message"`
}

type RecentLogStore struct {
	mu        sync.Mutex
	retention time.Duration
	maxItems  int
	entries   []LogEntry
}

func NewRecentLogStore(retention time.Duration, maxItems int) *RecentLogStore {
	return &RecentLogStore{
		retention: retention,
		maxItems:  maxItems,
		entries:   make([]LogEntry, 0, 256),
	}
}

func (s *RecentLogStore) Add(level, source, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.entries = append(s.entries, LogEntry{
		Time:    now,
		Level:   level,
		Source:  source,
		Message: strings.TrimSpace(msg),
	})

	s.pruneNoLock(now)

	if len(s.entries) > s.maxItems {
		overflow := len(s.entries) - s.maxItems
		s.entries = append([]LogEntry(nil), s.entries[overflow:]...)
	}
}

func (s *RecentLogStore) Last(duration time.Duration, sourceFilter string) []LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.pruneNoLock(now)

	if len(s.entries) == 0 {
		return nil
	}

	threshold := now.Add(-duration)
	var result []LogEntry
	// Loop backwards to get newest items first
	for i := len(s.entries) - 1; i >= 0; i-- {
		e := s.entries[i]
		if e.Time.After(threshold) {
			if sourceFilter == "" || sourceFilter == "all" || e.Source == sourceFilter {
				result = append(result, e)
			}
		}
	}
	return result
}

func (s *RecentLogStore) pruneNoLock(now time.Time) {
	if len(s.entries) == 0 {
		return
	}

	threshold := now.Add(-s.retention)
	cut := 0
	for cut < len(s.entries) && s.entries[cut].Time.Before(threshold) {
		cut++
	}

	if cut > 0 {
		s.entries = append([]LogEntry(nil), s.entries[cut:]...)
	}
}

type logMirrorWriter struct {
	store *RecentLogStore
}

func detectLogLevel(msg string) string {
	upper := strings.ToUpper(msg)
	if strings.Contains(upper, "ERROR") || strings.Contains(upper, "FATAL") || strings.Contains(upper, "PANIC") || strings.Contains(upper, "FAILED") {
		return "ERROR"
	}
	if strings.Contains(upper, "WARN") {
		return "WARN"
	}
	return "INFO"
}

func (w *logMirrorWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		w.store.Add(detectLogLevel(msg), "main", msg)
	}

	return len(p), nil
}

type singboxLogWriter struct {
	store *RecentLogStore
}

func (w *singboxLogWriter) Write(p []byte) (n int, err error) {
	text := string(p)
	for _, line := range strings.Split(text, "\n") {
		msg := strings.TrimSpace(line)
		if msg == "" {
			continue
		}
		w.store.Add(detectLogLevel(msg), "sing-box", msg)
	}
	return len(p), nil
}

var recentLogs = NewRecentLogStore(LOG_RETENTION, 10000)

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

// TunnelManager manages the sing-box process
type TunnelManager struct {
	process     *os.Process
	mu          sync.Mutex
	singboxPath string
	configPath  string
	logPath     string
	logFile     *os.File
	logWriter   *singboxLogWriter
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

	logInfo(fmt.Sprintf("Tunnel start requested: username=%s appname=%s", username, appname))

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
	tm.logWriter = &singboxLogWriter{store: recentLogs}

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

	return map[string]interface{}{
		"status":       "started",
		"pid":          tm.process.Pid,
		"singbox_path": tm.singboxPath,
		"config_path":  tm.configPath,
		"log_path":     tm.logPath,
	}
}

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
		upper := strings.ToUpper(item)
		if strings.HasPrefix(upper, prefix) {
			env[i] = key + "=" + value
			return env
		}
	}

	return append(env, key+"="+value)
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

	logInfo("Tunnel stop requested")

	if !tm.isRunning {
		logInfo("Tunnel is not running")
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
		logError("Process didn't exit gracefully, killing")
		tm.process.Kill()
		tm.process.Wait()
	}

	if tm.logFile != nil {
		_ = tm.logFile.Close()
		tm.logFile = nil
	}
	tm.logWriter = nil
	tm.process = nil
	tm.isRunning = false
	logInfo("Tunnel stopped")

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
			"log_path":     tm.logPath,
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

func renderLogsHTML(entries []LogEntry, currentSource string) string {
	var b strings.Builder

	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><title>RocketMan Logs</title>")
	b.WriteString("<style>")
	b.WriteString("body{font-family:Segoe UI,Arial,sans-serif;background:#f5f7fb;color:#1f2937;margin:0;padding:24px;}")
	b.WriteString(".wrap{max-width:1200px;margin:0 auto;background:#fff;border-radius:12px;box-shadow:0 8px 24px rgba(0,0,0,.08);overflow:hidden;}")
	b.WriteString(".head{padding:16px 20px;border-bottom:1px solid #e5e7eb;display:flex;justify-content:space-between;align-items:center;}")
	b.WriteString("h1{font-size:20px;margin:0;} .meta{font-size:13px;color:#6b7280;} .tools{display:flex;gap:15px;align-items:center;}")
	b.WriteString(".tools a{color:#2563eb;text-decoration:none;font-size:13px;}")
	b.WriteString("select{padding:4px 8px;border-radius:4px;border:1px solid #d1d5db;font-size:13px;outline:none;cursor:pointer;}")
	b.WriteString("table{width:100%;border-collapse:collapse;} th,td{padding:10px 12px;border-bottom:1px solid #f1f5f9;vertical-align:top;font-size:13px;}")
	b.WriteString("th{background:#f8fafc;text-align:left;color:#334155;font-weight:600;position:sticky;top:0;}")
	b.WriteString(".lvl{display:inline-block;padding:2px 8px;border-radius:999px;font-weight:600;font-size:11px;}")
	b.WriteString(".lvl-info{background:#dbeafe;color:#1d4ed8;} .lvl-error{background:#fee2e2;color:#b91c1c;} .lvl-warn{background:#ffedd5;color:#9a3412;}")
	b.WriteString(".src{font-size:11px;color:#6b7280;text-transform:uppercase;font-weight:bold;}")
	b.WriteString(".msg{white-space:pre-wrap;word-break:break-word;font-family:Consolas,monospace;}")
	b.WriteString("</style></head><body>")
	b.WriteString("<div class=\"wrap\"><div class=\"head\"><div><h1>RocketMan Service Logs</h1>")
	b.WriteString(fmt.Sprintf("<div class=\"meta\">Last 2 hours • Entries: %d • Updated: %s</div>",
		len(entries), html.EscapeString(time.Now().Format("2006-01-02 15:04:05"))))
	b.WriteString("</div><div class=\"tools\">")

	// Source Selector
	b.WriteString("<select onchange=\"window.location.href='/logs?source='+this.value\">")
	sources := []struct{ val, label string }{{"all", "All Sources"}, {"main", "Main Service"}, {"sing-box", "Sing-box"}}
	for _, s := range sources {
		selected := ""
		if currentSource == s.val {
			selected = " selected"
		}
		b.WriteString(fmt.Sprintf("<option value=\"%s\"%s>%s</option>", s.val, selected, s.label))
	}
	b.WriteString("</select>")

	refreshURL := "/logs"
	if currentSource != "all" && currentSource != "" {
		refreshURL = "/logs?source=" + currentSource
	}
	jsonURL := "/logs?format=json"
	if currentSource != "all" && currentSource != "" {
		jsonURL += "&source=" + currentSource
	}

	b.WriteString(fmt.Sprintf("<a href=\"%s\">JSON</a> • <a href=\"%s\">Refresh</a></div></div>", jsonURL, refreshURL))
	b.WriteString("<table><thead><tr><th style=\"width:150px\">Time</th><th style=\"width:80px\">Source</th><th style=\"width:80px\">Level</th><th>Message</th></tr></thead><tbody>")

	for _, entry := range entries {
		lvlClass := "lvl-info"
		if strings.EqualFold(entry.Level, "ERROR") {
			lvlClass = "lvl-error"
		} else if strings.EqualFold(entry.Level, "WARN") {
			lvlClass = "lvl-warn"
		}

		b.WriteString("<tr>")
		b.WriteString("<td>" + html.EscapeString(entry.Time.Format("15:04:05.000")) + "</td>")
		b.WriteString("<td><span class=\"src\">" + html.EscapeString(entry.Source) + "</span></td>")
		b.WriteString("<td><span class=\"lvl " + lvlClass + "\">" + html.EscapeString(strings.ToUpper(entry.Level)) + "</span></td>")
		b.WriteString("<td class=\"msg\">" + html.EscapeString(entry.Message) + "</td>")
		b.WriteString("</tr>")
	}

	b.WriteString("</tbody></table></div>")
	b.WriteString("<script>")
	b.WriteString("const params = new URLSearchParams(window.location.search);")
	b.WriteString("const source = params.get('source') || 'all';")
	b.WriteString("setTimeout(function(){ if(source === 'all' || source === 'main' || source === 'sing-box') window.location.reload(); }, 5000);")
	b.WriteString("</script>")
	b.WriteString("</body></html>")
	return b.String()
}

// ServeHTTP handles incoming HTTP requests
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch r.URL.Path {
	case "/logs":
		source := r.URL.Query().Get("source")
		if source == "" {
			source = "all"
		}
		entries := recentLogs.Last(LOG_RETENTION, source)
		if strings.EqualFold(r.URL.Query().Get("format"), "json") {
			respondJSON(w, http.StatusOK, map[string]interface{}{
				"status":      "ok",
				"source":      source,
				"retention":   LOG_RETENTION.String(),
				"entry_count": len(entries),
				"entries":     entries,
			})
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(renderLogsHTML(entries, source)))
		return

	case "/start":
		username := r.URL.Query().Get("username")
		appname := r.URL.Query().Get("appname")
		logInfo(fmt.Sprintf("HTTP /start called: username=%s appname=%s", username, appname))

		if username == "" || appname == "" {
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error": "Missing required parameters: username, appname",
			})
			return
		}

		result := h.tunnelManager.Start(username, appname)
		respondJSON(w, http.StatusOK, result)

	case "/stop":
		logInfo("HTTP /stop called")
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

	logInfo("Windows service process starting")

	tunnelManager := NewTunnelManager()
	appMonitor := NewAppMonitor(tunnelManager, APP_PING_URL, APP_CHECK_INTERVAL)

	s := &rmService{
		tunnelManager: tunnelManager,
		appMonitor:    appMonitor,
	}

	if err = svc.Run(name, s); err != nil {
		logError(fmt.Sprintf("Service %s failed: %v", name, err))
	}

	logInfo("Windows service process stopped")
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
	// Do NOT set log.SetOutput to MultiWriter with logMirrorWriter here,
	// because logInfo/logError already call recentLogs.Add manually to avoid double logging.
	
	logInfo("Logger initialized with 2-hour retention")

	if *versionFlag {
		fmt.Println("RocketMan Tunnel Service v1.0.0")
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
