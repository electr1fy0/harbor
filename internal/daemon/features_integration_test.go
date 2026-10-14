package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paas/internal/dashboard"
	"paas/internal/deployment"
	"paas/internal/model"
	"paas/internal/router"
	managedservice "paas/internal/service"
	"paas/internal/store"

	"testing/fstest"
)

// featureGatewayFixture deliberately assembles the same in-process route stack
// as the web listener: the dashboard handler, API adapter, daemon mux, real
// deployment store, real managed-service manager, and host router.
type featureGatewayFixture struct {
	root       string
	state      *store.Store
	appService *deployment.Service
	services   *managedservice.Manager
	routes     *router.HostRouter
	server     *Server
	handler    http.Handler
}

func newFeatureGatewayFixture(t *testing.T) *featureGatewayFixture {
	t.Helper()
	root := t.TempDir()
	state, err := store.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	routes := router.New()
	appService, err := deployment.New(state, nil, nil, routes, deployment.Options{DataDir: filepath.Join(root, "data")})
	if err != nil {
		_ = state.Close()
		t.Fatal(err)
	}
	services := managedservice.NewManager(state, filepath.Join(root, "data"))
	server := NewPlatformServerWithServices("", appService, services)
	api := dashboard.NewAPI(server.http.Handler)
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>Harbor test</title>")}}
	handler, err := dashboard.NewHandlerWithHooks(api, assets, dashboard.Config{Mode: dashboard.ModeLocal}, server.PublicHooks())
	if err != nil {
		_ = server.Shutdown(context.Background())
		_ = appService.Close()
		_ = services.Close(context.Background())
		_ = state.Close()
		t.Fatal(err)
	}
	f := &featureGatewayFixture{root: root, state: state, appService: appService, services: services, routes: routes, server: server, handler: handler}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shut down feature gateway: %v", err)
		}
		if err := appService.Close(); err != nil {
			t.Errorf("close deployment service: %v", err)
		}
		if err := services.Close(ctx); err != nil {
			t.Errorf("close managed services: %v", err)
		}
		if err := state.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return f
}

func (f *featureGatewayFixture) dashboardCall(t *testing.T, function, body string) (int, []byte) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/dashboard/"+function, strings.NewReader(body))
	request.Host = "localhost"
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Origin", "http://localhost")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.Bytes()
}

func (f *featureGatewayFixture) publicHookCall(method, target string, body []byte, headers http.Header) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(string(body)))
	request.Host = "localhost"
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header = headers.Clone()
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeFeatureResponse(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode feature response %q: %v", body, err)
	}
	return result
}

