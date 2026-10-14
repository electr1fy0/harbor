package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"paas/internal/onboarding"
)

func registerOnboardingRoutes(mux *http.ServeMux, server *Server) {
	mux.HandleFunc("/github/connection", func(w http.ResponseWriter, r *http.Request) {
		jobs := server.onboardingJobs
		if jobs == nil {
			writeOnboardingError(w, http.StatusServiceUnavailable, "GitHub connection is unavailable.")
			return
		}
		switch r.Method {
		case http.MethodGet:
			connection, err := jobs.Connection()
			if err != nil {
				writeOnboardingError(w, http.StatusInternalServerError, "GitHub connection status is unavailable.")
				return
			}
			writeOnboardingJSON(w, http.StatusOK, map[string]any{"connection": connection})
		case http.MethodPost:
			var request struct {
				Token string `json:"token"`
			}
			if !decodeOnboardingRequest(w, r, &request) {
				return
			}
			connection, err := jobs.ConnectGitHub(r.Context(), request.Token)
			if err != nil {
				writeOnboardingError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeOnboardingJSON(w, http.StatusOK, map[string]any{"connection": connection})
		case http.MethodDelete:
			if err := jobs.DisconnectGitHub(); err != nil {
				writeOnboardingError(w, http.StatusInternalServerError, "GitHub connection could not be removed.")
				return
			}
			writeOnboardingJSON(w, http.StatusOK, map[string]any{"connection": map[string]bool{"connected": false}})
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			writeOnboardingError(w, http.StatusMethodNotAllowed, "Method not allowed.")
		}
	})
	mux.HandleFunc("/github/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOnboardingError(w, http.StatusMethodNotAllowed, "Method not allowed.")
			return
		}
		if server.onboardingJobs == nil {
			writeOnboardingError(w, http.StatusServiceUnavailable, "GitHub repositories are unavailable.")
			return
		}
		repositories, err := server.onboardingJobs.BrowseRepositories(r.Context(), r.URL.Query().Get("owner"))
		if err != nil {
			writeOnboardingError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeOnboardingJSON(w, http.StatusOK, map[string]any{"repositories": repositories})
	})
	mux.HandleFunc("/github/repo", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOnboardingError(w, http.StatusMethodNotAllowed, "Method not allowed.")
			return
		}
		if server.onboardingJobs == nil {
			writeOnboardingError(w, http.StatusServiceUnavailable, "GitHub repositories are unavailable.")
			return
		}
		repo, err := server.onboardingJobs.Repository(r.Context(), r.URL.Query().Get("owner"), r.URL.Query().Get("repo"))
		if err != nil {
			writeOnboardingError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeOnboardingJSON(w, http.StatusOK, map[string]any{"repository": repo})
	})

	mux.HandleFunc("/projects/inspect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOnboardingError(w, http.StatusMethodNotAllowed, "Method not allowed.")
			return
		}
		var request struct {
			Name    string                     `json:"name,omitempty"`
			RepoURL string                     `json:"repoURL"`
			Ref     string                     `json:"ref,omitempty"`
			RootDir string                     `json:"rootDir,omitempty"`
			Config  onboarding.ConfigSelection `json:"config,omitempty"`
		}
		if !decodeOnboardingRequest(w, r, &request) {
			return
		}
		if server.onboardingJobs == nil {
			writeOnboardingError(w, http.StatusServiceUnavailable, "Project inspection is unavailable.")
			return
		}
		inspection, err := server.onboardingJobs.InspectProject(r.Context(), request.RepoURL, request.Ref, request.RootDir, request.Name, request.Config)
		if err != nil {
			writeOnboardingError(w, onboardingErrorStatus(err), err.Error())
			return
		}
		writeOnboardingJSON(w, http.StatusOK, map[string]any{"inspection": inspection})
	})
	mux.HandleFunc("/jobs/", func(w http.ResponseWriter, r *http.Request) {
		jobs := server.onboardingJobs
		if jobs == nil {
			writeOnboardingError(w, http.StatusServiceUnavailable, "Deployment jobs are unavailable.")
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/jobs/")
		switch path {
		case "deploy":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeOnboardingError(w, http.StatusMethodNotAllowed, "Method not allowed.")
				return
			}
			var request onboarding.DeployRequest
			if !decodeOnboardingRequest(w, r, &request) {
				return
			}
			job, err := jobs.Submit(r.Context(), request)
			if err != nil {
				writeOnboardingError(w, onboardingErrorStatus(err), err.Error())
				return
			}
			writeOnboardingJSON(w, http.StatusAccepted, map[string]any{"job": job})
		case "redeploy":
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeOnboardingError(w, http.StatusMethodNotAllowed, "Method not allowed.")
				return
			}
			var request struct {
				Name string `json:"name"`
			}
			if !decodeOnboardingRequest(w, r, &request) {
				return
			}
			job, err := jobs.Redeploy(r.Context(), request.Name)
			if err != nil {
				writeOnboardingError(w, onboardingErrorStatus(err), err.Error())
				return
			}
			writeOnboardingJSON(w, http.StatusAccepted, map[string]any{"job": job})
		default:
			id := strings.TrimSuffix(path, "/")
			if id == "" || strings.Contains(id, "/") {
				writeOnboardingError(w, http.StatusNotFound, "Deployment job was not found.")
				return
			}
			var job onboarding.Job
			var err error
			switch r.Method {
			case http.MethodGet:
				job, err = jobs.Get(id)
			case http.MethodDelete:
				job, err = jobs.Cancel(id)
			default:
				w.Header().Set("Allow", "GET, DELETE")
				writeOnboardingError(w, http.StatusMethodNotAllowed, "Method not allowed.")
				return
			}
			if err != nil {
				writeOnboardingError(w, onboardingErrorStatus(err), err.Error())
				return
			}
			writeOnboardingJSON(w, http.StatusOK, map[string]any{"job": job})
		}
	})
}

func decodeOnboardingRequest(w http.ResponseWriter, r *http.Request, output any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		writeOnboardingError(w, http.StatusBadRequest, "Invalid JSON request.")
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeOnboardingError(w, http.StatusBadRequest, "Request must contain one JSON object.")
		return false
	}
	return true
}

func onboardingErrorStatus(err error) int {
	switch {
	case errors.Is(err, onboarding.ErrJobNotFound):
		return http.StatusNotFound
	case errors.Is(err, onboarding.ErrQueueFull):
		return http.StatusTooManyRequests
	case errors.Is(err, onboarding.ErrManagerClosed):
		return http.StatusServiceUnavailable
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout
	default:
		return http.StatusBadRequest
	}
}

func writeOnboardingJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeOnboardingError(w http.ResponseWriter, status int, message string) {
	writeOnboardingJSON(w, status, map[string]string{"error": message})
}
