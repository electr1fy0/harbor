package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"paas/internal/deployment"
	"paas/internal/model"
	"paas/internal/store"
)

type dataServiceManager interface {
	DataServiceList(context.Context) ([]model.DataServiceSummary, error)
	Unlink(context.Context, string, string) error
	CreateBackup(context.Context, string, string, bool) (*model.DataBackup, error)
	VerifyBackup(context.Context, string) (*model.DataBackup, error)
	DownloadBackup(context.Context, string) (*model.DataBackup, []byte, error)
	SetBackupPolicy(context.Context, string, string, int, bool) (*model.BackupPolicy, error)
	RestoreBackup(context.Context, string, string, string) (*model.Binding, error)
	ExportEncrypted(context.Context, string, []string) (*model.DataServiceExport, error)
}

type dataServiceRequest struct {
	Action      string   `json:"action"`
	ServiceType string   `json:"serviceType,omitempty"`
	Name        string   `json:"name,omitempty"`
	AppName     string   `json:"appName,omitempty"`
	ServiceName string   `json:"serviceName,omitempty"`
	BackupID    string   `json:"backupId,omitempty"`
	NewAppName  string   `json:"newAppName,omitempty"`
	Frequency   string   `json:"frequency,omitempty"`
	Retention   int      `json:"retention,omitempty"`
	Enabled     *bool    `json:"enabled,omitempty"`
	Passphrase  string   `json:"passphrase,omitempty"`
	AppNames    []string `json:"appNames,omitempty"`
}

type dataServiceBindingResponse struct {
	ID        string `json:"id"`
	AppName   string `json:"appName"`
	ServiceID string `json:"serviceId"`
	CreatedAt string `json:"createdAt"`
}

// registerDataServicesRoutes adds safe, lower-camel-case API routes for
// managed data services. Binding mutations take effect on the next app restart
// or deployment; an existing runtime keeps its current environment and mount.
func registerDataServicesRoutes(mux *http.ServeMux, services ServiceManager, deployments *deployment.Service) {
	_ = deployments // bindings are intentionally picked up by the next deploy/restart.
	mux.HandleFunc("/data-services", func(w http.ResponseWriter, r *http.Request) {
		if services == nil {
			writeDataServiceError(w, http.StatusServiceUnavailable, "Data services are unavailable.")
			return
		}
		manager, hasDataFeatures := services.(dataServiceManager)
		switch r.Method {
		case http.MethodGet:
			writeDataServiceList(w, r, services, manager, hasDataFeatures)
		case http.MethodPost:
			var request dataServiceRequest
			if !decodeDataServiceRequest(w, r, &request) {
				return
			}
			switch request.Action {
			case "list":
				writeDataServiceList(w, r, services, manager, hasDataFeatures)
			case "create":
				if request.ServiceType != "sqlite" || strings.TrimSpace(request.Name) == "" {
					writeDataServiceError(w, http.StatusBadRequest, "A SQLite service type and name are required.")
					return
				}
				created, err := services.AddService(r.Context(), request.ServiceType, request.Name)
				if err != nil {
					writeDataServiceOperationError(w, err, "The SQLite service could not be created.")
					return
				}
				if hasDataFeatures {
					if summaries, err := manager.DataServiceList(r.Context()); err == nil {
						for _, summary := range summaries {
							if summary.ID == created.ID {
								writeDataServiceJSON(w, http.StatusCreated, map[string]any{"service": summary})
								return
							}
						}
					}
				}
				writeDataServiceJSON(w, http.StatusCreated, map[string]any{"service": safeService(created)})
			case "link":
				if strings.TrimSpace(request.AppName) == "" || strings.TrimSpace(request.ServiceName) == "" {
					writeDataServiceError(w, http.StatusBadRequest, "Application and service names are required.")
					return
				}
				binding, err := services.Link(r.Context(), request.AppName, request.ServiceName)
				if err != nil {
					writeDataServiceOperationError(w, err, "The application could not be linked to the service.")
					return
				}
				writeDataServiceJSON(w, http.StatusOK, map[string]any{"binding": safeBinding(binding, request.AppName), "restartRequired": true})
			case "unlink":
				if !hasDataFeatures {
					writeDataServiceError(w, http.StatusServiceUnavailable, "Service unlink is unavailable.")
					return
				}
				if strings.TrimSpace(request.AppName) == "" || strings.TrimSpace(request.ServiceName) == "" {
					writeDataServiceError(w, http.StatusBadRequest, "Application and service names are required.")
					return
				}
				if err := manager.Unlink(r.Context(), request.AppName, request.ServiceName); err != nil {
					writeDataServiceOperationError(w, err, "The service binding could not be removed.")
					return
				}
				writeDataServiceJSON(w, http.StatusOK, map[string]any{"unlinked": true, "restartRequired": true, "dataPreserved": true})
			case "backup":
				if !hasDataFeatures {
					writeDataServiceError(w, http.StatusServiceUnavailable, "SQLite backups are unavailable.")
					return
				}
				backup, err := manager.CreateBackup(r.Context(), request.AppName, request.ServiceName, false)
				if err != nil {
					writeDataServiceOperationError(w, err, "The SQLite backup could not be created.")
					return
				}
				writeDataServiceJSON(w, http.StatusCreated, map[string]any{"backup": backup})
			case "verifyBackup":
				if !hasDataFeatures || request.BackupID == "" {
					writeDataServiceError(w, http.StatusBadRequest, "A backup id is required.")
					return
				}
				backup, err := manager.VerifyBackup(r.Context(), request.BackupID)
				if err != nil {
					writeDataServiceOperationError(w, err, "The SQLite backup did not pass verification.")
					return
				}
				writeDataServiceJSON(w, http.StatusOK, map[string]any{"backup": backup})
			case "downloadBackup":
				if !hasDataFeatures || request.BackupID == "" {
					writeDataServiceError(w, http.StatusBadRequest, "A backup id is required.")
					return
				}
				backup, content, err := manager.DownloadBackup(r.Context(), request.BackupID)
				if err != nil {
					writeDataServiceOperationError(w, err, "The backup could not be downloaded; verify it or use a smaller snapshot.")
					return
				}
				writeDataServiceJSON(w, http.StatusOK, map[string]any{"download": map[string]any{
					"fileName":    "harbor-backup-" + backup.ID + ".sqlite",
					"contentType": "application/vnd.sqlite3", "data": base64.StdEncoding.EncodeToString(content),
					"sizeBytes": len(content),
				}})
			case "restoreBackup":
				if !hasDataFeatures {
					writeDataServiceError(w, http.StatusServiceUnavailable, "SQLite restore is unavailable.")
					return
				}
				binding, err := manager.RestoreBackup(r.Context(), request.BackupID, request.NewAppName, request.ServiceName)
				if err != nil {
					writeDataServiceOperationError(w, err, "A new application could not be restored from this backup.")
					return
				}
				writeDataServiceJSON(w, http.StatusCreated, map[string]any{"binding": safeBinding(binding, request.NewAppName), "restartRequired": true, "restoredAsNewApp": true})
			case "setBackupPolicy":
				if !hasDataFeatures {
					writeDataServiceError(w, http.StatusServiceUnavailable, "Scheduled backups are unavailable.")
					return
				}
				enabled := true
				if request.Enabled != nil {
					enabled = *request.Enabled
				}
				policy, err := manager.SetBackupPolicy(r.Context(), request.ServiceName, request.Frequency, request.Retention, enabled)
				if err != nil {
					writeDataServiceOperationError(w, err, "The scheduled backup policy could not be saved.")
					return
				}
				writeDataServiceJSON(w, http.StatusOK, map[string]any{"backupPolicy": policy})
			case "export":
				if !hasDataFeatures {
					writeDataServiceError(w, http.StatusServiceUnavailable, "Harbor export is unavailable.")
					return
				}
				export, err := manager.ExportEncrypted(r.Context(), request.Passphrase, request.AppNames)
				if err != nil {
					writeDataServiceOperationError(w, err, "The encrypted Harbor export could not be created.")
					return
				}
				writeDataServiceJSON(w, http.StatusOK, map[string]any{"export": export})
			default:
				writeDataServiceError(w, http.StatusBadRequest, "The requested data-service action is not supported.")
			}
		default:
			w.Header().Set("Allow", "GET, POST")
			writeDataServiceError(w, http.StatusMethodNotAllowed, "Method not allowed.")
		}
	})
}

