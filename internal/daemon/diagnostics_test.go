package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"paas/internal/deployment"
	"paas/internal/router"
	"paas/internal/store"
)

func TestDiagnosticsAPIIsReadOnlyAndReturnsMetadataOnly(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service, err := deployment.New(db, nil, nil, router.New(), deployment.Options{DataDir: filepath.Join(root, "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	secret := "diagnostic-api-secret"
	if _, err := service.Configure(context.Background(), "demo", deployment.ConfigureOptions{Environment: map[string]string{"TOKEN": secret}}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerDiagnosticsRoute(mux, service, func(name string) bool { return name == "demo" })

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/diagnostics", strings.NewReader(`{"Name":"demo"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("POST /diagnostics status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatalf("diagnostics API leaked an environment value: %s", response.Body.String())
	}
	var body struct {
		Diagnostics struct {
			Production    json.RawMessage `json:"production"`
			LatestAttempt json.RawMessage `json:"latestAttempt"`
			Summary       string          `json:"summary"`
			Actions       []struct {
				ID        string `json:"id"`
				Available bool   `json:"available"`
			} `json:"actions"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if string(body.Diagnostics.Production) != "null" || string(body.Diagnostics.LatestAttempt) != "null" || body.Diagnostics.Summary != "No deployment attempt has been recorded." {
		t.Fatalf("diagnostics metadata = %+v", body.Diagnostics)
	}
	availableRetry := false
	for _, action := range body.Diagnostics.Actions {
		if action.ID == "retry_github_deploy" {
			availableRetry = action.Available
		}
	}
	if !availableRetry {
		t.Fatalf("diagnostics did not use persisted-source availability callback: %+v", body.Diagnostics.Actions)
	}

	get := httptest.NewRecorder()
	mux.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/diagnostics", nil))
	if get.Code != http.StatusMethodNotAllowed || get.Header().Get("Allow") != "POST" {
		t.Fatalf("GET /diagnostics status=%d allow=%q", get.Code, get.Header().Get("Allow"))
	}
	badName := httptest.NewRecorder()
	mux.ServeHTTP(badName, httptest.NewRequest(http.MethodPost, "/diagnostics", strings.NewReader(`{"Name":"missing"}`)))
	if badName.Code != http.StatusNotFound || strings.Contains(badName.Body.String(), secret) {
		t.Fatalf("unknown app diagnostics status=%d body=%s", badName.Code, badName.Body.String())
	}
}

func TestDiagnosticsAPIRejectsMalformedAndUnknownRequests(t *testing.T) {
	mux := http.NewServeMux()
	registerDiagnosticsRoute(mux, nil, nil)
	for _, body := range []string{"", "null", `{"Name":"demo","Unknown":true}`, `{"Name":"demo"} {}`} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/diagnostics", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("request %q status=%d body=%q", body, recorder.Code, recorder.Body.String())
		}
	}
}
