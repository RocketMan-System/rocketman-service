package shared

import "time"

// Configuration constants
const (
	SERVICE_NAME       = "rocketman-tunnel"
	HTTP_PORT          = 5020
	APP_PING_URL       = "http://localhost:8081/ping"
	APP_CHECK_INTERVAL = 2 * time.Second
	VERSION            = "1.0.0"
)
