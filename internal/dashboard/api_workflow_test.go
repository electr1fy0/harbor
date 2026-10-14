package dashboard

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const workflowJobID = "job_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestAPIOnboardingInspectionMapsDaemonResponse(t *testing.T) {
	var gotPath, gotMethod string
	var gotInput map[string]any
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotInput); err != nil {
			t.Errorf("decode inspection request: %v", err)
		}
		dashboardJSON(w, http.StatusOK, map[string]any{"inspection": map[string]any{
			"existingConfig": true,
			"config": map[string]any{
				"runtime": "go", "buildCommand": "go build ./...", "startCommand": "/app/server",
				"healthPath": "/health", "memoryLimit": "256Mi", "cpuQuota": 1.5, "tasksLimit": 32,
			},
			"environmentKeys": []string{"DATABASE_URL", "API_TOKEN", "not valid"},
			"warnings":        []string{".env.example is too large or is not a regular file; environment names were skipped"},
		}})
	})

	response := dashboardRequest(NewAPI(control), http.MethodPost, "inspectProject", `{"name":"demo","repoURL":"https://github.com/octo/demo.git","ref":"feature/test","rootDir":"packages/server","config":{"runtime":"go","buildCommand":"go build ./...","cpuQuota":1.5}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if gotPath != "/projects/inspect" || gotMethod != http.MethodPost {
		t.Fatalf("daemon request = %s %s, want POST /projects/inspect", gotMethod, gotPath)
	}
	if gotInput["repoURL"] != "https://github.com/octo/demo.git" || gotInput["name"] != "demo" || gotInput["ref"] != "feature/test" || gotInput["rootDir"] != "packages/server" {
		t.Fatalf("inspection input was not forwarded: %#v", gotInput)
	}
	gotConfig, ok := gotInput["config"].(map[string]any)
	if !ok || gotConfig["runtime"] != "go" || gotConfig["buildCommand"] != "go build ./..." || gotConfig["cpuQuota"] != 1.5 {
		t.Fatalf("inspection config was not forwarded: %#v", gotInput["config"])
	}
	result := decodeDashboardResponse(t, response.Body.Bytes())
	if result["existingConfig"] != true || result["environmentKeys"].([]any)[0] != "DATABASE_URL" {
		t.Fatalf("inspection fields were not mapped: %#v", result)
	}
	if len(result["environmentKeys"].([]any)) != 2 || result["warnings"].([]any)[0] != ".env.example is too large or is not a regular file; environment names were skipped" {
		t.Fatalf("inspection projection did not filter/retain safe fields: %#v", result)
	}
}

func TestAPISubmitsOnboardingJobAndProjectsSafeJob(t *testing.T) {
	var gotPath, gotMethod string
	var gotInput map[string]any
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		if err := json.NewDecoder(r.Body).Decode(&gotInput); err != nil {
			t.Errorf("decode deployment request: %v", err)
		}
		dashboardJSON(w, http.StatusAccepted, map[string]any{"job": workflowJob("QUEUED", "QUEUED")})
	})

	response := dashboardRequest(NewAPI(control), http.MethodPost, "submitOnboardingJob", `{"name":"demo","repoURL":"https://github.com/octo/demo","ref":"main","rootDir":"services/api","config":{"runtime":"go","startCommand":"/app/server"},"environment":{"API_TOKEN":"sensitive-value"},"sqliteService":"primary-db","createSqlite":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if gotPath != "/jobs/deploy" || gotMethod != http.MethodPost {
		t.Fatalf("daemon request = %s %s, want POST /jobs/deploy", gotMethod, gotPath)
	}
	if gotInput["name"] != "demo" || gotInput["repoURL"] != "https://github.com/octo/demo" || gotInput["ref"] != "main" || gotInput["rootDir"] != "services/api" || gotInput["sqliteService"] != "primary-db" || gotInput["createSqlite"] != true {
		t.Fatalf("deployment request was not forwarded: %#v", gotInput)
	}
	if !reflect.DeepEqual(gotInput["environment"], map[string]any{"API_TOKEN": "sensitive-value"}) {
		t.Fatalf("environment was not forwarded to daemon: %#v", gotInput["environment"])
	}
	if config, ok := gotInput["config"].(map[string]any); !ok || config["runtime"] != "go" || config["startCommand"] != "/app/server" {
		t.Fatalf("config was not forwarded to daemon: %#v", gotInput["config"])
	}
	assertMappedWorkflowJob(t, response.Body.Bytes(), "QUEUED", "QUEUED")
	if strings.Contains(response.Body.String(), "private-build-path") || strings.Contains(response.Body.String(), "ghp_") || strings.Contains(response.Body.String(), "PID") {
		t.Fatalf("job response exposed daemon internals: %s", response.Body)
	}
}

func TestAPIRedeployAndJobReadCancelRoutes(t *testing.T) {
	t.Run("redeploy", func(t *testing.T) {
		var gotPath, gotMethod string
		var gotInput map[string]any
		control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath, gotMethod = r.URL.Path, r.Method
			if err := json.NewDecoder(r.Body).Decode(&gotInput); err != nil {
				t.Errorf("decode redeploy request: %v", err)
			}
			dashboardJSON(w, http.StatusAccepted, map[string]any{"job": workflowJob("QUEUED", "QUEUED")})
		})
		response := dashboardRequest(NewAPI(control), http.MethodPost, "retryGitHubDeployment", `{"name":"demo"}`)
		if response.Code != http.StatusOK || gotPath != "/jobs/redeploy" || gotMethod != http.MethodPost || gotInput["name"] != "demo" {
			t.Fatalf("redeploy status=%d request=%s %s payload=%#v body=%s", response.Code, gotMethod, gotPath, gotInput, response.Body)
		}
		assertMappedWorkflowJob(t, response.Body.Bytes(), "QUEUED", "QUEUED")
	})

	for _, test := range []struct {
		name, function, method, state, stage string
	}{
		{"get", "getOnboardingJob", http.MethodGet, "RUNNING", "BUILDING"},
		{"cancel", "cancelOnboardingJob", http.MethodDelete, "CANCELLING", "BUILDING"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gotPath, gotMethod string
			control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotMethod = r.URL.Path, r.Method
				dashboardJSON(w, http.StatusOK, map[string]any{"job": workflowJob(test.state, test.stage)})
			})
			response := dashboardRequest(NewAPI(control), http.MethodPost, test.function, `{"id":"`+workflowJobID+`"}`)
			if response.Code != http.StatusOK || gotPath != "/jobs/"+workflowJobID || gotMethod != test.method {
				t.Fatalf("status=%d daemon request=%s %s body=%s", response.Code, gotMethod, gotPath, response.Body)
			}
			assertMappedWorkflowJob(t, response.Body.Bytes(), test.state, test.stage)
		})
	}
}

