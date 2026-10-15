package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"paas/internal/daemon"
)

func dashboardRequest(handler http.Handler, method, function, input string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, apiPrefix+function, strings.NewReader(input))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func dashboardJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeDashboardResponse(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode dashboard response: %v; body=%s", err, body)
	}
	return value
}

func TestAPIProjectsSafeAppResponse(t *testing.T) {
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ping":
			_, _ = io.WriteString(w, "pong\n")
		case "/inspect":
			dashboardJSON(w, http.StatusOK, map[string]any{
				"URL": "http://demo.localhost:8080",
				"Deployment": map[string]any{
					"ID": "deploy-current", "AppID": "app-1", "AppName": "demo", "ReleaseID": "release-1",
					"Port": 8080, "State": "RUNNING", "Error": "/srv/private/raw deployment error", "PID": 4321,
					"LogPath": "/var/lib/paas/logs/private.log", "SystemdUnit": "harbor-secret.service", "SandboxID": "private-id",
					"MemoryLimit": "256Mi", "CPUQuota": 1, "TasksLimit": 64, "ReadinessKnown": true,
					"HealthPath": "/health", "CreatedAt": "2026-10-01T10:00:00Z", "PromotedAt": "2026-10-01T10:01:00Z",
					"StartedAt": "2026-10-01T10:00:30Z", "StoppedAt": nil,
				},
			})
		case "/releases":
			dashboardJSON(w, http.StatusOK, map[string]any{"Releases": []any{map[string]any{
				"id": "release-1", "app_id": "app-1", "app_name": "demo", "source_hash": "abcdef", "runtime": "go",
				"artifact_dir": "/var/lib/paas/artifacts/secret", "created_at": "2026-10-01T10:00:00Z", "is_active": true,
			}}})
		case "/settings":
			dashboardJSON(w, http.StatusOK, map[string]any{"settings": map[string]any{
				"environmentKeys": []string{"DATABASE_URL", "TOKEN"},
				"memoryLimit":     "256Mi", "cpuQuota": 1, "tasksLimit": 64,
				"environment": map[string]string{"TOKEN": "never return this"},
			}})
		case "/diagnostics":
			dashboardJSON(w, http.StatusOK, map[string]any{"diagnostics": map[string]any{
				"production":    map[string]any{"ID": "deploy-current", "AppName": "demo", "State": "RUNNING", "CreatedAt": "2026-10-01T10:00:00Z"},
				"latestAttempt": map[string]any{"ID": "deploy-current", "AppName": "demo", "State": "RUNNING", "CreatedAt": "2026-10-01T10:00:00Z"},
				"summary":       "Serving production traffic", "excerpt": "from /Users/ayush/private.txt with ghp_12345678901234567890",
				"actions": []any{
					map[string]any{"id": "view_attempt_logs", "label": "untrusted label", "available": true, "reason": "open /home/harbor/x", "releaseID": "release-1"},
					map[string]any{"id": "arbitrary", "label": "leak"},
				},
			}})
		case "/logs":
			dashboardJSON(w, http.StatusOK, map[string]any{"Logs": "token=ghp_12345678901234567890\n/var/lib/paas/apps/demo.log"})
		default:
			http.NotFound(w, r)
		}
	})
	response := dashboardRequest(NewAPI(control), http.MethodPost, "getApp", `{"name":"demo"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	result := decodeDashboardResponse(t, response.Body.Bytes())
	if result["url"] != "http://demo.localhost:8080" {
		t.Fatalf("local production URL was not returned: %v", result["url"])
	}
	deployment, ok := result["deployment"].(map[string]any)
	if !ok || deployment["Error"] != "" || deployment["PID"] != nil || deployment["State"] != "RUNNING" {
		t.Fatalf("unexpected safe deployment projection: %#v", result["deployment"])
	}
	releases := result["releases"].([]any)
	if len(releases) != 1 || releases[0].(map[string]any)["artifact_dir"] != nil || releases[0].(map[string]any)["id"] != "release-1" {
		t.Fatalf("unexpected safe release projection: %#v", result["releases"])
	}
	diagnostics := result["diagnostics"].(map[string]any)
	if strings.Contains(diagnostics["excerpt"].(string), "/Users/") || strings.Contains(diagnostics["excerpt"].(string), "ghp_") {
		t.Fatalf("diagnostic excerpt was not sanitized: %#v", diagnostics["excerpt"])
	}
	actions := diagnostics["actions"].([]any)
	if len(actions) != 1 || actions[0].(map[string]any)["label"] != "View failed attempt logs" {
		t.Fatalf("diagnostic actions were not allowlisted: %#v", actions)
	}
	logs := result["logs"].(string)
	if strings.Contains(logs, "ghp_") || strings.Contains(logs, "/var/lib/paas/") {
		t.Fatalf("logs were not sanitized: %q", logs)
	}
	for _, secret := range []string{"/srv/private", "/var/lib/paas", "4321", "harbor-secret", "never return this", "artifact_dir"} {
		if bytes.Contains(response.Body.Bytes(), []byte(secret)) {
			t.Fatalf("response exposed internal value %q: %s", secret, response.Body)
		}
	}
}

func TestAPIConfigureValidationAndNormalization(t *testing.T) {
	var daemonInput map[string]any
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/configure" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&daemonInput); err != nil {
			t.Errorf("decode daemon input: %v", err)
		}
		dashboardJSON(w, http.StatusOK, map[string]any{"settings": map[string]any{
			"environmentKeys": []string{"TOKEN"},
			"memoryLimit":     daemonInput["MemoryLimit"], "cpuQuota": daemonInput["CPUQuota"], "tasksLimit": daemonInput["TasksLimit"],
		}})
	})
	response := dashboardRequest(NewAPI(control), http.MethodPost, "configureApp", `{"name":"demo","memoryLimit":" 512Mi ","cpuQuota":0.5,"environment":{"TOKEN":"value"}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if daemonInput["MemoryLimit"] != "512Mi" {
		t.Fatalf("configuration was not normalized before daemon call: %#v", daemonInput)
	}
	result := decodeDashboardResponse(t, response.Body.Bytes())
	if result["memoryLimit"] != "512Mi" {
		t.Fatalf("unexpected normalized settings: %#v", result)
	}
}

func TestAPIEndpointMethodAndBodyValidation(t *testing.T) {
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	api := NewAPI(control)
	cases := []struct {
		name, method, function, body string
		want                         int
	}{
		{"method", http.MethodGet, "getOverview", `{}`, http.StatusMethodNotAllowed},
		{"unknown function", http.MethodPost, "deleteEverything", `{}`, http.StatusNotFound},
		{"unknown property", http.MethodPost, "getApp", `{"name":"demo","extra":true}`, http.StatusBadRequest},
		{"invalid name", http.MethodPost, "getApp", `{"name":"../etc"}`, http.StatusBadRequest},
		{"missing rollback id", http.MethodPost, "rollbackApp", `{"name":"demo"}`, http.StatusBadRequest},
		{"null environment value", http.MethodPost, "configureApp", `{"name":"demo","environment":{"TOKEN":null}}`, http.StatusBadRequest},
		{"malformed json", http.MethodPost, "getOverview", `{"broken"`, http.StatusBadRequest},
		{"json null", http.MethodPost, "getOverview", `null`, http.StatusBadRequest},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := dashboardRequest(api, test.method, test.function, test.body)
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.want, response.Body)
			}
			_ = decodeDashboardResponse(t, response.Body.Bytes())
		})
	}
	oversized := `{"name":"demo","environment":{"TOKEN":"` + strings.Repeat("x", maxDashboardBody) + `"}}`
	response := dashboardRequest(api, http.MethodPost, "configureApp", oversized)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status=%d want=%d body=%s", response.Code, http.StatusRequestEntityTooLarge, response.Body)
	}
}

