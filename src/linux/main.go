package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xanderwp/proxybridgeservice/src/shared"
)

// recentLogs is the process-wide log store (shared with the HTTP /logs endpoint).
var recentLogs = shared.NewRecentLogStore(shared.LOG_RETENTION, shared.LOG_MAX_ITEMS)

// ---------------------------------------------------------------------------
// TunnelManager — Linux-specific (firewalld + unix signals)
// ---------------------------------------------------------------------------

// PID_FILE lets a restarted service kill a sing-box orphaned by a crashed predecessor.
const PID_FILE = "/run/rocketman-tunnel-singbox.pid"

// GRACEFUL_STOP_TIMEOUT keeps /stop well inside the app's 5s HTTP timeout.
const GRACEFUL_STOP_TIMEOUT = 2500 * time.Millisecond

// TunnelManager manages the sing-box process on Linux.
type TunnelManager struct {
	mu           sync.Mutex
	singboxPath  string
	configPath   string
	tunIfName    string
	lastUsername string
	lastAppname  string
	logWriter    *shared.SingboxLogWriter
	pidFile      shared.PidFile
	supervisor   *shared.Supervisor
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
// The service runs as root, so the user's home is looked up explicitly; the
// first candidate that actually contains sing-box wins.
func resolveBasePath(username, appname string) string {
	var configHomes []string

	if username != "" {
		if u, err := user.Lookup(username); err == nil && u.HomeDir != "" {
			configHomes = append(configHomes, filepath.Join(u.HomeDir, ".config"))
		}
		configHomes = append(configHomes, filepath.Join("/home", username, ".config"))
	} else if homeDir, err := os.UserHomeDir(); err == nil {
		configHomes = append(configHomes, filepath.Join(homeDir, ".config"))
	} else {
		configHomes = append(configHomes, "/root/.config")
	}

	// Kept for compatibility with older releases that honoured the service's XDG_CONFIG_HOME.
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		configHomes = append([]string{xdg}, configHomes...)
	}

	for _, configHome := range configHomes {
		basePath := filepath.Join(configHome, appname, ".sing-box")
		if _, err := os.Stat(filepath.Join(basePath, "sing-box")); err == nil {
			return basePath
		}
	}

	return filepath.Join(configHomes[0], appname, ".sing-box")
}

