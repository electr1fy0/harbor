package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	ui "paas/web"
)

func nativeTestAPI(t *testing.T) (http.Handler, *[]string) {
	t.Helper()
	calls := []string{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fn := strings.TrimPrefix(r.URL.Path, apiPrefix)
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		calls = append(calls, fn+":"+str(input, "action"))
		var data any = map[string]any{}
		switch fn {
		case "getOverview":
			data = map[string]any{"connected": true, "apps": []any{map[string]any{"Application": map[string]any{"Name": "demo", "CreatedAt": "2026-10-15T00:00:00Z"}, "Production": map[string]any{"State": "RUNNING"}, "LatestAttempt": map[string]any{"State": "BUILD_FAILED"}}}}
		case "getApp":
			data = map[string]any{"diagnostics": map[string]any{"summary": "Production is healthy", "production": map[string]any{"ID": "dep_live", "State": "RUNNING"}, "latestAttempt": map[string]any{"ID": "dep_bad", "State": "BUILD_FAILED"}, "excerpt": "safe <script>log</script>"}, "settings": map[string]any{"environmentKeys": []string{"TOKEN"}, "memoryLimit": "128M", "cpuQuota": 1, "tasksLimit": 64}, "releases": []any{map[string]any{"id": "release_1", "runtime": "go", "artifactAvailable": true}}, "url": "http://demo.localhost:8080"}
		case "operations":
			switch str(input, "action") {
			case "settings":
				data = map[string]any{"operations": map[string]any{"favorite": true, "tags": []string{"test"}, "healthStatus": "healthy"}}
			case "history":
				data = map[string]any{"deployments": []any{map[string]any{"deployment": map[string]any{"id": "dep_bad", "state": "BUILD_FAILED"}}}, "hasMore": true}
			case "events":
				data = map[string]any{"events": []any{map[string]any{"type": "BUILD_FAILED", "payload": "redacted event"}}, "hasMore": true}
			}
		case "automation":
			switch str(input, "action") {
			case "getSource":
				data = map[string]any{"source": map[string]any{"repoURL": "https://github.com/example/repo", "ref": "main", "selection": map[string]any{"runtime": "go"}}, "buildEnvironmentKeys": []string{"BUILD_TOKEN"}}
			case "listJobs":
				data = map[string]any{"jobs": []any{map[string]any{"id": "job_abc", "name": "demo", "state": "RUNNING", "stage": "BUILDING"}}}
			case "listHooks":
				data = map[string]any{"hooks": []any{map[string]any{"id": "hook_1", "branch": "main"}}}
			}
		case "dataServices":
			data = map[string]any{"services": []any{map[string]any{"name": "db", "state": "READY", "backups": []any{map[string]any{"id": "backup_1", "verification": "verified"}}}}}
		case "getCapacity":
			data = map[string]any{"host": map[string]any{"memoryAvailableBytes": 1048576}, "apps": []any{map[string]any{"name": "demo", "state": "RUNNING"}}}
		case "getGitHubConnection":
			data = map[string]any{"connected": true, "login": "octocat"}
		case "observe":
			data = map[string]any{"settings": map[string]any{"name": "demo", "cooldownSeconds": 300}, "history": []any{map[string]any{"at": "2026-10-15T00:00:00Z", "snapshot": map[string]any{"apps": []any{map[string]any{"name": "demo", "memoryCurrentBytes": 100}}}}}}
		}
		writeJSON(w, 200, data)
	}), &calls
}

func TestNativePagesRenderDataAndKeepReadOperationsReadOnly(t *testing.T) {
	api, calls := nativeTestAPI(t)
	handler, err := NewHandler(api, ui.Assets, testConfig(ModeLocal))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, want string }{
		{"/", "★ demo"}, {"/?q=missing", "No matching projects"}, {"/new", "Inspect repository"}, {"/deployments", "job_abc"}, {"/capacity", "1.0 MiB"}, {"/services", "db"}, {"/services?view=backups", "backup_1"}, {"/settings", "Connected as octocat"},
		{"/projects/demo", "Production is healthy"}, {"/projects/demo?tab=deployments&deployment=dep_bad", "redacted event"}, {"/projects/demo?tab=logs", "Live logs"}, {"/projects/demo?tab=settings", "TOKEN"}, {"/projects/demo?tab=operations", "Crash recovery"}, {"/projects/demo?tab=automation", "BUILD_TOKEN"}, {"/projects/demo?tab=observability", "Memory history"}, {"/projects/demo?tab=services", "Restore to a new project"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got := request(handler, "GET", "http://localhost:3000"+tc.path, "localhost:3000", "127.0.0.1:1234", nil, "")
			if got.Code != 200 || !strings.Contains(got.Body.String(), tc.want) {
				t.Fatalf("status=%d, want %q in %s", got.Code, tc.want, got.Body.String())
			}
			if strings.Contains(got.Body.String(), "<script>log</script>") {
				t.Fatal("logs were rendered as executable HTML")
			}
		})
	}
	allowed := map[string]bool{"getOverview:": true, "getApp:": true, "getGitHubConnection:": true, "getCapacity:": true, "operations:settings": true, "operations:history": true, "operations:events": true, "automation:getSource": true, "automation:listHooks": true, "automation:listJobs": true, "dataServices:list": true, "observe:get": true}
	for _, call := range *calls {
		if !allowed[call] {
			t.Errorf("page render performed a non-read operation: %s", call)
		}
	}
}