func TestFeatureGatewaySettingsOperationsAndObservability(t *testing.T) {
	f := newFeatureGatewayFixture(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("backend response"))
	}))
	t.Cleanup(backend.Close)
	if err := f.routes.SetRoute("demo.localhost", backend.URL); err != nil {
		t.Fatal(err)
	}
	if err := f.routes.SetRoute("app.example.test", backend.URL); err != nil {
		t.Fatal(err)
	}

	status, body := f.dashboardCall(t, "configureApp", `{"name":"demo","environment":{"TOKEN":"runtime-secret"},"domains":["app.example.test"],"memoryLimit":"256Mi","cpuQuota":1.25,"tasksLimit":42}`)
	if status != http.StatusOK {
		t.Fatalf("configureApp status=%d body=%s", status, body)
	}
	if strings.Contains(string(body), "runtime-secret") || !strings.Contains(string(body), `"environmentKeys":["TOKEN"]`) || !strings.Contains(string(body), `"domains":["app.example.test"]`) {
		t.Fatalf("configureApp response leaked or omitted safe metadata: %s", body)
	}

	status, body = f.dashboardCall(t, "getAppSettings", `{"name":"demo"}`)
	if status != http.StatusOK || strings.Contains(string(body), "runtime-secret") || !strings.Contains(string(body), `"environmentKeys":["TOKEN"]`) {
		t.Fatalf("getAppSettings status=%d body=%s", status, body)
	}
	status, body = f.dashboardCall(t, "getOverview", `{}`)
	if status != http.StatusOK {
		t.Fatalf("getOverview pending app status=%d body=%s", status, body)
	}
	overview := decodeFeatureResponse(t, body)
	apps, ok := overview["apps"].([]any)
	if !ok || len(apps) != 1 {
		t.Fatalf("getOverview omitted the configured app: %s", body)
	}
	appView, ok := apps[0].(map[string]any)
	if !ok || appView["Production"] != nil || appView["LatestAttempt"] != nil {
		t.Fatalf("getOverview did not retain empty production/latest-attempt metadata: %s", body)
	}

	status, body = f.dashboardCall(t, "operations", `{"action":"configureMetadata","appName":"demo","favorite":true,"tags":["production","team-api"],"requiredEnvironmentKeys":["TOKEN"]}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"favorite":true`) || !strings.Contains(string(body), `"requiredEnvironmentKeys":["TOKEN"]`) {
		t.Fatalf("configureMetadata status=%d body=%s", status, body)
	}

	app, err := f.state.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(f.root, "demo-build.log")
	if err := os.WriteFile(logPath, []byte("configured TOKEN=runtime-secret\nfirst-line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.state.CreateDeployment(model.Deployment{
		ID: "dep_feature_logs", AppID: app.ID, AppName: app.Name, State: model.BuildFailed,
		Error: "build failed after TOKEN=runtime-secret", LogPath: logPath, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	status, body = f.dashboardCall(t, "operations", `{"action":"historyAll","limit":20,"offset":0}`)
	if status != http.StatusOK {
		t.Fatalf("historyAll status=%d body=%s", status, body)
	}
	history := decodeFeatureResponse(t, body)
	if history["total"] != float64(1) {
		t.Fatalf("historyAll did not include the persisted attempt: %s", body)
	}
	rows, ok := history["deployments"].([]any)
	if !ok || len(rows) != 1 || !strings.Contains(string(body), "dep_feature_logs") || strings.Contains(string(body), "runtime-secret") {
		t.Fatalf("historyAll deployment metadata is missing or leaked: %s", body)
	}

	status, body = f.dashboardCall(t, "observe", `{"action":"logs","name":"demo","deploymentId":"dep_feature_logs","source":"all"}`)
	if status != http.StatusOK {
		t.Fatalf("initial observe logs status=%d body=%s", status, body)
	}
	logs := decodeFeatureResponse(t, body)
	if strings.Contains(logs["text"].(string), "runtime-secret") || !logs["reset"].(bool) {
		t.Fatalf("initial logs are not redacted/reset: %s", body)
	}
	cursor := logs["cursor"].(string)
	status, body = f.dashboardCall(t, "observe", fmt.Sprintf(`{"action":"logs","name":"demo","deploymentId":"dep_feature_logs","source":"all","cursor":%q}`, cursor))
	if status != http.StatusOK {
		t.Fatalf("observe with current cursor status=%d body=%s", status, body)
	}
	logs = decodeFeatureResponse(t, body)
	if logs["text"] != "" || logs["reset"].(bool) {
		t.Fatalf("unchanged logs were not returned as an empty delta: %s", body)
	}
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("appended-line\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	cursor = logs["cursor"].(string)
	status, body = f.dashboardCall(t, "observe", fmt.Sprintf(`{"action":"logs","name":"demo","deploymentId":"dep_feature_logs","source":"all","cursor":%q}`, cursor))
	if status != http.StatusOK {
		t.Fatalf("observe appended logs status=%d body=%s", status, body)
	}
	logs = decodeFeatureResponse(t, body)
	if logs["text"] != "appended-line\n" || logs["reset"].(bool) {
		t.Fatalf("observe did not return an incremental log delta: %s", body)
	}
	cursor = logs["cursor"].(string)
	if err := os.WriteFile(logPath, []byte("rotated TOKEN=runtime-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	status, body = f.dashboardCall(t, "observe", fmt.Sprintf(`{"action":"logs","name":"demo","deploymentId":"dep_feature_logs","source":"all","cursor":%q}`, cursor))
	if status != http.StatusOK {
		t.Fatalf("observe rotated logs status=%d body=%s", status, body)
	}
	logs = decodeFeatureResponse(t, body)
	if !logs["reset"].(bool) || strings.Contains(logs["text"].(string), "runtime-secret") {
		t.Fatalf("rotated logs did not reset safely: %s", body)
	}
	status, body = f.dashboardCall(t, "observe", `{"action":"logs","name":"demo","deploymentId":"dep_feature_logs","source":"all","cursor":"bad cursor"}`)
	if status != http.StatusOK {
		t.Fatalf("observe malformed cursor status=%d body=%s", status, body)
	}
	logs = decodeFeatureResponse(t, body)
	if !logs["reset"].(bool) || strings.Contains(logs["text"].(string), "runtime-secret") {
		t.Fatalf("malformed cursor did not safely reset logs: %s", body)
	}

	observeSettings := `{"name":"demo","maintenance":false,"maintenanceMessage":"Scheduled maintenance","primaryDomain":"app.example.test","memoryPercent":0,"diskPercent":0,"cooldownSeconds":300,"events":[]}`
	status, body = f.dashboardCall(t, "observe", `{"action":"configure","name":"demo","settings":`+observeSettings+`}`)
	if status != http.StatusOK {
		t.Fatalf("observe configure redirect status=%d body=%s", status, body)
	}
	redirectRequest := httptest.NewRequest(http.MethodGet, "http://demo.localhost/docs/guide?lang=en", nil)
	redirectRequest.Host = "demo.localhost"
	redirect := httptest.NewRecorder()
	f.routes.ServeHTTP(redirect, redirectRequest)
	if redirect.Code != http.StatusPermanentRedirect || redirect.Header().Get("Location") != "https://app.example.test/docs/guide?lang=en" {
		t.Fatalf("primary-domain redirect status=%d location=%q", redirect.Code, redirect.Header().Get("Location"))
	}

	observeSettings = `{"name":"demo","maintenance":true,"maintenanceMessage":"Deploy in progress","primaryDomain":"app.example.test","memoryPercent":0,"diskPercent":0,"cooldownSeconds":300,"events":[]}`
	status, body = f.dashboardCall(t, "observe", `{"action":"configure","name":"demo","settings":`+observeSettings+`}`)
	if status != http.StatusOK {
		t.Fatalf("observe configure maintenance status=%d body=%s", status, body)
	}
	maintenanceRequest := httptest.NewRequest(http.MethodGet, "http://app.example.test/health", nil)
	maintenanceRequest.Host = "app.example.test"
	maintenance := httptest.NewRecorder()
	f.routes.ServeHTTP(maintenance, maintenanceRequest)
	if maintenance.Code != http.StatusServiceUnavailable || !strings.Contains(maintenance.Body.String(), "Deploy in progress") || maintenance.Header().Get("Retry-After") == "" {
		t.Fatalf("maintenance response status=%d retry-after=%q body=%q", maintenance.Code, maintenance.Header().Get("Retry-After"), maintenance.Body.String())
	}

	status, body = f.dashboardCall(t, "observe", `{"action":"get","name":"demo","limit":20,"window":"hour"}`)
	if status != http.StatusOK {
		t.Fatalf("observe get status=%d body=%s", status, body)
	}
	view := decodeFeatureResponse(t, body)
	traffic, ok := view["traffic"].([]any)
	if !ok || len(traffic) == 0 {
		t.Fatalf("observe did not return traffic collected by the live router: %s", body)
	}

	status, body = f.dashboardCall(t, "observe", `{"action":"forget","name":"demo"}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"forgotten":true`) {
		t.Fatalf("observe forget status=%d body=%s", status, body)
	}
	requestAfterForget := httptest.NewRequest(http.MethodGet, "http://app.example.test/health", nil)
	requestAfterForget.Host = "app.example.test"
	afterForget := httptest.NewRecorder()
	f.routes.ServeHTTP(afterForget, requestAfterForget)
	if afterForget.Code != http.StatusOK || afterForget.Body.String() != "backend response" {
		t.Fatalf("observe forget did not clear the live maintenance policy: status=%d body=%q", afterForget.Code, afterForget.Body.String())
	}
}

func TestFeatureGatewayDataServiceActions(t *testing.T) {
	f := newFeatureGatewayFixture(t)
	status, body := f.dashboardCall(t, "configureApp", `{"name":"data-app","environment":{"APP_MODE":"test"}}`)
	if status != http.StatusOK {
		t.Fatalf("configure app for data service status=%d body=%s", status, body)
	}

	status, body = f.dashboardCall(t, "dataServices", `{"action":"create","serviceType":"sqlite","name":"primary"}`)
	if status != http.StatusOK {
		t.Fatalf("create SQLite data service status=%d body=%s", status, body)
	}
	created := decodeFeatureResponse(t, body)
	service, ok := created["service"].(map[string]any)
	if !ok || service["name"] != "primary" || service["type"] != "sqlite" || service["state"] != string(model.ServiceReady) {
		t.Fatalf("create response omitted safe service metadata: %s", body)
	}

	status, body = f.dashboardCall(t, "dataServices", `{"action":"list"}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"name":"primary"`) {
		t.Fatalf("list services status=%d body=%s", status, body)
	}

	status, body = f.dashboardCall(t, "dataServices", `{"action":"link","appName":"data-app","serviceName":"primary"}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"restartRequired":true`) || !strings.Contains(string(body), `"appName":"data-app"`) {
		t.Fatalf("link service status=%d body=%s", status, body)
	}

	status, body = f.dashboardCall(t, "dataServices", `{"action":"backup","appName":"data-app","serviceName":"primary"}`)
	if status != http.StatusOK {
		t.Fatalf("create backup status=%d body=%s", status, body)
	}
	backupResult := decodeFeatureResponse(t, body)
	backup, ok := backupResult["backup"].(map[string]any)
	if !ok || backup["id"] == "" || backup["verification"] != "verified" {
		t.Fatalf("backup response is incomplete: %s", body)
	}
	backupID := backup["id"].(string)

	status, body = f.dashboardCall(t, "dataServices", fmt.Sprintf(`{"action":"verifyBackup","backupId":%q}`, backupID))
	if status != http.StatusOK || !strings.Contains(string(body), `"verification":"verified"`) {
		t.Fatalf("verify backup status=%d body=%s", status, body)
	}
	status, body = f.dashboardCall(t, "dataServices", fmt.Sprintf(`{"action":"downloadBackup","backupId":%q}`, backupID))
	if status != http.StatusOK || !strings.Contains(string(body), `"contentType":"application/vnd.sqlite3"`) || !strings.Contains(string(body), `"data":"`) {
		t.Fatalf("download backup status=%d body=%s", status, body)
	}

	status, body = f.dashboardCall(t, "dataServices", fmt.Sprintf(`{"action":"restoreBackup","backupId":%q,"newAppName":"data-recovered","serviceName":"primary"}`, backupID))
	if status != http.StatusOK || !strings.Contains(string(body), `"restoredAsNewApp":true`) || !strings.Contains(string(body), `"appName":"data-recovered"`) {
		t.Fatalf("restore backup status=%d body=%s", status, body)
	}

	status, body = f.dashboardCall(t, "dataServices", `{"action":"export","passphrase":"feature-test-passphrase-2026","appNames":["data-app","data-recovered"]}`)
	if status != http.StatusOK {
		t.Fatalf("encrypted export status=%d body=%s", status, body)
	}
	exportResult := decodeFeatureResponse(t, body)
	export, ok := exportResult["export"].(map[string]any)
	size, sizeOK := export["sizeBytes"].(float64)
	if !ok || export["encryptedData"] == "" || !sizeOK || size <= 0 || !strings.Contains(string(body), "harbor-export-") {
		t.Fatalf("encrypted export response is incomplete: %s", body)
	}
}

func TestFeatureGatewayAutomationSourceAndPublicHooks(t *testing.T) {
	f := newFeatureGatewayFixture(t)
	status, body := f.dashboardCall(t, "automation", `{"action":"updateSource","source":{"name":"auto-app","repoURL":"https://github.com/example/auto-app.git","ref":"main","rootDir":"services/api","config":{"runtime":"node","buildCommand":"npm run build","startCommand":"npm start","healthPath":"/healthz"},"includePaths":["services/api/**"],"ignorePaths":["services/api/docs/**"],"buildTimeoutSeconds":240,"buildEnvironment":{"BUILD_TOKEN":"build-secret-value"}}}`)
	if status != http.StatusOK {
		t.Fatalf("update source status=%d body=%s", status, body)
	}
	if strings.Contains(string(body), "build-secret-value") || !strings.Contains(string(body), `"buildEnvironmentKeys":["BUILD_TOKEN"]`) || !strings.Contains(string(body), `"buildTimeoutSeconds":240`) {
		t.Fatalf("source update exposed a secret or omitted persisted settings: %s", body)
	}

	status, body = f.dashboardCall(t, "automation", `{"action":"getSource","name":"auto-app"}`)
	if status != http.StatusOK || strings.Contains(string(body), "build-secret-value") || !strings.Contains(string(body), `"ref":"main"`) || !strings.Contains(string(body), `"rootDir":"services/api"`) || !strings.Contains(string(body), `"includePaths":["services/api/**"]`) {
		t.Fatalf("getSource did not return safe persisted settings: status=%d body=%s", status, body)
	}

	status, body = f.dashboardCall(t, "automation", `{"action":"createHook","name":"auto-app","branch":"main"}`)
	if status != http.StatusOK {
		t.Fatalf("create webhook status=%d body=%s", status, body)
	}
	created := decodeFeatureResponse(t, body)
	hook, ok := created["hook"].(map[string]any)
	if !ok || created["secret"] == "" || hook["id"] == "" {
		t.Fatalf("create webhook response is incomplete: %s", body)
	}
	secret := created["secret"].(string)
	hookID := hook["id"].(string)
	if created["githubPath"] != "/api/hooks/github/"+hookID || created["deployPath"] != "/api/hooks/deploy/"+hookID {
		t.Fatalf("hook paths are not usable from the UI contract: %s", body)
	}

	status, body = f.dashboardCall(t, "automation", `{"action":"listHooks","name":"auto-app"}`)
	if status != http.StatusOK || strings.Contains(string(body), secret) || strings.Contains(string(body), `"secret"`) || !strings.Contains(string(body), hookID) {
		t.Fatalf("listHooks leaked the one-time secret or omitted metadata: status=%d body=%s", status, body)
	}

	wrongBearer := make(http.Header)
	wrongBearer.Set("Authorization", "Bearer wrong-token")
	response := f.publicHookCall(http.MethodPost, "/api/hooks/deploy/"+hookID, []byte(`{}`), wrongBearer)
	if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("generic CI hook accepted bad token or echoed credentials: status=%d body=%q", response.Code, response.Body.String())
	}

	pushBody := []byte(`{"ref":"refs/heads/other","after":"0123456789abcdef0123456789abcdef01234567","repository":{"html_url":"https://github.com/example/auto-app"}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(pushBody)
	validSignature := make(http.Header)
	validSignature.Set("X-GitHub-Event", "push")
	validSignature.Set("X-GitHub-Delivery", "feature-integration-delivery")
	validSignature.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response = f.publicHookCall(http.MethodPost, "/api/hooks/github/"+hookID, pushBody, validSignature)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"accepted":false`) {
		t.Fatalf("signed branch-mismatched push status=%d body=%q", response.Code, response.Body.String())
	}

	status, body = f.dashboardCall(t, "automation", fmt.Sprintf(`{"action":"regenerateHook","id":%q}`, hookID))
	if status != http.StatusOK {
		t.Fatalf("regenerate hook status=%d body=%s", status, body)
	}
	regenerated := decodeFeatureResponse(t, body)
	newSecret, ok := regenerated["secret"].(string)
	if !ok || newSecret == "" || newSecret == secret {
		t.Fatalf("hook secret was not rotated: %s", body)
	}
	oldBearer := make(http.Header)
	oldBearer.Set("Authorization", "Bearer "+secret)
	response = f.publicHookCall(http.MethodPost, "/api/hooks/deploy/"+hookID, []byte(`{}`), oldBearer)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("old CI bearer remained valid after rotation: status=%d body=%q", response.Code, response.Body.String())
	}

	status, body = f.dashboardCall(t, "automation", fmt.Sprintf(`{"action":"revokeHook","id":%q}`, hookID))
	if status != http.StatusOK || !strings.Contains(string(body), `"revoked":true`) {
		t.Fatalf("revoke hook status=%d body=%s", status, body)
	}
	activeBearer := make(http.Header)
	activeBearer.Set("Authorization", "Bearer "+newSecret)
	response = f.publicHookCall(http.MethodPost, "/api/hooks/deploy/"+hookID, []byte(`{}`), activeBearer)
	if response.Code != http.StatusNotFound {
		t.Fatalf("revoked generic CI hook remained active: status=%d body=%q", response.Code, response.Body.String())
	}
}
