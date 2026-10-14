package daemon

import (
	"net/http"

	"paas/internal/onboarding"
)

type automationSourceInput struct {
	Name                string                     `json:"name"`
	RepoURL             string                     `json:"repoURL"`
	Ref                 string                     `json:"ref"`
	RootDir             string                     `json:"rootDir"`
	Config              onboarding.ConfigSelection `json:"config"`
	IncludePaths        []string                   `json:"includePaths,omitempty"`
	IgnorePaths         []string                   `json:"ignorePaths,omitempty"`
	BuildTimeoutSeconds int                        `json:"buildTimeoutSeconds,omitempty"`
	BuildEnvironment    *map[string]string         `json:"buildEnvironment,omitempty"`
}

// registerAutomationRoutes registers the dashboard-authenticated automation
// control endpoint. Public webhook traffic is served by Server.PublicHooks.
func registerAutomationRoutes(mux *http.ServeMux, server *Server) {
	mux.HandleFunc("/automation", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOnboardingError(w, http.StatusMethodNotAllowed, "Method not allowed.")
			return
		}
		jobs := server.onboardingJobs
		if jobs == nil {
			writeOnboardingError(w, http.StatusServiceUnavailable, "Automation is unavailable.")
			return
		}
		var request struct {
			Action     string                 `json:"action"`
			Name       string                 `json:"name,omitempty"`
			ID         string                 `json:"id,omitempty"`
			Branch     string                 `json:"branch,omitempty"`
			ClearCache bool                   `json:"clearCache,omitempty"`
			Source     *automationSourceInput `json:"source,omitempty"`
		}
		if !decodeOnboardingRequest(w, r, &request) {
			return
		}
		switch request.Action {
		case "getSource":
			settings, keys, err := jobs.GetSource(request.Name)
			if err != nil {
				writeOnboardingError(w, onboardingErrorStatus(err), "Saved repository settings are unavailable.")
				return
			}
			writeOnboardingJSON(w, http.StatusOK, map[string]any{"source": settings, "buildEnvironmentKeys": keys})
		case "updateSource":
			if request.Source == nil {
				writeOnboardingError(w, http.StatusBadRequest, "Source settings are required.")
				return
			}
			input := request.Source
			settings := onboarding.SourceSettings{Name: input.Name, RepoURL: input.RepoURL, Ref: input.Ref, RootDir: input.RootDir,
				Selection: input.Config, IncludePaths: input.IncludePaths, IgnorePaths: input.IgnorePaths, BuildTimeoutSeconds: input.BuildTimeoutSeconds}
			if err := jobs.UpdateSource(r.Context(), settings, input.BuildEnvironment); err != nil {
				writeOnboardingError(w, onboardingErrorStatus(err), err.Error())
				return
			}
			updated, keys, err := jobs.GetSource(settings.Name)
			if err != nil {
				writeOnboardingError(w, http.StatusInternalServerError, "Saved repository settings could not be read.")
				return
			}
			writeOnboardingJSON(w, http.StatusOK, map[string]any{"source": updated, "buildEnvironmentKeys": keys})
		case "forgetSource":
			if err := jobs.Forget(request.Name); err != nil {
				writeOnboardingError(w, onboardingErrorStatus(err), err.Error())
				return
			}
			writeOnboardingJSON(w, http.StatusOK, map[string]bool{"forgotten": true})
		case "createHook":
			metadata, secret, err := jobs.CreateHook(r.Context(), request.Name, request.Branch)
			if err != nil {
				writeOnboardingError(w, onboardingErrorStatus(err), err.Error())
				return
			}
			writeOnboardingJSON(w, http.StatusCreated, map[string]any{
				"hook": metadata, "secret": secret,
				"githubPath": "/api/hooks/github/" + metadata.ID,
				"deployPath": "/api/hooks/deploy/" + metadata.ID,
			})
		case "listHooks":
			hooks, err := jobs.ListHooks(request.Name)
			if err != nil {
				writeOnboardingError(w, http.StatusServiceUnavailable, "Webhook metadata is unavailable.")
				return
			}
			writeOnboardingJSON(w, http.StatusOK, map[string]any{"hooks": hooks})
		case "revokeHook":
			if err := jobs.RevokeHook(request.ID); err != nil {
				writeOnboardingError(w, onboardingErrorStatus(err), err.Error())
				return
			}
			writeOnboardingJSON(w, http.StatusOK, map[string]bool{"revoked": true})
		case "regenerateHook":
			metadata, secret, err := jobs.RegenerateHook(request.ID)
			if err != nil {
				writeOnboardingError(w, onboardingErrorStatus(err), err.Error())
				return
			}
			writeOnboardingJSON(w, http.StatusOK, map[string]any{"hook": metadata, "secret": secret})
		case "redeploy":
			job, err := jobs.RedeployWithOptions(r.Context(), request.Name, request.ClearCache)
			if err != nil {
				writeOnboardingError(w, onboardingErrorStatus(err), err.Error())
				return
			}
			writeOnboardingJSON(w, http.StatusAccepted, map[string]any{"job": job})
		case "listJobs":
			result := jobs.ListJobs(request.Name)
			for i := range result {
				result[i].Logs = nil
			}
			writeOnboardingJSON(w, http.StatusOK, map[string]any{"jobs": result})
		case "getJob":
			job, err := jobs.Get(request.ID)
			if err != nil {
				writeOnboardingError(w, onboardingErrorStatus(err), err.Error())
				return
			}
			writeOnboardingJSON(w, http.StatusOK, map[string]any{"job": job})
		case "cancelJob":
			job, err := jobs.Cancel(request.ID)
			if err != nil {
				writeOnboardingError(w, onboardingErrorStatus(err), err.Error())
				return
			}
			writeOnboardingJSON(w, http.StatusOK, map[string]any{"job": job})
		default:
			writeOnboardingError(w, http.StatusBadRequest, "Unknown automation action.")
		}
	})
}

// PublicHooks returns the daemon's public callback handler. A gateway can
// mount it below /api/hooks/ without applying the dashboard session gate.
func (s *Server) PublicHooks() http.Handler {
	if s == nil || s.onboardingJobs == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "webhooks unavailable", http.StatusServiceUnavailable)
		})
	}
	return s.onboardingJobs.PublicHandler()
}