func TestNativePageAuthPrecedesDataLoading(t *testing.T) {
	api, calls := nativeTestAPI(t)
	handler, err := NewHandler(api, ui.Assets, testConfig(ModePublic))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/projects/demo", "/settings", "/new", "/login"} {
		got := request(handler, "GET", "https://dashboard.example.test"+path, "dashboard.example.test", "203.0.113.1:1234", nil, "")
		want := 302
		if path == "/login" {
			want = 200
		}
		if got.Code != want {
			t.Errorf("%s: status=%d want=%d", path, got.Code, want)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("unauthenticated rendering accessed API: %v", *calls)
	}
}

func TestNativePagesRejectUnknownProjectAndSection(t *testing.T) {
	api, _ := nativeTestAPI(t)
	handler, err := NewHandler(api, ui.Assets, testConfig(ModeLocal))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/projects/app.name", "/projects/demo?tab=unknown"} {
		got := request(handler, "GET", "http://localhost:3000"+path, "localhost:3000", "127.0.0.1:1234", nil, "")
		if got.Code != 404 {
			t.Fatalf("%s: status=%d", path, got.Code)
		}
	}
}

func TestNativeAssetVersionsMatchCachePolicy(t *testing.T) {
	api, _ := nativeTestAPI(t)
	handler, err := NewHandler(api, ui.Assets, testConfig(ModeLocal))
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "?v=stale", "?v=" + assetVersion(ui.Assets, "assets/app.js")} {
		got := request(handler, "GET", "http://localhost:3000/assets/app.js"+suffix, "localhost:3000", "127.0.0.1:1234", nil, "")
		want := "no-cache"
		if suffix == "?v="+assetVersion(ui.Assets, "assets/app.js") {
			want = "public, max-age=31536000, immutable"
		}
		if got.Code != 200 || got.Header().Get("Cache-Control") != want {
			t.Fatalf("%s: status=%d cache=%s", suffix, got.Code, got.Header().Get("Cache-Control"))
		}
	}
}

func TestNativeLogViewerSelectsAConcreteDeploymentByDefault(t *testing.T) {
	api, _ := nativeTestAPI(t)
	handler, err := NewHandler(api, ui.Assets, testConfig(ModeLocal))
	if err != nil {
		t.Fatal(err)
	}
	got := request(handler, "GET", "http://localhost:3000/projects/demo?tab=logs", "localhost:3000", "127.0.0.1:1234", nil, "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), `value="dep_bad" selected`) {
		t.Fatalf("log viewer must select an existing attempt, since empty IDs cannot be read: %s", got.Body.String())
	}
}

func TestGitHubDisconnectRequiresAnActiveConnection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		connected bool
		status    int
	}{
		{name: "connected", connected: true, status: http.StatusOK},
		{name: "disconnected", status: http.StatusOK},
		{name: "connection unavailable", status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != apiPrefix+"getGitHubConnection" {
					t.Errorf("unexpected API call: %s", r.URL.Path)
				}
				writeJSON(w, tc.status, map[string]any{"connected": tc.connected, "login": "octocat"})
			})
			handler, err := NewHandler(api, ui.Assets, testConfig(ModeLocal))
			if err != nil {
				t.Fatal(err)
			}
			got := request(handler, http.MethodGet, "http://localhost:3000/settings", "localhost:3000", "127.0.0.1:1234", nil, "")
			hasDisconnect := strings.Contains(got.Body.String(), `data-api="disconnectGitHub"`)
			if hasDisconnect != tc.connected {
				t.Fatalf("disconnect action shown=%v, connected=%v", hasDisconnect, tc.connected)
			}
		})
	}
}

func TestProjectControlsFollowServingStateAndSavedSource(t *testing.T) {
	api, _ := nativeTestAPI(t)
	withoutSource := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == apiPrefix+"automation" {
			var input map[string]any
			_ = json.NewDecoder(r.Body).Decode(&input)
			if input["action"] == "getSource" {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "No saved source."})
				return
			}
		}
		api.ServeHTTP(w, r)
	})
	handler, err := NewHandler(withoutSource, ui.Assets, testConfig(ModeLocal))
	if err != nil {
		t.Fatal(err)
	}
	got := request(handler, "GET", "http://localhost:3000/projects/demo", "localhost:3000", "127.0.0.1:1234", nil, "")
	html := got.Body.String()
	if got.Code != 200 || !strings.Contains(html, `data-action="restart"`) || !strings.Contains(html, `data-action="stop"`) {
		t.Fatal("running project lost its runtime controls")
	}
	if strings.Contains(html, `data-action="start"`) || strings.Contains(html, `data-api="retryGitHubDeployment"`) {
		t.Fatal("project offered a start or source deployment that cannot be used")
	}
}

func TestServicesViewsKeepOnlyRelevantActions(t *testing.T) {
	api, _ := nativeTestAPI(t)
	handler, err := NewHandler(api, ui.Assets, testConfig(ModeLocal))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ query, want, absent string }{
		{"", `data-action="create"`, `data-action="export"`},
		{"?view=backups", `data-action="backup"`, `data-action="create"`},
		{"?view=export", `data-action="export"`, `data-action="link"`},
	} {
		got := request(handler, "GET", "http://localhost:3000/services"+tc.query, "localhost:3000", "127.0.0.1:1234", nil, "")
		html := got.Body.String()
		if got.Code != 200 || !strings.Contains(html, tc.want) || strings.Contains(html, tc.absent) {
			t.Fatalf("services%s mixes unrelated actions", tc.query)
		}
	}
}