func TestAPIOnboardingQueueFullAndMissingJobStatuses(t *testing.T) {
	t.Run("queue full preserves 429", func(t *testing.T) {
		control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/jobs/deploy" {
				http.NotFound(w, r)
				return
			}
			dashboardJSON(w, http.StatusTooManyRequests, map[string]string{"error": "deployment queue is full"})
		})
		response := dashboardRequest(NewAPI(control), http.MethodPost, "submitOnboardingJob", `{"name":"demo","repoURL":"https://github.com/octo/demo"}`)
		if response.Code != http.StatusTooManyRequests {
			t.Fatalf("queue-full status=%d want=%d body=%s", response.Code, http.StatusTooManyRequests, response.Body)
		}
		if got := decodeDashboardResponse(t, response.Body.Bytes())["error"]; got != "deployment queue is full" {
			t.Fatalf("queue-full error=%v", got)
		}
	})

	for _, test := range []struct{ function, method string }{
		{"getOnboardingJob", http.MethodGet},
		{"cancelOnboardingJob", http.MethodDelete},
	} {
		t.Run(test.function+" not found", func(t *testing.T) {
			control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != test.method || r.URL.Path != "/jobs/"+workflowJobID {
					http.NotFound(w, r)
					return
				}
				dashboardJSON(w, http.StatusNotFound, map[string]string{"error": "deployment job was not found"})
			})
			response := dashboardRequest(NewAPI(control), http.MethodPost, test.function, `{"id":"`+workflowJobID+`"}`)
			if response.Code != http.StatusNotFound {
				t.Fatalf("not-found status=%d want=%d body=%s", response.Code, http.StatusNotFound, response.Body)
			}
			if got := decodeDashboardResponse(t, response.Body.Bytes())["error"]; got != "Harbor resource was not found." {
				t.Fatalf("not-found error=%v", got)
			}
		})
	}
}

