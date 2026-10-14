package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paas/internal/deployment"
	"paas/internal/model"
	"paas/internal/router"
	"paas/internal/store"
)

func observeFixture(t *testing.T) (*Observatory, *store.Store, *deployment.Service, string) {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := deployment.New(db, nil, nil, router.New(), deployment.Options{DataDir: filepath.Join(root, "data")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Configure(context.Background(), "demo", deployment.ConfigureOptions{Environment: map[string]string{"TOKEN": "log-secret-value"}})
	if err != nil {
		t.Fatal(err)
	}
	o, err := newObservatory(service)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close(); db.Close() })
	app, err := db.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "build.log")
	if err = os.WriteFile(path, []byte("first log-secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = db.CreateDeployment(model.Deployment{ID: "dep_logs", AppID: app.ID, AppName: "demo", State: model.BuildFailed, LogPath: path}); err != nil {
		t.Fatal(err)
	}
	return o, db, service, path
}
func TestIncrementalLogCursorRedactsAndResets(t *testing.T) {
	o, _, _, path := observeFixture(t)
	fetch := func(cursor string) map[string]any {
		result, err := o.logDelta(observeRequest{Name: "demo", DeploymentID: "dep_logs", Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		return result.(map[string]any)
	}
	first := fetch("")
	if strings.Contains(first["text"].(string), "log-secret-value") || !first["reset"].(bool) {
		t.Fatalf("unsafe first=%+v", first)
	}
	same := fetch(first["cursor"].(string))
	if same["text"] != "" || same["reset"].(bool) {
		t.Fatalf("same=%+v", same)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("second\n")
	f.Close()
	next := fetch(same["cursor"].(string))
	if next["text"] != "second\n" || next["reset"].(bool) {
		t.Fatalf("next=%+v", next)
	}
	os.WriteFile(path, []byte("rotated\n"), 0600)
	rotated := fetch(next["cursor"].(string))
	if rotated["text"] != "rotated\n" || !rotated["reset"].(bool) {
		t.Fatalf("rotation=%+v", rotated)
	}
	malformed := fetch("bad cursor")
	if !malformed["reset"].(bool) {
		t.Fatal("invalid cursor wasn't reset")
	}
}
func TestObservationSettingsEncryptWebhookAndValidateEvents(t *testing.T) {
	o, _, _, _ := observeFixture(t)
	secretURL := "https://hooks.example.com/opaque-secret"
	cfg := observeSettings{Name: "demo", CooldownSeconds: 60, Events: []string{"CRASHED"}, Maintenance: true}
	result, err := o.dispatch(context.Background(), observeRequest{Action: "configure", Name: "demo", Settings: &cfg, WebhookURL: &secretURL})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), secretURL) {
		t.Fatal("webhook leaked in response")
	}
	var persisted []byte
	if err = o.db.QueryRow(`SELECT payload FROM observation_settings WHERE name='demo'`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "opaque-secret") {
		t.Fatal("webhook stored in plaintext")
	}
	cfg.Events = []string{"unknown"}
	if _, err = o.dispatch(context.Background(), observeRequest{Action: "configure", Name: "demo", Settings: &cfg}); err == nil {
		t.Fatal("unknown event accepted")
	}
}
func TestObserveRouteRejectsMalformedAndUnknownFields(t *testing.T) {
	o, _, svc, _ := observeFixture(t)
	_ = o
	mux := http.NewServeMux()
	registerObservabilityRoutes(mux, svc)
	for _, body := range []string{`{"action":"domains","name":"demo"}`, `{"action":"configure","name":"demo","settings":{"name":"demo","primaryDomain":"example.com","cooldownSeconds":60}}`, `null`, `{"action":"logs","name":"../bad"}`, `{"action":"get","name":"demo","unexpected":1}`} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/observe", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("%s status=%d body=%s", body, w.Code, w.Body.String())
		}
	}
}

func TestOutboundObservationRejectsPrivateAndReservedNetworks(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.1.1", "192.0.2.1", "198.51.100.1", "203.0.113.1", "198.18.1.1", "240.0.0.1", "0.1.1.1", "::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1", "2001:db8::1", "2002:7f00:1::1", "3fff::1"} {
		if publicObserveIP(net.ParseIP(address)) {
			t.Errorf("accepted %s", address)
		}
	}
	for _, address := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111", "2001:4860:4860::8888"} {
		if !publicObserveIP(net.ParseIP(address)) {
			t.Errorf("rejected %s", address)
		}
	}
}

type observationTransport func(*http.Request) (*http.Response, error)

func (f observationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestObservationNotificationCooldownSurvivesMemoryReset(t *testing.T) {
	o, _, _, _ := observeFixture(t)
	encrypted, err := o.seal("https://hooks.example.com/secret")
	if err != nil {
		t.Fatal(err)
	}
	settings := storedObserveSettings{Settings: observeSettings{Name: "demo", CooldownSeconds: 300}, Webhook: encrypted}
	calls := 0
	o.client = &http.Client{Transport: observationTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	})}
	o.notify(context.Background(), settings, "CRASHED", "App crashed")
	o.lastNotice = map[string]time.Time{}
	o.notify(context.Background(), settings, "CRASHED", "App crashed")
	if calls != 1 {
		t.Fatalf("deliveries=%d want1", calls)
	}
	var successes int
	if err = o.db.QueryRow(`SELECT COUNT(*) FROM observation_notifications WHERE name='demo' AND success=1`).Scan(&successes); err != nil || successes != 1 {
		t.Fatalf("history successes=%d err=%v", successes, err)
	}
}
