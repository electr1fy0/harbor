package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"paas/internal/config"
	"paas/internal/contracts"
	"paas/internal/deployment"
	"paas/internal/model"
	"paas/internal/store"
)

var operationIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

type operationsRequest struct {
	Action                  string    `json:"action"`
	AppName                 string    `json:"appName,omitempty"`
	DeploymentID            string    `json:"deploymentId,omitempty"`
	ReleaseID               string    `json:"releaseId,omitempty"`
	PreserveData            *bool     `json:"preserveData,omitempty"`
	Limit                   int       `json:"limit,omitempty"`
	Offset                  int       `json:"offset,omitempty"`
	Favorite                *bool     `json:"favorite,omitempty"`
	Tags                    *[]string `json:"tags,omitempty"`
	Archived                *bool     `json:"archived,omitempty"`
	RequiredEnvironmentKeys *[]string `json:"requiredEnvironmentKeys,omitempty"`
	IntervalSeconds         int       `json:"intervalSeconds,omitempty"`
	TimeoutSeconds          int       `json:"timeoutSeconds,omitempty"`
	FailureThreshold        int       `json:"failureThreshold,omitempty"`
	RetainReleaseCount      int       `json:"retainReleaseCount,omitempty"`
	Enabled                 *bool     `json:"enabled,omitempty"`
	MaxAttempts             int       `json:"maxAttempts,omitempty"`
	BaseDelaySeconds        int       `json:"baseDelaySeconds,omitempty"`
	Build                   *bool     `json:"build,omitempty"`
}

type operationDeploymentView struct {
	ID          string      `json:"id"`
	AppName     string      `json:"appName"`
	ReleaseID   string      `json:"releaseId,omitempty"`
	Port        int         `json:"port,omitempty"`
	State       model.State `json:"state"`
	Error       string      `json:"error,omitempty"`
	PID         int         `json:"pid,omitempty"`
	MemoryLimit string      `json:"memoryLimit,omitempty"`
	CPUQuota    float64     `json:"cpuQuota,omitempty"`
	TasksLimit  int         `json:"tasksLimit,omitempty"`
	HealthPath  string      `json:"healthPath,omitempty"`
	CreatedAt   time.Time   `json:"createdAt"`
	StartedAt   *time.Time  `json:"startedAt,omitempty"`
	StoppedAt   *time.Time  `json:"stoppedAt,omitempty"`
	PromotedAt  *time.Time  `json:"promotedAt,omitempty"`
}

