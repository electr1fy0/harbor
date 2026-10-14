package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"paas/internal/model"
)

type routeDataServiceManager struct {
	services []model.Service
	summary  []model.DataServiceSummary
	unlinked bool
}

func (m *routeDataServiceManager) AddService(_ context.Context, serviceType, name string) (*model.Service, error) {
	service := model.Service{ID: "svc_0123456789abcdef", Name: name, Type: serviceType, Version: "3", State: model.ServiceReady, VolumeDir: "/private/harbor/volumes/service"}
	m.services = append(m.services, service)
	return &service, nil
}
func (m *routeDataServiceManager) Link(context.Context, string, string) (*model.Binding, error) {
	return &model.Binding{ID: "bind_0123456789abcdef", AppID: "app_0123456789abcdef0123456789abcdef", ServiceID: "svc_0123456789abcdef", Username: "private-user", Password: "private-password", EnvKey: "DATABASE_URL", CreatedAt: time.Date(2026, 10, 14, 1, 2, 3, 0, time.UTC)}, nil
}
func (m *routeDataServiceManager) List(context.Context) ([]model.Service, error) {
	return m.services, nil
}
func (m *routeDataServiceManager) DataServiceList(context.Context) ([]model.DataServiceSummary, error) {
	return m.summary, nil
}
func (m *routeDataServiceManager) Unlink(context.Context, string, string) error {
	m.unlinked = true
	return nil
}
func (*routeDataServiceManager) CreateBackup(context.Context, string, string, bool) (*model.DataBackup, error) {
	return &model.DataBackup{ID: "bkp_0123456789abcdef", Verification: "verified", SizeBytes: 8}, nil
}
func (*routeDataServiceManager) VerifyBackup(_ context.Context, id string) (*model.DataBackup, error) {
	return &model.DataBackup{ID: id, Verification: "verified", SizeBytes: 8}, nil
}
func (*routeDataServiceManager) DownloadBackup(_ context.Context, id string) (*model.DataBackup, []byte, error) {
	return &model.DataBackup{ID: id, Verification: "verified", SizeBytes: 8}, []byte("sqlite!!"), nil
}
func (*routeDataServiceManager) SetBackupPolicy(context.Context, string, string, int, bool) (*model.BackupPolicy, error) {
	return &model.BackupPolicy{Frequency: "daily", Retention: 7, Enabled: true}, nil
}
func (*routeDataServiceManager) RestoreBackup(context.Context, string, string, string) (*model.Binding, error) {
	return &model.Binding{ID: "bind_clone", ServiceID: "svc_0123456789abcdef", CreatedAt: time.Now().UTC()}, nil
}
func (*routeDataServiceManager) ExportEncrypted(context.Context, string, []string) (*model.DataServiceExport, error) {
	return &model.DataServiceExport{FileName: "harbor-export.harbor.enc", ContentType: "application/vnd.harbor.export+encrypted", EncryptedData: "encrypted-content", SizeBytes: 16}, nil
}

func TestDataServicesRouteUsesSafeLowerCamelCaseViews(t *testing.T) {
	manager := &routeDataServiceManager{
		services: []model.Service{{ID: "svc_0123456789abcdef", Name: "primary", Type: "sqlite", Version: "3", State: model.ServiceReady, VolumeDir: "/private/harbor/volumes/service"}},
		summary:  []model.DataServiceSummary{{ID: "svc_0123456789abcdef", Name: "primary", Type: "sqlite", State: model.ServiceReady, Bindings: []model.DataBinding{}, Backups: []model.DataBackup{}}},
	}
	mux := http.NewServeMux()
	registerDataServicesRoutes(mux, manager, nil)

	for _, request := range []struct {
		method string
		body   string
	}{
		{method: http.MethodGet},
		{method: http.MethodPost, body: `{"action":"list"}`},
		{method: http.MethodPost, body: `{"action":"create","serviceType":"sqlite","name":"new"}`},
		{method: http.MethodPost, body: `{"action":"link","appName":"demo","serviceName":"primary"}`},
	} {
		req := httptest.NewRequest(request.method, "/data-services", strings.NewReader(request.body))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, req)
		if response.Code < 200 || response.Code >= 300 {
			t.Fatalf("%s %s status = %d: %s", request.method, request.body, response.Code, response.Body.String())
		}
		body := response.Body.String()
		if strings.Contains(body, "/private/harbor") || strings.Contains(body, "private-password") || strings.Contains(body, "volumeDir") || strings.Contains(body, "VolumeDir") {
			t.Fatalf("response leaked storage path or credential: %s", body)
		}
		if request.body != "" && !strings.Contains(body, `"service"`) && strings.Contains(request.body, `"action":"create"`) {
			t.Fatalf("create response is missing service: %s", body)
		}
	}

	request := httptest.NewRequest(http.MethodPost, "/data-services", strings.NewReader(`{"action":"unlink","appName":"demo","serviceName":"primary"}`))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !manager.unlinked || !strings.Contains(response.Body.String(), `"dataPreserved":true`) || !strings.Contains(response.Body.String(), `"restartRequired":true`) {
		t.Fatalf("unlink response = %d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/data-services", strings.NewReader(`{"action":"downloadBackup","backupId":"bkp_0123456789abcdef"}`))
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	var result struct {
		Download struct {
			Data     string `json:"data"`
			FileName string `json:"fileName"`
		} `json:"download"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(result.Download.Data)
	if err != nil || string(data) != "sqlite!!" || result.Download.FileName != "harbor-backup-bkp_0123456789abcdef.sqlite" {
		t.Fatalf("download response = %+v, data=%q, err=%v", result, data, err)
	}
}

func TestDataServicesRouteRejectsUnknownFieldsAndUnsupportedActions(t *testing.T) {
	manager := &routeDataServiceManager{}
	mux := http.NewServeMux()
	registerDataServicesRoutes(mux, manager, nil)
	for _, body := range []string{
		`{"action":"list","volumeDir":"/tmp"}`,
		`{"action":"readBackupFile"}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/data-services", strings.NewReader(body))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("request %s status = %d: %s", body, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "/tmp") {
			t.Fatalf("error leaked input path: %s", response.Body.String())
		}
	}
}