func TestAPIGetAppReturnsNotFoundWhenNoAppResourcesExist(t *testing.T) {
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/inspect", "/releases", "/settings", "/diagnostics":
			dashboardJSON(w, http.StatusNotFound, map[string]string{"error": "Application not found."})
		default:
			http.NotFound(w, r)
		}
	})
	response := dashboardRequest(NewAPI(control), http.MethodPost, "getApp", `{"name":"missing"}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing app status=%d body=%s", response.Code, response.Body)
	}
	if got := decodeDashboardResponse(t, response.Body.Bytes())["error"]; got != "Harbor resource was not found." {
		t.Fatalf("missing app error=%v", got)
	}
}

func TestAPIMapsCapacityWithoutHostInternals(t *testing.T) {
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/capacity" {
			http.NotFound(w, r)
			return
		}
		dashboardJSON(w, http.StatusOK, map[string]any{"Capacity": map[string]any{
			"sampledAt": "2026-10-01T10:00:00Z",
			"platform":  map[string]any{"supported": true, "name": "linux", "reason": ""},
			"host":      map[string]any{"memoryTotalBytes": 1024, "pid": 1234},
			"disk":      map[string]any{"filesystemTotalBytes": 2048},
			"apps":      []any{map[string]any{"deploymentId": "d1", "name": "demo", "state": "RUNNING", "pid": 1234, "controlGroup": "/private/cgroup"}},
			"builds":    map[string]any{"memoryLimitBytes": 512, "cgroupPath": "/private/build"},
			"headroom":  map[string]any{"memoryReservePercent": 10},
			"caveats":   []string{"host metric"},
		}})
	})
	response := dashboardRequest(NewAPI(control), http.MethodPost, "getCapacity", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if bytes.Contains(response.Body.Bytes(), []byte("pid")) || bytes.Contains(response.Body.Bytes(), []byte("controlGroup")) || bytes.Contains(response.Body.Bytes(), []byte("cgroupPath")) {
		t.Fatalf("capacity projection exposed host internals: %s", response.Body)
	}
}

func TestAPIGitHubRepositoryDetailsIncludeSafeBranches(t *testing.T) {
	var repositoryRequestPath string
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/github/connection":
			dashboardJSON(w, http.StatusOK, map[string]any{"connection": map[string]any{"connected": true, "login": "octo", "token": "never"}})
		case "/github/repo":
			repositoryRequestPath = r.URL.RawQuery
			dashboardJSON(w, http.StatusOK, map[string]any{"repository": map[string]any{
				"fullName": "octo/demo", "name": "demo", "defaultBranch": "main", "description": "A demo", "language": "Go",
				"private": true, "updatedAt": "2026-10-01T10:00:00Z", "branches": []any{map[string]any{"name": "feature/x", "commitSha": "abc123"}},
			}})
		default:
			http.NotFound(w, r)
		}
	})
	response := dashboardRequest(NewAPI(control), http.MethodPost, "getGitHubRepo", `{"repoURL":" https://github.com/octo/demo.git "}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if repositoryRequestPath != "owner=octo&repo=demo" {
		t.Fatalf("unexpected daemon request query: %q", repositoryRequestPath)
	}
	result := decodeDashboardResponse(t, response.Body.Bytes())
	branches := result["branches"].([]any)
	if len(branches) != 1 || branches[0].(map[string]any)["commitSha"] != "abc123" {
		t.Fatalf("unexpected branch projection: %#v", branches)
	}
	if bytes.Contains(response.Body.Bytes(), []byte("never")) || result["url"] != "https://github.com/octo/demo" {
		t.Fatalf("unsafe repository projection: %s", response.Body)
	}
}