type operationEventView struct {
	ID           int64           `json:"id"`
	DeploymentID string          `json:"deploymentId"`
	Type         model.EventType `json:"type"`
	Payload      string          `json:"payload,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
}

func registerOperationsRoutes(mux *http.ServeMux, service *deployment.Service) {
	mux.HandleFunc("/operations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOperationsError(w, http.StatusMethodNotAllowed, "Method not allowed.")
			return
		}
		if service == nil {
			writeOperationsError(w, http.StatusServiceUnavailable, "Operations are unavailable.")
			return
		}
		var request operationsRequest
		if !decodeOperationsRequest(w, r, &request) {
			return
		}
		if err := validateOperationsRequest(request); err != nil {
			writeOperationsError(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx := r.Context()
		var result any
		var err error
		switch request.Action {
		case "start":
			var d *model.Deployment
			d, err = service.Start(ctx, request.AppName)
			if err == nil {
				result = map[string]any{"deployment": operationDeployment(service, request.AppName, d)}
			}
		case "restart":
			var d *model.Deployment
			d, err = service.Restart(ctx, request.AppName)
			if err == nil {
				result = map[string]any{"deployment": operationDeployment(service, request.AppName, d)}
			}
		case "stop":
			var d *model.Deployment
			d, err = service.Stop(ctx, request.AppName)
			if err == nil {
				result = map[string]any{"deployment": operationDeployment(service, request.AppName, d)}
			}
		case "rollback":
			var d *model.Deployment
			d, err = service.Rollback(ctx, request.AppName, request.ReleaseID)
			if err == nil {
				result = map[string]any{"deployment": operationDeployment(service, request.AppName, d)}
			}
		case "cancelCandidate":
			var d *model.Deployment
			d, err = service.CancelCandidate(ctx, request.AppName, request.DeploymentID)
			if err == nil {
				result = map[string]any{"deployment": operationDeployment(service, request.AppName, d)}
			}
		case "delete":
			err = service.Delete(ctx, request.AppName, *request.PreserveData)
			if err == nil {
				result = map[string]any{"deleted": true, "appName": request.AppName, "preservedData": *request.PreserveData}
			}
		case "archive", "unarchive":
			archived := request.Action == "archive"
			var settings *model.OperationSettings
			settings, err = service.SetArchived(ctx, request.AppName, archived)
			if err == nil {
				result = map[string]any{"operations": settings}
			}
		case "configureMetadata":
			var settings *model.OperationSettings
			settings, err = service.ConfigureMetadata(ctx, request.AppName, deployment.MetadataChanges{
				Favorite: request.Favorite, Tags: request.Tags, Archived: request.Archived,
				RequiredEnvironmentKeys: request.RequiredEnvironmentKeys,
			})
			if err == nil {
				result = map[string]any{"operations": settings}
			}
		case "healthPolicy":
			var settings *model.OperationSettings
			settings, err = service.ConfigureHealthPolicy(ctx, request.AppName, deployment.HealthPolicy{
				IntervalSeconds: request.IntervalSeconds, TimeoutSeconds: request.TimeoutSeconds,
				FailureThreshold: request.FailureThreshold,
			})
			if err == nil {
				result = map[string]any{"operations": settings}
			}
		case "restartPolicy":
			var settings *model.OperationSettings
			settings, err = service.ConfigureRestartPolicy(ctx, request.AppName, deployment.RestartPolicy{
				Enabled: *request.Enabled, MaxAttempts: request.MaxAttempts, BaseDelaySeconds: request.BaseDelaySeconds,
			})
			if err == nil {
				result = map[string]any{"operations": settings}
			}
		case "retention":
			var settings *model.OperationSettings
			var pruned []string
			settings, pruned, err = service.ConfigureRetention(ctx, request.AppName, request.RetainReleaseCount)
			if err == nil {
				result = map[string]any{"operations": settings, "prunedReleaseIds": pruned}
			}
		case "pruneReleases":
			var pruned []string
			pruned, err = service.PruneReleases(ctx, request.AppName)
			if err == nil {
				result = map[string]any{"prunedReleaseIds": pruned}
			}
		case "pruneCache":
			var pruned bool
			pruned, err = service.PruneBuildCache(ctx)
			if err == nil {
				result = map[string]any{"pruned": pruned}
			}
		case "preflight":
			var report *deployment.PreflightResult
			buildRequired := true
			if request.Build != nil {
				buildRequired = *request.Build
			}
			report, err = service.Preflight(ctx, request.AppName, buildRequired)
			if err == nil {
				result = map[string]any{"preflight": report}
			}
		case "settings":
			var settings *model.Settings
			var operations *model.OperationSettings
			settings, err = service.Settings(request.AppName)
			if err == nil {
				operations, err = service.OperationSettings(request.AppName)
			}
			if err == nil {
				result = map[string]any{"settings": settings, "operations": operations}
			}
		case "history":
			var page *deployment.HistoryPage
			page, err = service.History(request.AppName, request.Limit, request.Offset)
			if err == nil {
				views := make([]map[string]any, 0, len(page.Deployments))
				for _, entry := range page.Deployments {
					views = append(views, map[string]any{"deployment": operationDeployment(service, request.AppName, &entry.Deployment), "stages": entry.Stages})
				}
				result = map[string]any{"deployments": views, "total": page.Total, "offset": page.Offset, "limit": page.Limit, "hasMore": page.HasMore}
			}
		case "historyAll":
			var page *deployment.HistoryPage
			page, err = service.HistoryAll(request.Limit, request.Offset)
			if err == nil {
				views := make([]map[string]any, 0, len(page.Deployments))
				for _, entry := range page.Deployments {
					views = append(views, map[string]any{"deployment": operationDeployment(service, entry.Deployment.AppName, &entry.Deployment), "stages": entry.Stages})
				}
				result = map[string]any{"deployments": views, "total": page.Total, "offset": page.Offset, "limit": page.Limit, "hasMore": page.HasMore}
			}
		case "events":
			var page *deployment.EventPage
			page, err = service.EventsPage(request.AppName, request.DeploymentID, request.Limit, request.Offset)
			if err == nil {
				views := make([]operationEventView, 0, len(page.Events))
				for _, event := range page.Events {
					views = append(views, operationEventView{ID: event.ID, DeploymentID: event.DeploymentID, Type: event.Type, Payload: event.Payload, CreatedAt: event.CreatedAt})
				}
				result = map[string]any{"events": views, "total": page.Total, "offset": page.Offset, "limit": page.Limit, "hasMore": page.HasMore}
			}
		default:
			writeOperationsError(w, http.StatusBadRequest, "Unknown operation action.")
			return
		}
		if err != nil {
			status := operationsErrorStatus(err)
			message := "Operation failed."
			if redacted, redactErr := service.Redact(request.AppName, err.Error()); redactErr == nil {
				message = redacted
			}
			writeOperationsError(w, status, message)
			return
		}
		writeOperationsJSON(w, http.StatusOK, result)
	})
}

func validateOperationsRequest(request operationsRequest) error {
	if request.Action == "" {
		return errors.New("action is required")
	}
	if request.Action != "pruneCache" && request.Action != "historyAll" {
		if !config.ValidName(request.AppName) {
			return errors.New("a valid appName is required")
		}
	}
	if request.DeploymentID != "" && !operationIdentityPattern.MatchString(request.DeploymentID) {
		return errors.New("deploymentId is invalid")
	}
	if request.ReleaseID != "" && !operationIdentityPattern.MatchString(request.ReleaseID) {
		return errors.New("releaseId is invalid")
	}
	if request.Limit < 0 || request.Limit > 500 || request.Offset < 0 || request.Offset > 10_000_000 {
		return errors.New("history pagination is out of range")
	}
	policyFieldsPresent := request.Enabled != nil || request.MaxAttempts != 0 || request.BaseDelaySeconds != 0
	switch request.Action {
	case "start", "restart", "stop", "archive", "unarchive", "pruneReleases", "settings", "preflight":
		if request.DeploymentID != "" || request.ReleaseID != "" || request.PreserveData != nil || request.Limit != 0 || request.Offset != 0 || request.Favorite != nil || request.Tags != nil || request.Archived != nil || request.RequiredEnvironmentKeys != nil || request.IntervalSeconds != 0 || request.TimeoutSeconds != 0 || request.FailureThreshold != 0 || request.RetainReleaseCount != 0 || policyFieldsPresent || request.Action != "preflight" && request.Build != nil {
			return errors.New("request contains fields that do not apply to this action")
		}
	case "rollback":
		if request.DeploymentID != "" || request.PreserveData != nil || request.Limit != 0 || request.Offset != 0 || request.Favorite != nil || request.Tags != nil || request.Archived != nil || request.RequiredEnvironmentKeys != nil || request.IntervalSeconds != 0 || request.TimeoutSeconds != 0 || request.FailureThreshold != 0 || request.RetainReleaseCount != 0 || policyFieldsPresent || request.Build != nil {
			return errors.New("rollback accepts only appName and optional releaseId")
		}
	case "cancelCandidate":
		if request.DeploymentID == "" || request.ReleaseID != "" || request.PreserveData != nil || request.Limit != 0 || request.Offset != 0 || request.Favorite != nil || request.Tags != nil || request.Archived != nil || request.RequiredEnvironmentKeys != nil || request.IntervalSeconds != 0 || request.TimeoutSeconds != 0 || request.FailureThreshold != 0 || request.RetainReleaseCount != 0 || policyFieldsPresent || request.Build != nil {
			return errors.New("cancelCandidate requires only deploymentId and appName")
		}
	case "delete":
		if request.PreserveData == nil || request.DeploymentID != "" || request.ReleaseID != "" || request.Limit != 0 || request.Offset != 0 || request.Favorite != nil || request.Tags != nil || request.Archived != nil || request.RequiredEnvironmentKeys != nil || request.IntervalSeconds != 0 || request.TimeoutSeconds != 0 || request.FailureThreshold != 0 || request.RetainReleaseCount != 0 || policyFieldsPresent || request.Build != nil {
			return errors.New("delete requires an explicit preserveData choice")
		}
	case "configureMetadata":
		if request.Favorite == nil && request.Tags == nil && request.Archived == nil && request.RequiredEnvironmentKeys == nil {
			return errors.New("configureMetadata requires at least one metadata field")
		}
		if request.DeploymentID != "" || request.ReleaseID != "" || request.PreserveData != nil || request.Limit != 0 || request.Offset != 0 || request.IntervalSeconds != 0 || request.TimeoutSeconds != 0 || request.FailureThreshold != 0 || request.RetainReleaseCount != 0 || policyFieldsPresent || request.Build != nil {
			return errors.New("request contains fields that do not apply to configureMetadata")
		}
	case "healthPolicy":
		if request.TimeoutSeconds == 0 || request.FailureThreshold == 0 || request.DeploymentID != "" || request.ReleaseID != "" || request.PreserveData != nil || request.Limit != 0 || request.Offset != 0 || request.Favorite != nil || request.Tags != nil || request.Archived != nil || request.RequiredEnvironmentKeys != nil || request.RetainReleaseCount != 0 || request.MaxAttempts != 0 || request.BaseDelaySeconds != 0 || request.Build != nil {
			return errors.New("healthPolicy requires intervalSeconds (0 disables probes), timeoutSeconds, and failureThreshold")
		}
		if request.Enabled != nil && !*request.Enabled && request.IntervalSeconds != 0 {
			return errors.New("disabled health probes require intervalSeconds: 0")
		}
		if request.Enabled != nil && *request.Enabled && request.IntervalSeconds == 0 {
			return errors.New("enabled health probes require a non-zero intervalSeconds")
		}
	case "restartPolicy":
		if request.Enabled == nil || request.MaxAttempts == 0 || request.BaseDelaySeconds == 0 || request.DeploymentID != "" || request.ReleaseID != "" || request.PreserveData != nil || request.Limit != 0 || request.Offset != 0 || request.Favorite != nil || request.Tags != nil || request.Archived != nil || request.RequiredEnvironmentKeys != nil || request.IntervalSeconds != 0 || request.TimeoutSeconds != 0 || request.FailureThreshold != 0 || request.RetainReleaseCount != 0 || request.Build != nil {
			return errors.New("restartPolicy requires enabled, maxAttempts, and baseDelaySeconds")
		}
	case "retention":
		if request.RetainReleaseCount < 1 || request.RetainReleaseCount > 1000 || request.DeploymentID != "" || request.ReleaseID != "" || request.PreserveData != nil || request.Limit != 0 || request.Offset != 0 || request.Favorite != nil || request.Tags != nil || request.Archived != nil || request.RequiredEnvironmentKeys != nil || request.IntervalSeconds != 0 || request.TimeoutSeconds != 0 || request.FailureThreshold != 0 || policyFieldsPresent || request.Build != nil {
			return errors.New("retention requires retainReleaseCount from 1 to 1000")
		}
	case "pruneCache":
		if request.AppName != "" || request.DeploymentID != "" || request.ReleaseID != "" || request.PreserveData != nil || request.Limit != 0 || request.Offset != 0 || request.Favorite != nil || request.Tags != nil || request.Archived != nil || request.RequiredEnvironmentKeys != nil || request.IntervalSeconds != 0 || request.TimeoutSeconds != 0 || request.FailureThreshold != 0 || request.RetainReleaseCount != 0 || policyFieldsPresent || request.Build != nil {
			return errors.New("pruneCache accepts only action")
		}
	case "history":
		if request.DeploymentID != "" || request.ReleaseID != "" || request.PreserveData != nil || request.Favorite != nil || request.Tags != nil || request.Archived != nil || request.RequiredEnvironmentKeys != nil || request.IntervalSeconds != 0 || request.TimeoutSeconds != 0 || request.FailureThreshold != 0 || request.RetainReleaseCount != 0 || policyFieldsPresent || request.Build != nil {
			return errors.New("history accepts only appName, limit, and offset")
		}
	case "historyAll":
		if request.AppName != "" || request.DeploymentID != "" || request.ReleaseID != "" || request.PreserveData != nil || request.Favorite != nil || request.Tags != nil || request.Archived != nil || request.RequiredEnvironmentKeys != nil || request.IntervalSeconds != 0 || request.TimeoutSeconds != 0 || request.FailureThreshold != 0 || request.RetainReleaseCount != 0 || policyFieldsPresent || request.Build != nil {
			return errors.New("historyAll accepts only action, limit, and offset")
		}
	case "events":
		if request.ReleaseID != "" || request.PreserveData != nil || request.Favorite != nil || request.Tags != nil || request.Archived != nil || request.RequiredEnvironmentKeys != nil || request.IntervalSeconds != 0 || request.TimeoutSeconds != 0 || request.FailureThreshold != 0 || request.RetainReleaseCount != 0 || policyFieldsPresent || request.Build != nil {
			return errors.New("events accepts only appName, deploymentId, limit, and offset")
		}
	default:
		return errors.New("unknown operation action")
	}
	return nil
}

func operationDeployment(service *deployment.Service, appName string, d *model.Deployment) *operationDeploymentView {
	if d == nil {
		return nil
	}
	view := &operationDeploymentView{
		ID: d.ID, AppName: d.AppName, ReleaseID: d.ReleaseID, Port: d.Port, State: d.State, PID: d.PID,
		MemoryLimit: d.MemoryLimit, CPUQuota: d.CPUQuota, TasksLimit: d.TasksLimit, HealthPath: d.HealthPath,
		CreatedAt: d.CreatedAt, StartedAt: d.StartedAt, StoppedAt: d.StoppedAt, PromotedAt: d.PromotedAt,
	}
	if d.Error != "" {
		if value, err := service.Redact(appName, d.Error); err == nil {
			view.Error = value
		} else {
			view.Error = "[WITHHELD]"
		}
	}
	return view
}

func decodeOperationsRequest(w http.ResponseWriter, r *http.Request, output any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, contracts.MaxControlRequestBytes)
	if err := decodeDaemonJSON(r.Body, output); err != nil {
		writeOperationsError(w, http.StatusBadRequest, "Invalid JSON request.")
		return false
	}
	return true
}

func operationsErrorStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, deployment.ErrBuildCacheBusy):
		return http.StatusConflict
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout
	default:
		return http.StatusBadRequest
	}
}

func writeOperationsJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeOperationsError(w http.ResponseWriter, status int, message string) {
	writeOperationsJSON(w, status, map[string]string{"error": message})
}
