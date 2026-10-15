package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

func TestObservationSampleTimestampMigrationNormalizesLegacyFractionalTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observability.sqlite")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE observation_samples(id INTEGER PRIMARY KEY,at TEXT NOT NULL,payload BLOB NOT NULL);
		INSERT INTO observation_samples(id,at,payload) VALUES(1,'2026-10-15T12:00:00.1Z','{}'),(2,'2026-10-15T12:00:00.19Z','{}')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen the existing on-disk table as newObservatory does at startup.
	db, err = sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := normalizeObservationSampleTimes(db); err != nil {
		t.Fatalf("normalize legacy timestamps: %v", err)
	}

	var first, second string
	if err := db.QueryRow(`SELECT at FROM observation_samples WHERE id=1`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT at FROM observation_samples WHERE id=2`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != "2026-10-15T12:00:00.100000000Z" || second != "2026-10-15T12:00:00.190000000Z" || first >= second {
		t.Fatalf("normalized timestamps = (%q, %q), want fixed-width chronological values", first, second)
	}
	var included int
	if err := db.QueryRow(`SELECT COUNT(*) FROM observation_samples WHERE at>=?`, "2026-10-15T12:00:00.150000000Z").Scan(&included); err != nil || included != 1 {
		t.Fatalf("samples after fractional cutoff = %d, err=%v; want 1", included, err)
	}
}

func TestObservationSampleTimestampFormatPreservesNanosecondChronology(t *testing.T) {
	base := time.Date(2026, 10, 15, 12, 0, 0, 100_000_000, time.UTC)
	later := base.Add(90 * time.Millisecond)
	legacyEarly, legacyLater := base.Format(time.RFC3339Nano), later.Format(time.RFC3339Nano)
	if legacyEarly < legacyLater {
		t.Fatalf("fixture does not demonstrate legacy lexical inversion: %q < %q", legacyEarly, legacyLater)
	}
	early, late := formatObservationTimestamp(base), formatObservationTimestamp(later)
	if early != "2026-10-15T12:00:00.100000000Z" || late != "2026-10-15T12:00:00.190000000Z" || early >= late {
		t.Fatalf("fixed timestamps = (%q, %q), want chronological fixed-width UTC values", early, late)
	}
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

func TestObservationFirstSampleBaselinesExistingFailures(t *testing.T) {
	o, db, _, _ := observeFixture(t)
	secretURL := "https://hooks.example.test/opaque-secret"
	cfg := observeSettings{Name: "demo", CooldownSeconds: 60, Events: []string{"BUILD_FAILED"}}
	if _, err := o.dispatch(context.Background(), observeRequest{Action: "configure", Name: "demo", Settings: &cfg, WebhookURL: &secretURL}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	o.client = &http.Client{Transport: observationTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}

	// This deployment was already failed before the notification hook existed.
	o.sample(context.Background())
	if calls != 0 {
		t.Fatalf("initial sample delivered %d historical notifications", calls)
	}
	var state string
	if err := o.db.QueryRow(`SELECT state FROM observation_states WHERE name='demo' AND deployment_id='dep_logs'`).Scan(&state); err != nil || state != string(model.BuildFailed) {
		t.Fatalf("initial state baseline = %q, err=%v", state, err)
	}

	deployment, err := db.LatestDeployment("demo")
	if err != nil {
		t.Fatal(err)
	}
	deployment.State = model.Running
	if err = db.UpdateDeployment(*deployment); err != nil {
		t.Fatal(err)
	}
	o.sample(context.Background())
	deployment.State = model.BuildFailed
	if err = db.UpdateDeployment(*deployment); err != nil {
		t.Fatal(err)
	}
	o.sample(context.Background())
	if calls != 1 {
		t.Fatalf("transition back to failure delivered %d notifications, want 1", calls)
	}
}

func TestObservationFirstSampleNotifiesNewFailureAfterConfiguration(t *testing.T) {
	o, db, _, _ := observeFixture(t)
	secretURL := "https://hooks.example.test/opaque-secret"
	cfg := observeSettings{Name: "demo", CooldownSeconds: 60, Events: []string{"BUILD_FAILED"}}
	if _, err := o.dispatch(context.Background(), observeRequest{Action: "configure", Name: "demo", Settings: &cfg, WebhookURL: &secretURL}); err != nil {
		t.Fatal(err)
	}
	apps, err := db.Apps()
	if err != nil || len(apps) != 1 {
		t.Fatalf("Apps() = %d, %v", len(apps), err)
	}
	if err := db.CreateDeployment(model.Deployment{
		ID: "dep_after_hook", AppID: apps[0].ID, AppName: "demo", State: model.BuildFailed,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	o.client = &http.Client{Transport: observationTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	o.sample(context.Background())
	if calls != 1 {
		t.Fatalf("new failure delivered %d notifications, want 1", calls)
	}
}

func TestObservationSamplesProductionBeyondRecentAttempts(t *testing.T) {
	o, db, service, _ := observeFixture(t)
	apps, err := db.Apps()
	if err != nil || len(apps) != 1 {
		t.Fatalf("Apps() = %d, %v", len(apps), err)
	}
	base := time.Now().UTC().Add(-2 * time.Hour)
	production := model.Deployment{ID: "dep_old_production", AppID: apps[0].ID, AppName: "demo", State: model.Running, CreatedAt: base}
	if err := db.CreateDeployment(production); err != nil {
		t.Fatal(err)
	}
	oldCandidate := model.Deployment{ID: "dep_old_candidate", AppID: apps[0].ID, AppName: "demo", State: model.Building, CreatedAt: base.Add(-time.Minute)}
	if err := db.CreateDeployment(oldCandidate); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 21; i++ {
		candidate := model.Deployment{
			ID: fmt.Sprintf("dep_candidate_%02d", i), AppID: apps[0].ID, AppName: "demo",
			State: model.BuildFailed, CreatedAt: base.Add(time.Duration(i+1) * time.Minute),
		}
		if err := db.CreateDeployment(candidate); err != nil {
			t.Fatal(err)
		}
	}

	// Route the old deployment as production. Newer failed candidates do not
	// change this mapping, but they push it beyond the observer's history page.
	routeDB, err := sql.Open("sqlite3", filepath.Join(filepath.Dir(service.DataDir()), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer routeDB.Close()
	if _, err := routeDB.Exec(`INSERT INTO routes(hostname,deployment_id) VALUES(?,?)`, "demo.localhost", production.ID); err != nil {
		t.Fatal(err)
	}
	secretURL := "https://hooks.example.test/opaque-secret"
	cfg := observeSettings{Name: "demo", CooldownSeconds: 60, Events: []string{"CRASHED", "BUILD_FAILED"}}
	if _, err := o.dispatch(context.Background(), observeRequest{Action: "configure", Name: "demo", Settings: &cfg, WebhookURL: &secretURL}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	o.client = &http.Client{Transport: observationTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}

	// Match the real crash path: Finish commits the terminal state and removes
	// the active route before the next observability sample.
	production.State = model.Crashed
	if err := db.Finish(production, "demo.localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ActiveDeployment("demo"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("production route still resolves after Finish(): err=%v", err)
	}
	oldCandidate.State = model.BuildFailed
	if err := db.Finish(oldCandidate, "demo.localhost"); err != nil {
		t.Fatal(err)
	}
	o.sample(context.Background())
	if calls != 2 {
		t.Fatalf("off-page transitions delivered %d notifications, want 2", calls)
	}
	for _, expected := range []struct {
		id    string
		state model.State
	}{
		{production.ID, model.Crashed},
		{oldCandidate.ID, model.BuildFailed},
	} {
		var state string
		if err := o.db.QueryRow(`SELECT state FROM observation_states WHERE name='demo' AND deployment_id=?`, expected.id).Scan(&state); err != nil || state != string(expected.state) {
			t.Fatalf("deployment %s observation state = %q, err=%v", expected.id, state, err)
		}
	}
}

func TestObservationStatesStayBoundedAcrossDeploymentHistory(t *testing.T) {
	o, db, _, _ := observeFixture(t)
	apps, err := db.Apps()
	if err != nil || len(apps) != 1 {
		t.Fatalf("Apps() = %d, %v", len(apps), err)
	}
	payload, err := json.Marshal(storedObserveSettings{Settings: observeSettings{Name: "demo", Events: []string{"BUILD_FAILED"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = o.db.Exec(`INSERT INTO observation_settings(name,payload) VALUES('demo',?)`, payload); err != nil {
		t.Fatal(err)
	}

	createFailures := func(prefix string, first int) {
		t.Helper()
		base := time.Now().UTC().Add(time.Duration(first) * time.Hour)
		for i := 0; i < 125; i++ {
			item := model.Deployment{
				ID: fmt.Sprintf("%s_%03d", prefix, i), AppID: apps[0].ID, AppName: "demo",
				State: model.BuildFailed, CreatedAt: base.Add(time.Duration(i) * time.Second),
			}
			if err := db.CreateDeployment(item); err != nil {
				t.Fatal(err)
			}
		}
	}
	stateCount := func() int {
		t.Helper()
		var count int
		if err := o.db.QueryRow(`SELECT COUNT(*) FROM observation_states WHERE name='demo'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	createFailures("dep_first", 0)
	o.sample(context.Background())
	if count := stateCount(); count > 20 {
		t.Fatalf("observation states after first history page = %d, want at most 20", count)
	}
	createFailures("dep_second", 1)
	o.sample(context.Background())
	if count := stateCount(); count > 20 {
		t.Fatalf("observation states after repeated history pages = %d, want at most 20", count)
	}
}