func TestAPISafeRouteURLProjection(t *testing.T) {
	deployment := map[string]any{"AppName": "demo", "State": "RUNNING"}
	if got := routeURL("http://demo.localhost:8080", deployment); got != "http://demo.localhost:8080" {
		t.Fatalf("safe local route was rejected: %q", got)
	}
	for _, candidate := range []string{
		"http://other.localhost:8080",
		"http://demo.localhost:8080/private",
		"http://user:pass@demo.localhost:8080",
		"https://unconfigured.example",
		"https://example.com",
		"http://demo.localhost:70000",
	} {
		if got := routeURL(candidate, deployment); got != "" {
			t.Errorf("unsafe route %q was exposed as %q", candidate, got)
		}
	}
}

func TestAPIUsesDaemonHandlerInProcess(t *testing.T) {
	control := daemon.NewPlatformServerWithServicesAndBuildUser("", nil, nil, "paas-build").Handler()
	response := dashboardRequest(NewAPI(control), http.MethodPost, "getOverview", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	result := decodeDashboardResponse(t, response.Body.Bytes())
	if result["connected"] != true || result["apps"].([]any) == nil || !strings.Contains(result["error"].(string), "Service inventory is unavailable.") {
		t.Fatalf("unexpected daemon-backed overview: %#v", result)
	}
}

func TestAPIAllowsEmptyOwnerForConnectedRepositoryList(t *testing.T) {
	var sawRepositoryList bool
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/github/connection":
			dashboardJSON(w, http.StatusOK, map[string]any{"connection": map[string]any{"connected": true}})
		case "/github/repos":
			sawRepositoryList = true
			dashboardJSON(w, http.StatusOK, map[string]any{"repositories": []any{}})
		default:
			http.NotFound(w, r)
		}
	})
	response := dashboardRequest(NewAPI(control), http.MethodPost, "listGitHubRepos", `{"owner":""}`)
	if response.Code != http.StatusOK || !sawRepositoryList || !strings.HasPrefix(strings.TrimSpace(response.Body.String()), "[") {
		t.Fatalf("connected empty-owner browse failed: status=%d called=%t body=%s", response.Code, sawRepositoryList, response.Body)
	}
}

func TestAPISanitizesOnboardingJobErrors(t *testing.T) {
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jobs/job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
			http.NotFound(w, r)
			return
		}
		dashboardJSON(w, http.StatusOK, map[string]any{"job": map[string]any{
			"id": "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "name": "demo", "state": "FAILED", "stage": "FAILED",
			"createdAt": "2026-10-01T10:00:00Z", "updatedAt": "2026-10-01T10:00:00Z",
			"error": "failed at /Users/ayush/build with token=ghp_12345678901234567890", "logs": []string{"/var/lib/paas/private.log"},
		}})
	})
	response := dashboardRequest(NewAPI(control), http.MethodPost, "getOnboardingJob", `{"id":"job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	for _, secret := range []string{"/Users/", "/var/lib/paas", "ghp_"} {
		if bytes.Contains(response.Body.Bytes(), []byte(secret)) {
			t.Fatalf("job projection exposed %q: %s", secret, response.Body)
		}
	}
}
