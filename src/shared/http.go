package shared

import (
	"encoding/json"
	"net/http"
)

// ITunnelOperations defines tunnel control operations
type ITunnelOperations interface {
	Start(username, appname string) map[string]interface{}
	Stop() map[string]interface{}
	GetStatus() map[string]interface{}
}

// HTTPHandler handles HTTP control requests
type HTTPHandler struct {
	tunnelOps ITunnelOperations
}

// NewHTTPHandler creates a new HTTP handler
func NewHTTPHandler(tunnelOps ITunnelOperations) *HTTPHandler {
	return &HTTPHandler{
		tunnelOps: tunnelOps,
	}
}

// ServeHTTP handles incoming HTTP requests
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch r.URL.Path {
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
		RespondJSON(w, http.StatusOK, map[string]interface{}{
			"status": "ok",
		})

	default:
		RespondJSON(w, http.StatusNotFound, map[string]interface{}{
			"error": "Not found",
		})
	}
}

// RespondJSON sends a JSON response
func RespondJSON(w http.ResponseWriter, code int, data interface{}) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}