func TestObservationStatePruningWaitsForCompletePass(t *testing.T) {
	o, db, _, _ := observeFixture(t)
	apps, err := db.Apps()
	if err != nil || len(apps) != 1 {
		t.Fatalf("Apps() = %d, %v", len(apps), err)
	}
	payload, err := json.Marshal(storedObserveSettings{Settings: observeSettings{Name: "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = o.db.Exec(`INSERT INTO observation_settings(name,payload) VALUES('demo',?)`, payload); err != nil {
		t.Fatal(err)
	}
	if _, err = o.db.Exec(`INSERT INTO observation_states(name,deployment_id,state) VALUES('demo','dep_stale','BUILD_FAILED')`); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateDeployment(model.Deployment{ID: "dep_state_write_fails", AppID: apps[0].ID, AppName: "demo", State: model.BuildFailed, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err = o.db.Exec(`CREATE TRIGGER fail_observation_state_write BEFORE INSERT ON observation_states
		WHEN NEW.deployment_id='dep_state_write_fails' BEGIN SELECT RAISE(ABORT,'injected state write failure'); END`); err != nil {
		t.Fatal(err)
	}

	o.sample(context.Background())
	var retained int
	if err := o.db.QueryRow(`SELECT COUNT(*) FROM observation_states WHERE name='demo' AND deployment_id='dep_stale'`).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("stale state after incomplete sample = %d, err=%v; want it preserved for retry", retained, err)
	}
}

func TestObservationSettingsAreNotSilentlyCappedAtOneThousand(t *testing.T) {
	o, _, _, _ := observeFixture(t)
	for i := 0; i < 1001; i++ {
		name := fmt.Sprintf("app-%04d", i)
		payload, err := json.Marshal(storedObserveSettings{Settings: observeSettings{Name: name}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = o.db.Exec(`INSERT INTO observation_settings(name,payload) VALUES(?,?)`, name, payload); err != nil {
			t.Fatal(err)
		}
	}
	settings, err := o.allSettings()
	if err != nil {
		t.Fatal(err)
	}
	if len(settings) != 1001 {
		t.Fatalf("allSettings returned %d entries, want all 1001", len(settings))
	}
}

func TestObserveConfigureSerializesWithForget(t *testing.T) {
	o, _, _, _ := observeFixture(t)
	cfg := observeSettings{Name: "demo", CooldownSeconds: 60, Maintenance: true}
	configureDone := make(chan error, 1)
	o.sampleMu.Lock()
	go func() {
		_, err := o.dispatch(context.Background(), observeRequest{Action: "configure", Name: "demo", Settings: &cfg})
		configureDone <- err
	}()
	time.Sleep(25 * time.Millisecond)
	var count int
	if err := o.db.QueryRow(`SELECT COUNT(*) FROM observation_settings WHERE name='demo'`).Scan(&count); err != nil {
		o.sampleMu.Unlock()
		t.Fatal(err)
	}
	if count != 0 {
		o.sampleMu.Unlock()
		t.Fatal("configure persisted settings while sample/forget lock was held")
	}
	o.sampleMu.Unlock()
	if err := <-configureDone; err != nil {
		t.Fatal(err)
	}
	if _, err := o.dispatch(context.Background(), observeRequest{Action: "forget", Name: "demo"}); err != nil {
		t.Fatal(err)
	}
	if err := o.db.QueryRow(`SELECT COUNT(*) FROM observation_settings WHERE name='demo'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("settings after forget = %d, err=%v", count, err)
	}
	o.mu.Lock()
	_, applied := o.applied["demo"]
	o.mu.Unlock()
	if applied {
		t.Fatal("traffic policy remained applied after forget")
	}
}
