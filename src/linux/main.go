package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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

	"github.com/xanderwp/proxybridgeservice/src/shared"
)

// TunnelManager manages the sing-box process
type TunnelManager struct {
	process     *os.Process
	mu          sync.Mutex
	singboxPath string
	configPath  string
	tunIfName   string
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
			"status":        "already_running",
			"pid":           tm.process.Pid,
			"tun_interface": tm.tunIfName,
		}
	}

	// Resolve home directory by requested username to avoid using service user (root)
	homeDir := ""
	if username != "" {
		u, err := user.Lookup(username)
		if err == nil && u.HomeDir != "" {
			homeDir = u.HomeDir
		} else {
			homeDir = filepath.Join("/home", username)
		}
	} else {
		var err error
		homeDir, err = os.UserHomeDir()
		if err != nil {
			homeDir = "/root"
		}
	}

	// XDG_CONFIG_HOME or default ~/.config
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(homeDir, ".config")
	}

	basePath := filepath.Join(configHome, appname, ".sing-box")
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

	tunIfName, err := getTunInterfaceNameFromConfig(tm.configPath)
	if err != nil {
		log.Printf("Warning: failed to detect tun interface from config: %v", err)
		tunIfName = ""
	}
	tm.tunIfName = tunIfName

	// Start sing-box process
	cmd := exec.Command(tm.singboxPath, "run", "-c", tm.configPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // Create new process group
	}

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

	// Check if process is still alive
	if err := tm.process.Signal(syscall.Signal(0)); err != nil {
		tm.isRunning = false
		return map[string]interface{}{
			"status":  "error",
			"message": "Process exited immediately",
		}
	}

	log.Printf("Tunnel started: PID=%d, singbox=%s, config=%s",
		tm.process.Pid, tm.singboxPath, tm.configPath)

	if tm.tunIfName != "" {
		ifName := tm.tunIfName
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

	return map[string]interface{}{
		"status":        "started",
		"pid":           tm.process.Pid,
		"singbox_path":  tm.singboxPath,
		"config_path":   tm.configPath,
		"tun_interface": tm.tunIfName,
	}
}

// Stop stops the tunnel
func (tm *TunnelManager) Stop() map[string]interface{} {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if !tm.isRunning {
		if tm.tunIfName != "" {
			if err := removeInterfaceFromTrustedZone(tm.tunIfName); err != nil {
				log.Printf("Warning: failed to remove interface %s from trusted zone: %v", tm.tunIfName, err)
			} else {
				log.Printf("Removed interface %s from firewalld trusted zone", tm.tunIfName)
			}
			tm.tunIfName = ""
		}

		return map[string]interface{}{
			"status": "not_running",
		}
	}

	// Send SIGTERM first
	if err := tm.process.Signal(syscall.SIGTERM); err != nil {
		log.Printf("Error sending SIGTERM: %v", err)
	}

	// Wait for process to exit with timeout
	done := make(chan error, 1)
	go func() {
		_, err := tm.process.Wait()
		done <- err
	}()

	select {
	case <-done:
		// Process exited gracefully
	case <-time.After(5 * time.Second):
		// Timeout — force kill
		log.Println("Process didn't exit gracefully, sending SIGKILL")
		tm.process.Signal(syscall.SIGKILL)
		tm.process.Wait()
	}

	tm.process = nil
	tm.isRunning = false

	if tm.tunIfName != "" {
		if err := removeInterfaceFromTrustedZone(tm.tunIfName); err != nil {
			log.Printf("Warning: failed to remove interface %s from trusted zone: %v", tm.tunIfName, err)
		} else {
			log.Printf("Removed interface %s from firewalld trusted zone", tm.tunIfName)
		}
		tm.tunIfName = ""
	}

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
			"status":        "running",
			"pid":           tm.process.Pid,
			"singbox_path":  tm.singboxPath,
			"config_path":   tm.configPath,
			"tun_interface": tm.tunIfName,
		}
	}

	return map[string]interface{}{
		"status": "stopped",
	}
}

// HTTPHandler wraps TunnelManager to implement shared.ITunnelOperations
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

func addInterfaceToTrustedZone(iface string) error {
	return runFirewallCmd("--zone=trusted", "--add-interface="+iface)
}

func removeInterfaceFromTrustedZone(iface string) error {
	err := runFirewallCmd("--zone=trusted", "--remove-interface="+iface)
	if err == nil || errors.Is(err, errFirewallNotRunning) || errors.Is(err, errUnknownInterface) {
		return nil
	}
	return err
}

func ensureInterfaceInTrustedZone(iface string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		err := addInterfaceToTrustedZone(iface)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, errFirewallNotRunning) {
			return false, nil
		}
		if errors.Is(err, errUnknownInterface) {
			if time.Now().After(deadline) {
				return false, nil
			}
			time.Sleep(1 * time.Second)
			continue
		}
		return false, err
	}
}

var (
	errFirewallNotRunning = errors.New("firewalld not running")
	errUnknownInterface   = errors.New("unknown interface")
)

func runFirewallCmd(args ...string) error {
	if _, err := exec.LookPath("firewall-cmd"); err != nil {
		return errFirewallNotRunning
	}

	cmd := exec.Command("firewall-cmd", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		// exit 252: FirewallD is not running
		if strings.Contains(trimmed, "FirewallD is not running") {
			return errFirewallNotRunning
		}
		// exit 17: UNKNOWN_INTERFACE
		if strings.Contains(trimmed, "UNKNOWN_INTERFACE") {
			return fmt.Errorf("%w: %s", errUnknownInterface, trimmed)
		}
		if trimmed == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, trimmed)
	}

	return nil
}

func main() {
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

	// Wait for interrupt or termination signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	log.Println("Shutdown signal received, stopping service...")

	// Graceful shutdown
	appMonitor.Stop()
	tunnelManager.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	log.Println("Service stopped")
}