func writeDataServiceList(w http.ResponseWriter, r *http.Request, services ServiceManager, manager dataServiceManager, extended bool) {
	if extended {
		summaries, err := manager.DataServiceList(r.Context())
		if err != nil {
			writeDataServiceOperationError(w, err, "Data services could not be loaded.")
			return
		}
		writeDataServiceJSON(w, http.StatusOK, map[string]any{"services": summaries})
		return
	}
	list, err := services.List(r.Context())
	if err != nil {
		writeDataServiceOperationError(w, err, "Data services could not be loaded.")
		return
	}
	output := make([]safeDataService, 0, len(list))
	for _, svc := range list {
		output = append(output, safeService(&svc))
	}
	writeDataServiceJSON(w, http.StatusOK, map[string]any{"services": output})
}

type safeDataService struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Type      string             `json:"type"`
	Version   string             `json:"version"`
	State     model.ServiceState `json:"state"`
	CreatedAt string             `json:"createdAt"`
}

func safeService(svc *model.Service) safeDataService {
	if svc == nil {
		return safeDataService{}
	}
	return safeDataService{ID: svc.ID, Name: svc.Name, Type: svc.Type, Version: svc.Version, State: svc.State, CreatedAt: svc.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")}
}

func safeBinding(binding *model.Binding, appName string) dataServiceBindingResponse {
	if binding == nil {
		return dataServiceBindingResponse{}
	}
	return dataServiceBindingResponse{ID: binding.ID, AppName: appName, ServiceID: binding.ServiceID, CreatedAt: binding.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")}
}

func decodeDataServiceRequest(w http.ResponseWriter, r *http.Request, output *dataServiceRequest) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := decodeDaemonJSON(r.Body, output); err != nil {
		writeDataServiceError(w, http.StatusBadRequest, "Invalid data-service request.")
		return false
	}
	return true
}

func writeDataServiceOperationError(w http.ResponseWriter, err error, fallback string) {
	status := http.StatusBadRequest
	if errors.Is(err, store.ErrNotFound) {
		status = http.StatusNotFound
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusRequestTimeout
	}
	writeDataServiceError(w, status, fallback)
}

func writeDataServiceJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeDataServiceError(w http.ResponseWriter, status int, message string) {
	writeDataServiceJSON(w, status, map[string]string{"error": message})
}
