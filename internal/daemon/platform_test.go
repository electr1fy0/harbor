package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"paas/internal/deployment"
	"paas/internal/router"
	"paas/internal/store"
)

func TestPlatformRejectsMalformedRequests(t *testing.T) {
	server := NewPlatformServer("", nil)
	for _, body := range []string{
		"", "null", `{"unknown":true}`, `{} {}`, `{} garbage`,
		`{"Name":"first","Name":"second"}`,
		`{"Environment":{"TOKEN":null},"Environment":{"SAFE":"ok"}}`,
		strings.Repeat(" ", 65<<10),
	} {
		recorder := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/deploy", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("request %q status=%d", body[:min(len(body), 40)], recorder.Code)
		}
	}
}

func TestSettingsAndConfigureMetadataAPI(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	routes := router.New()
	appService, err := deployment.New(db, nil, nil, routes, deployment.Options{DataDir: filepath.Join(root, "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer appService.Close()
	server := NewPlatformServer("", appService)
	defer func() { _ = server.Shutdown(context.Background()) }()

	configure := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(configure, httptest.NewRequest(http.MethodPost, "/configure", strings.NewReader(
		`{"Name":"private","Environment":{"TOKEN":"api-secret"},"MemoryLimit":"192Mi","CPUQuota":0.5,"TasksLimit":24}`)))
	if configure.Code != http.StatusOK {
		t.Fatalf("configure status=%d body=%s", configure.Code, configure.Body.String())
	}
	body := configure.Body.String()
	if strings.Contains(body, "api-secret") {
		t.Fatalf("configure response leaked an environment value: %s", body)
	}
	for _, required := range []string{`"settings"`, `"environmentKeys":["TOKEN"]`, `"memoryLimit":"192Mi"`, `"cpuQuota":0.5`, `"tasksLimit":24`} {
		if !strings.Contains(body, required) {
			t.Errorf("configure response missing %s: %s", required, body)
		}
	}
	if strings.Contains(body, `"Settings"`) || strings.Contains(body, `"Environment"`) {
		t.Fatalf("configure response did not use the public metadata contract: %s", body)
	}

	settings := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(settings, httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(`{"Name":"private"}`)))
	if settings.Code != http.StatusOK || strings.Contains(settings.Body.String(), "api-secret") || !strings.Contains(settings.Body.String(), `"environmentKeys":["TOKEN"]`) {
		t.Fatalf("settings status=%d body=%s", settings.Code, settings.Body.String())
	}

	duplicate := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(duplicate, httptest.NewRequest(http.MethodPost, "/configure", strings.NewReader(
		`{"Name":"other","Domains":["app.example.com"]}`,
	)))
	if duplicate.Code != http.StatusBadRequest {
		t.Fatalf("removed custom domain field status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "data", "environment.key")); err != nil {
		t.Fatal(err)
	}

	apps := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(apps, httptest.NewRequest(http.MethodPost, "/apps", strings.NewReader(`{}`)))
	if apps.Code != http.StatusOK || !strings.Contains(apps.Body.String(), `"Production":null`) || !strings.Contains(apps.Body.String(), `"LatestAttempt":null`) {
		t.Fatalf("apps response did not include production/latest-attempt metadata: status=%d body=%s", apps.Code, apps.Body.String())
	}
	diagnostics := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(diagnostics, httptest.NewRequest(http.MethodPost, "/diagnostics", strings.NewReader(`{"Name":"private"}`)))
	if diagnostics.Code != http.StatusOK || strings.Contains(diagnostics.Body.String(), "api-secret") || !strings.Contains(diagnostics.Body.String(), `"latestAttempt":null`) {
		t.Fatalf("diagnostics route status=%d body=%s", diagnostics.Code, diagnostics.Body.String())
	}
}

func TestSettingsAndConfigureRoutesRequirePOST(t *testing.T) {
	server := NewPlatformServer("", nil)
	for _, route := range []string{"settings", "configure"} {
		recorder := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/"+route, nil))
		if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "POST" {
			t.Fatalf("GET /%s status=%d allow=%q", route, recorder.Code, recorder.Header().Get("Allow"))
		}
	}
}

func TestPlatformDeployAcceptsGitHubFieldsAndRejectsMixedSources(t *testing.T) {
	server := NewPlatformServer("", nil)
	valid := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(valid, httptest.NewRequest(http.MethodPost, "/deploy", strings.NewReader(
		`{"Name":"demo","RepoURL":"https://github.com/example/app.git","Ref":"main","RootDir":"web"}`,
	)))
	if valid.Code != http.StatusOK {
		t.Fatalf("GitHub deploy request status=%d body=%q", valid.Code, valid.Body.String())
	}

	for _, body := range []string{
		`{"SourceDir":"./app","RepoURL":"https://github.com/example/app"}`,
		`{"Ref":"main"}`,
		`{"RootDir":"web"}`,
		`{}`,
	} {
		recorder := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/deploy", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid deploy request %q status=%d body=%q", body, recorder.Code, recorder.Body.String())
		}
	}
}

type failedResponseWriter struct {
	*httptest.ResponseRecorder
}

func (failedResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func TestPlatformLogsResponseWriteFailure(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	server := NewPlatformServer("", nil)
	writer := failedResponseWriter{httptest.NewRecorder()}
	server.http.Handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/rollback", nil))
	if !strings.Contains(output.String(), "write rollback response: client disconnected") {
		t.Fatalf("response write failure lost: %q", output.String())
	}
}

func TestPlatformMethodsAndPing(t *testing.T) {
	server := NewPlatformServer("", nil)
	for _, command := range []string{"deploy", "apps", "inspect", "logs", "stop", "add", "link", "services", "capacity"} {
		recorder := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/"+command, nil))
		if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "POST" {
			t.Fatalf("%s status=%d", command, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "pong\n" {
		t.Fatalf("ping status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestPlatformCapacitySnapshotAndOptionalName(t *testing.T) {
	server := NewPlatformServer("", nil)
	for _, test := range []struct {
		body       string
		wantStatus int
	}{
		{body: `{}`, wantStatus: http.StatusOK},
		{body: `{"Name":"demo"}`, wantStatus: http.StatusNotFound},
		{body: `{"Name":"invalid-"}`, wantStatus: http.StatusBadRequest},
	} {
		recorder := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/capacity", strings.NewReader(test.body)))
		if recorder.Code != test.wantStatus {
			t.Fatalf("capacity request %s status=%d body=%q, want %d", test.body, recorder.Code, recorder.Body.String(), test.wantStatus)
		}
		if test.wantStatus != http.StatusOK {
			continue
		}
		var response Response
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Capacity == nil || response.Capacity.SampledAt.IsZero() || response.Capacity.Platform.Name == "" {
			t.Fatalf("capacity snapshot missing sample or platform: %+v", response.Capacity)
		}
	}
}
