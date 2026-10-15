package daemon

import (
	"database/sql"
	"errors"
	"net/http"

	"paas/internal/contracts"
	"paas/internal/deployment"
	"paas/internal/store"
)

// registerDiagnosticsRoute installs the read-only diagnostics contract. The
// callback is true only when the daemon has a durable GitHub source to retry.
func registerDiagnosticsRoute(mux *http.ServeMux, service *deployment.Service, canRedeploy func(string) bool) {
	mux.HandleFunc("/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
			writeResponse(w, "diagnostics", Response{Error: "method not allowed"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, contracts.MaxControlRequestBytes)
		var input *Request
		if err := decodeDaemonJSON(r.Body, &input); err != nil || input == nil {
			w.WriteHeader(http.StatusBadRequest)
			writeResponse(w, "diagnostics", Response{Error: "invalid JSON request"})
			return
		}
		if service == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeResponse(w, "diagnostics", Response{Error: "diagnostics are unavailable"})
			return
		}
		available := false
		if canRedeploy != nil {
			available = canRedeploy(input.Name)
		}
		result, err := service.Diagnostics(input.Name, available)
		if err != nil {
			status := http.StatusInternalServerError
			message := "Could not securely load app diagnostics."
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
				status = http.StatusNotFound
				message = "Application not found."
			}
			w.WriteHeader(status)
			writeResponse(w, "diagnostics", Response{Error: message})
			return
		}
		writeResponse(w, "diagnostics", Response{Diagnostics: result})
	})
}
