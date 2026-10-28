package shared

import "time"

// VERSION is reported by -version and /ping. CI overrides it from the VERSION file via
// -ldflags "-X github.com/xanderwp/proxybridgeservice/src/shared.VERSION=...".
var VERSION = "1.4.0"

// Configuration constants
const (
	SERVICE_NAME       = "rocketman-tunnel"
	HTTP_PORT          = 5020
	APP_PING_URL       = "http://localhost:8081/ping"
	APP_CHECK_INTERVAL = 2 * time.Second

	// Log store settings shared across all platforms.
	LOG_RETENTION = 2 * time.Hour
	LOG_MAX_ITEMS = 10000
	LOG_TITLE     = "RocketMan Service Logs"
)
