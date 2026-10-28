package shared

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ITunnelOperations defines tunnel control operations that each platform must implement.
type ITunnelOperations interface {
	Start(username, appname string) map[string]interface{}
	Stop() map[string]interface{}
	GetStatus() map[string]interface{}
}

// HTTPHandler handles HTTP control requests for all platforms.
// Optionally exposes a /logs endpoint when a RecentLogStore is provided.
type HTTPHandler struct {
	tunnelOps    ITunnelOperations
	logStore     *RecentLogStore
	logRetention time.Duration
	logTitle     string
}

// NewHTTPHandler creates a handler without log-viewing support.
func NewHTTPHandler(tunnelOps ITunnelOperations) *HTTPHandler {
	return &HTTPHandler{tunnelOps: tunnelOps}
}

// NewHTTPHandlerWithLogs creates a handler that also serves /logs (HTML and JSON).
// serviceTitle is shown as the page heading (e.g. "RocketMan Service Logs").
func NewHTTPHandlerWithLogs(tunnelOps ITunnelOperations, store *RecentLogStore, retention time.Duration, serviceTitle string) *HTTPHandler {
	return &HTTPHandler{
		tunnelOps:    tunnelOps,
		logStore:     store,
		logRetention: retention,
		logTitle:     serviceTitle,
	}
}

// ServeHTTP handles incoming HTTP requests.
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch r.URL.Path {
	case "/logs":
		h.handleLogs(w, r)

	case "/start":
		username := r.URL.Query().Get("username")
		appname := r.URL.Query().Get("appname")

		if username == "" || appname == "" {
			RespondJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error": "Missing required parameters: username, appname",
			})
			return
		}

		result := h.tunnelOps.Start(username, appname)
		RespondJSON(w, http.StatusOK, result)

	case "/stop":
		result := h.tunnelOps.Stop()
		RespondJSON(w, http.StatusOK, result)

	case "/status":
		result := h.tunnelOps.GetStatus()
		RespondJSON(w, http.StatusOK, result)

	case "/ping":
		RespondJSON(w, http.StatusOK, map[string]interface{}{"status": "ok", "version": VERSION})

	default:
		RespondJSON(w, http.StatusNotFound, map[string]interface{}{"error": "Endpoint not found"})
	}
}

func (h *HTTPHandler) handleLogs(w http.ResponseWriter, r *http.Request) {
	if h.logStore == nil {
		RespondJSON(w, http.StatusNotFound, map[string]interface{}{"error": "Log viewer not enabled"})
		return
	}

	source := r.URL.Query().Get("source")
	if source == "" {
		source = "all"
	}

	entries := h.logStore.Last(h.logRetention, source)

	if strings.EqualFold(r.URL.Query().Get("format"), "json") {
		RespondJSON(w, http.StatusOK, map[string]interface{}{
			"status":      "ok",
			"source":      source,
			"retention":   h.logRetention.String(),
			"entry_count": len(entries),
			"entries":     entries,
		})
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(RenderLogsHTML(entries, source, h.logRetention, h.logTitle)))
}

// RespondJSON writes a JSON-encoded response with the given status code.
func RespondJSON(w http.ResponseWriter, code int, data interface{}) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

// NewServer creates the control HTTP server bound to localhost.
func NewServer(handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", HTTP_PORT),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// ServeWithRetry runs the server until Shutdown, retrying when the port can't be bound
// (e.g. the previous instance is still releasing it) instead of leaving the service without API.
func ServeWithRetry(server *http.Server, logf func(isError bool, msg string)) {
	for {
		logf(false, fmt.Sprintf("HTTP server listening on %s", server.Addr))
		err := server.ListenAndServe()
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		logf(true, fmt.Sprintf("HTTP server error: %v, retrying in 2s", err))
		time.Sleep(2 * time.Second)
	}
}