func TestAPIListsPublicGitHubRepositoriesForOwner(t *testing.T) {
	var calls []string
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/github/connection":
			dashboardJSON(w, http.StatusOK, map[string]any{"connection": map[string]any{"connected": false}})
		case "/github/repos":
			if r.URL.Query().Get("owner") != "octo" {
				t.Errorf("public browse owner = %q, want octo", r.URL.Query().Get("owner"))
			}
			dashboardJSON(w, http.StatusOK, map[string]any{"repositories": []any{
				workflowRepo("octo", "demo"), workflowRepo("OCTO", "tools"), workflowRepo("another-owner", "hidden"),
			}})
		default:
			http.NotFound(w, r)
		}
	})

	response := dashboardRequest(NewAPI(control), http.MethodPost, "listGitHubRepos", `{"owner":"octo"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if !reflect.DeepEqual(calls, []string{"GET /github/connection", "GET /github/repos?owner=octo"}) {
		t.Fatalf("daemon browse calls=%v", calls)
	}
	var repositories []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &repositories); err != nil {
		t.Fatalf("decode repository list: %v body=%s", err, response.Body)
	}
	if len(repositories) != 2 || repositories[0]["fullName"] != "octo/demo" || repositories[1]["fullName"] != "OCTO/tools" {
		t.Fatalf("unexpected public repositories: %#v", repositories)
	}
	for _, repository := range repositories {
		fullName, _ := repository["fullName"].(string)
		if repository["private"] != false || repository["url"] != "https://github.com/"+fullName {
			t.Fatalf("unsafe public repository projection: %#v", repository)
		}
	}
}

func workflowJob(state, stage string) map[string]any {
	return map[string]any{
		"id": workflowJobID, "name": "demo", "state": state, "stage": stage,
		"createdAt": "2026-10-01T10:00:00Z", "updatedAt": "2026-10-01T10:01:00Z",
		"deployment": map[string]any{
			"ID": "deploy-1", "AppName": "demo", "ReleaseID": "release-1", "Port": 8080,
			"State": "RUNNING", "MemoryLimit": "256Mi", "CPUQuota": 1, "TasksLimit": 64,
			"PID": 4567, "LogPath": "/private-build-path/app.log", "SystemdUnit": "private-unit.service",
			"CreatedAt": "2026-10-01T10:00:00Z", "StartedAt": "2026-10-01T10:00:30Z",
		},
		"error": "failed at /Users/host/build with ghp_12345678901234567890",
		"logs":  []string{"building", "/var/lib/paas/logs/private.log"},
	}
}

func workflowRepo(owner, name string) map[string]any {
	return map[string]any{
		"fullName": owner + "/" + name, "name": name, "defaultBranch": "main",
		"description": "Example repository", "language": "Go", "private": false,
		"updatedAt": "2026-10-01T10:00:00Z", "branches": []any{},
	}
}

func assertMappedWorkflowJob(t *testing.T, body []byte, state, stage string) {
	t.Helper()
	job := decodeDashboardResponse(t, body)
	if job["id"] != workflowJobID || job["name"] != "demo" || job["state"] != state || job["stage"] != stage {
		t.Fatalf("unexpected mapped job fields: %#v", job)
	}
	logs, ok := job["logs"].([]any)
	if !ok || len(logs) != 2 || strings.Contains(logs[1].(string), "/var/lib/paas/") {
		t.Fatalf("job logs were not projected safely: %#v", job["logs"])
	}
	if strings.Contains(job["error"].(string), "/Users/") || strings.Contains(job["error"].(string), "ghp_") {
		t.Fatalf("job error was not sanitized: %#v", job["error"])
	}
	if strings.Contains(string(body), "PID") || strings.Contains(string(body), "private-build-path") || strings.Contains(string(body), "private-unit") {
		t.Fatalf("job deployment internals leaked: %s", body)
	}
}