// Start starts the tunnel with given parameters.
func (tm *TunnelManager) Start(username, appname string) map[string]interface{} {
	if pid, ok := tm.supervisor.RunningPID(); ok {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		return map[string]interface{}{
			"status":        "already_running",
			"pid":           pid,
			"tun_interface": tm.tunIfName,
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

	tunIfName, err := getTunInterfaceNameFromConfig(configPath)
	if err != nil {
		log.Printf("Warning: failed to detect tun interface from config: %v", err)
		tunIfName = ""
	}

	tm.mu.Lock()
	tm.singboxPath = singboxPath
	tm.configPath = configPath
	tm.tunIfName = tunIfName
	tm.lastUsername = username
	tm.lastAppname = appname
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
			"status":        "already_running",
			"pid":           pid,
			"tun_interface": tunIfName,
		}
	}

	log.Printf("Tunnel started: PID=%d, singbox=%s, config=%s", pid, singboxPath, configPath)

	return map[string]interface{}{
		"status":        "started",
		"pid":           pid,
		"singbox_path":  singboxPath,
		"config_path":   configPath,
		"tun_interface": tunIfName,
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
		Setpgid: true, // new process group
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

	tm.mu.Lock()
	ifName := tm.tunIfName
	tm.mu.Unlock()

	if ifName == "" {
		return
	}

	go func() {
		added, err := ensureInterfaceInTrustedZone(ifName, 30*time.Second)
		if err != nil {
			log.Printf("Warning: failed to add interface %s to trusted zone: %v", ifName, err)
		} else if added {
			log.Printf("Added interface %s to firewalld trusted zone", ifName)
		} else {
			log.Printf("Skipped firewalld trusted-zone binding for interface %s", ifName)
		}
	}()
}

// Stop stops the tunnel.
func (tm *TunnelManager) Stop() map[string]interface{} {
	wasRunning := tm.supervisor.Stop()

	tm.mu.Lock()
	tunIfName := tm.tunIfName
	tm.tunIfName = ""
	tm.mu.Unlock()

	if tunIfName != "" {
		if err := removeInterfaceFromTrustedZone(tunIfName); err != nil {
			log.Printf("Warning: failed to remove interface %s from trusted zone: %v", tunIfName, err)
		} else {
			log.Printf("Removed interface %s from firewalld trusted zone", tunIfName)
		}
	}

	if !wasRunning {
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
		"status":        "running",
		"pid":           pid,
		"singbox_path":  tm.singboxPath,
		"config_path":   tm.configPath,
		"tun_interface": tm.tunIfName,
		"restarts":      tm.supervisor.Restarts(),
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
// Linux-specific: tun interface config parsing and firewalld integration
// ---------------------------------------------------------------------------

func getTunInterfaceNameFromConfig(configPath string) (string, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("read config: %w", err)
	}

	var cfg struct {
		Inbounds []struct {
			Type          string `json:"type"`
			InterfaceName string `json:"interface_name"`
		} `json:"inbounds"`
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse config: %w", err)
	}

	for _, inbound := range cfg.Inbounds {
		if inbound.Type == "tun" && inbound.InterfaceName != "" {
			return inbound.InterfaceName, nil
		}
	}

	return "", fmt.Errorf("tun inbound with interface_name not found")
}

func waitForInterface(iface string, timeout time.Duration) error {
	if iface == "" {
		return fmt.Errorf("empty interface name")
	}

	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(filepath.Join("/sys/class/net", iface)); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("interface %s did not appear within %s", iface, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// FIREWALL_CMD_TIMEOUT stops a hung firewalld D-Bus call from blocking start/stop.
const FIREWALL_CMD_TIMEOUT = 3 * time.Second

// TRUSTED_ZONE_SETTLE is how long the binding is re-checked after it is first
// applied, so a NetworkManager (re)activation of the tun profile cannot quietly
// move the interface back to its own zone.
const TRUSTED_ZONE_SETTLE = 10 * time.Second

var (
	errFirewallNotRunning = errors.New("firewalld not running")
	errUnknownInterface   = errors.New("unknown interface")
	errZoneConflict       = errors.New("zone conflict")
	errNoZone             = errors.New("no zone")
)

// bindInterfaceToTrustedZone uses --change-interface rather than --add-interface:
// NetworkManager binds tun devices it manages to a zone of its own (usually
// public), and --add-interface then fails with ZONE_CONFLICT. In that zone the
// TCP connections the system stack loops back through the interface are rejected.
func bindInterfaceToTrustedZone(iface string) error {
	return runFirewallCmd("--zone=trusted", "--change-interface="+iface)
}

func removeInterfaceFromTrustedZone(iface string) error {
	err := runFirewallCmd("--zone=trusted", "--remove-interface="+iface)
	if err == nil || errors.Is(err, errFirewallNotRunning) || errors.Is(err, errUnknownInterface) || errors.Is(err, errZoneConflict) {
		return nil
	}
	return err
}

func getZoneOfInterface(iface string) (string, error) {
	zone, err := runFirewallCmdOutput("--get-zone-of-interface=" + iface)
	if errors.Is(err, errNoZone) {
		return "", nil
	}
	return zone, err
}

func ensureInterfaceInTrustedZone(iface string, timeout time.Duration) (bool, error) {
	if err := waitForInterface(iface, timeout); err != nil {
		return false, err
	}

	bound := false
	settleUntil := time.Now().Add(TRUSTED_ZONE_SETTLE)
	for {
		// The tunnel was stopped meanwhile — nothing left to bind.
		if _, err := os.Stat(filepath.Join("/sys/class/net", iface)); err != nil {
			return bound, nil
		}

		zone, err := getZoneOfInterface(iface)
		if errors.Is(err, errFirewallNotRunning) {
			return false, nil
		}
		if err != nil {
			return bound, err
		}

		if zone != "trusted" {
			if err := bindInterfaceToTrustedZone(iface); err != nil {
				if errors.Is(err, errFirewallNotRunning) {
					return false, nil
				}
				return bound, err
			}
			if bound {
				log.Printf("Interface %s was moved to zone %q, re-bound to trusted", iface, zone)
			}
			bound = true
		}

		if time.Now().After(settleUntil) {
			return bound, nil
		}
		time.Sleep(1 * time.Second)
	}
}

func runFirewallCmd(args ...string) error {
	_, err := runFirewallCmdOutput(args...)
	return err
}

func runFirewallCmdOutput(args ...string) (string, error) {
	if _, err := exec.LookPath("firewall-cmd"); err != nil {
		return "", errFirewallNotRunning
	}

	ctx, cancel := context.WithTimeout(context.Background(), FIREWALL_CMD_TIMEOUT)
	defer cancel()

	cmd := exec.CommandContext(ctx, "firewall-cmd", args...)
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		if strings.Contains(trimmed, "FirewallD is not running") {
			return "", errFirewallNotRunning
		}
		if strings.Contains(trimmed, "UNKNOWN_INTERFACE") {
			return "", fmt.Errorf("%w: %s", errUnknownInterface, trimmed)
		}
		if strings.Contains(trimmed, "ZONE_CONFLICT") {
			return "", fmt.Errorf("%w: %s", errZoneConflict, trimmed)
		}
		if trimmed == "no zone" {
			return "", errNoZone
		}
		if trimmed == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, trimmed)
	}

	return trimmed, nil
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

	platformHandler := &PlatformHTTPHandler{tunnelManager: tunnelManager}
	handler := shared.NewHTTPHandlerWithLogs(platformHandler, recentLogs, shared.LOG_RETENTION, shared.LOG_TITLE)
	server := shared.NewServer(handler)

	go shared.ServeWithRetry(server, func(_ bool, msg string) { log.Println(msg) })

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutdown signal received, stopping service...")

	appMonitor.Stop()
	tunnelManager.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	log.Println("Service stopped")
}
