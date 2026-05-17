package shared

// TunnelStartResult represents the result of starting a tunnel
type TunnelStartResult struct {
	Status      string `json:"status"`
	PID         int    `json:"pid,omitempty"`
	Message     string `json:"message,omitempty"`
	SingboxPath string `json:"singbox_path,omitempty"`
	ConfigPath  string `json:"config_path,omitempty"`
}

// TunnelStopResult represents the result of stopping a tunnel
type TunnelStopResult struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// TunnelStatus represents the tunnel status
type TunnelStatus struct {
	Status      string `json:"status"`
	PID         int    `json:"pid,omitempty"`
	SingboxPath string `json:"singbox_path,omitempty"`
	ConfigPath  string `json:"config_path,omitempty"`
}
