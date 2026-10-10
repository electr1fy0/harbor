package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"paas/internal/contracts"
	"paas/internal/model"

	sqlite3 "github.com/mattn/go-sqlite3"
)

func TestStorePersistsRecordsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paas.sqlite")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	journalMode := ""
	if err := first.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal mode = %q, want wal", journalMode)
	}

	app, err := first.CreateApp("hello")
	if err != nil {
		t.Fatal(err)
	}
	again, err := first.CreateApp("hello")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != app.ID || !again.CreatedAt.Equal(app.CreatedAt) {
		t.Fatalf("duplicate app = %#v, want existing app %#v", again, app)
	}

	createdAt := time.Date(2025, 2, 3, 4, 5, 6, 789123456, time.UTC)
	startedAt := time.Date(2025, 2, 3, 4, 6, 7, 123456789, time.UTC)
	stoppedAt := time.Date(2025, 2, 3, 5, 6, 7, 987654321, time.UTC)
	release := &contracts.Release{
		ID:          "rel_1",
		SourceHash:  "sha256:source",
		Runtime:     "go",
		ArtifactDir: "/var/lib/paas/releases/rel_1",
		Command:     []string{"/app/server", "--port", "$PORT"},
		CreatedAt:   createdAt,
	}
	if err := first.SaveRelease(app.ID, release); err != nil {
		t.Fatal(err)
	}
	deployment := model.Deployment{
		ID:          "dep_1",
		AppID:       app.ID,
		AppName:     app.Name,
		ReleaseID:   release.ID,
		Port:        43210,
		State:       model.Stopped,
		Error:       "process exited",
		PID:         1234,
		LogPath:     "/var/log/paas/dep_1.log",
		SystemdUnit: "paas-deploy-dep_1.service",
		SandboxType: "systemd",
		SandboxID:   "sbx_1",
		MemoryLimit: "128Mi",
		CPUQuota:    0.5,
		TasksLimit:  64,
		CreatedAt:   createdAt,
		StartedAt:   &startedAt,
		StoppedAt:   &stoppedAt,
	}
	if err := first.CreateDeployment(deployment); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	gotApps, err := second.Apps()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotApps) != 1 || gotApps[0].ID != app.ID || gotApps[0].Name != app.Name || !gotApps[0].CreatedAt.Equal(app.CreatedAt) {
		t.Fatalf("Apps() = %#v", gotApps)
	}

	gotRelease, err := second.Release(release.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotRelease, release) {
		t.Fatalf("Release() = %#v, want %#v", gotRelease, release)
	}

	gotDeployment, err := second.Deployment(deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotDeployment.AppName != app.Name || gotDeployment.ID != deployment.ID ||
		gotDeployment.AppID != deployment.AppID || gotDeployment.ReleaseID != deployment.ReleaseID ||
		gotDeployment.Port != deployment.Port || gotDeployment.State != deployment.State ||
		gotDeployment.Error != deployment.Error || gotDeployment.PID != deployment.PID ||
		gotDeployment.LogPath != deployment.LogPath ||
		gotDeployment.SystemdUnit != deployment.SystemdUnit ||
		gotDeployment.SandboxType != deployment.SandboxType ||
		gotDeployment.SandboxID != deployment.SandboxID ||
		gotDeployment.MemoryLimit != deployment.MemoryLimit ||
		gotDeployment.CPUQuota != deployment.CPUQuota ||
		gotDeployment.TasksLimit != deployment.TasksLimit ||
		!gotDeployment.CreatedAt.Equal(createdAt) ||
		gotDeployment.StartedAt == nil || !gotDeployment.StartedAt.Equal(startedAt) ||
		gotDeployment.StoppedAt == nil || !gotDeployment.StoppedAt.Equal(stoppedAt) {
		t.Fatalf("Deployment() = %#v, want persisted fields from %#v", gotDeployment, deployment)
	}

	latest, err := second.LatestDeployment(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != deployment.ID {
		t.Fatalf("LatestDeployment() id = %q, want %q", latest.ID, deployment.ID)
	}
	deployments, err := second.Deployments()
	if err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 1 || deployments[0].ID != deployment.ID {
		t.Fatalf("Deployments() = %#v", deployments)
	}
}

func TestUnfinishedDeploymentsExcludesHistoryAndScopesApplication(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().UTC()
	var wantAll, wantDemo []string
	for _, name := range []string{"demo", "other"} {
		app, err := s.CreateApp(name)
		if err != nil {
			t.Fatal(err)
		}
		states := []model.State{model.Queued, model.Building, model.Built, model.Starting, model.HealthChecking, model.Running,
			model.BuildFailed, model.StartFailed, model.Unhealthy, model.Crashed, model.OOMKilled, model.Stopped}
		for i, state := range states {
			for _, cleaned := range []bool{false, true} {
				id := fmt.Sprintf("%s-%d-%v", name, i, cleaned)
				d := model.Deployment{ID: id, AppID: app.ID, AppName: name, State: state, CreatedAt: now, SandboxID: id}
				if cleaned {
					d.StoppedAt = &now
				}
				if err := s.CreateDeployment(d); err != nil {
					t.Fatal(err)
				}
				if state.Active() || !cleaned {
					wantAll = append(wantAll, id)
					if name == "demo" {
						wantDemo = append(wantDemo, id)
					}
				}
			}
		}
		// A failed build never acquired a runtime and needs no cleanup, even
		// if no stop timestamp was recorded. Bad historical data must not
		// prevent unrelated admission and supervision from reading live work.
		d := model.Deployment{ID: name + "-history", AppID: app.ID, State: model.BuildFailed, CreatedAt: now}
		if err := s.CreateDeployment(d); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE deployments SET created_at = 'invalid' WHERE id = ?`, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name string
		want []string
	}{{"", wantAll}, {"demo", wantDemo}, {"missing", []string{}}} {
		got, err := s.UnfinishedDeployments(test.name)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(got))
		for _, d := range got {
			ids = append(ids, d.ID)
		}
		slices.Sort(ids)
		slices.Sort(test.want)
		if !reflect.DeepEqual(ids, test.want) {
			t.Errorf("UnfinishedDeployments(%q) = %v, want %v", test.name, ids, test.want)
		}
	}
}

func TestStoreEnforcesForeignKeysAndNotFound(t *testing.T) {
	s := openTestStore(t)
	app, err := s.CreateApp("hello")
	if err != nil {
		t.Fatal(err)
	}

	badRelease := &contracts.Release{ID: "rel_bad", Command: []string{"run"}}
	assertForeignKeyError(t, s.SaveRelease("missing-app", badRelease))

	release := saveTestRelease(t, s, app.ID, "rel_1", time.Now().UTC())
	badDeployment := testDeployment(app, release.ID, "dep_bad", time.Now().UTC())
	badDeployment.AppID = "missing-app"
	assertForeignKeyError(t, s.CreateDeployment(badDeployment))

	noRelease := testDeployment(app, "missing-release", "dep_no_release", time.Now().UTC())
	assertForeignKeyError(t, s.CreateDeployment(noRelease))

	if _, err := s.Release("missing-release"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Release(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := s.Deployment("missing-deployment"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Deployment(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := s.LatestDeployment("missing-app"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestDeployment(missing) error = %v, want ErrNotFound", err)
	}
	if err := s.UpdateDeployment(testDeployment(app, release.ID, "missing-deployment", time.Now().UTC())); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateDeployment(missing) error = %v, want ErrNotFound", err)
	}
}

func TestDeploymentCanMoveFromQueuedToBuilt(t *testing.T) {
	s := openTestStore(t)
	app, err := s.CreateApp("hello")
	if err != nil {
		t.Fatal(err)
	}
	queued := model.Deployment{
		ID:        "dep_queued",
		AppID:     app.ID,
		AppName:   app.Name,
		State:     model.Queued,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.CreateDeployment(queued); err != nil {
		t.Fatalf("create queued deployment without a release: %v", err)
	}
	if err := s.UpdateDeployment(queued); err != nil {
		t.Fatalf("update queued deployment without a release: %v", err)
	}

	release := saveTestRelease(t, s, app.ID, "rel_queued", time.Now().UTC())
	queued.ReleaseID = release.ID
	queued.State = model.Built
	if err := s.UpdateDeployment(queued); err != nil {
		t.Fatalf("update deployment with its built release: %v", err)
	}
	got, err := s.Deployment(queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.Built || got.ReleaseID != release.ID {
		t.Fatalf("deployment after build = %#v, want BUILT with release %q", got, release.ID)
	}
}

func TestPublishAndFinishKeepRouteAndDeploymentAtomic(t *testing.T) {
	s := openTestStore(t)
	app, err := s.CreateApp("hello")
	if err != nil {
		t.Fatal(err)
	}
	release := saveTestRelease(t, s, app.ID, "rel_1", time.Now().UTC())
	first := testDeployment(app, release.ID, "dep_1", time.Now().UTC())
	second := testDeployment(app, release.ID, "dep_2", time.Now().Add(time.Second).UTC())
	if err := s.CreateDeployment(first); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDeployment(second); err != nil {
		t.Fatal(err)
	}

	if err := s.Publish(first, " "); err == nil {
		t.Fatal("Publish with blank hostname succeeded")
	}
	gotFirst, err := s.Deployment(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotFirst.State != first.State {
		t.Fatalf("failed Publish changed state to %q, want %q", gotFirst.State, first.State)
	}
	routes, err := s.Routes()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Fatalf("failed Publish changed routes: %#v", routes)
	}

	if err := s.Publish(first, "hello.localhost"); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(second, "hello.localhost"); err != nil {
		t.Fatal(err)
	}
	gotSecond, err := s.Deployment(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSecond.State != model.Running {
		t.Fatalf("published deployment state = %q, want %q", gotSecond.State, model.Running)
	}

	badFinish := second
	badFinish.AppID = "missing-app"
	badFinish.State = model.Crashed
	assertForeignKeyError(t, s.Finish(badFinish, "hello.localhost"))
	routes, err = s.Routes()
	if err != nil {
		t.Fatal(err)
	}
	if routes["hello.localhost"] != second.ID {
		t.Fatalf("failed Finish changed route: %#v", routes)
	}
	gotSecond, err = s.Deployment(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSecond.State != model.Running {
		t.Fatalf("failed Finish changed state to %q, want %q", gotSecond.State, model.Running)
	}

	first.State = model.Stopped
	if err := s.Finish(first, "hello.localhost"); err != nil {
		t.Fatal(err)
	}
	routes, err = s.Routes()
	if err != nil {
		t.Fatal(err)
	}
	if routes["hello.localhost"] != second.ID {
		t.Fatalf("finishing old deployment removed newer route: %#v", routes)
	}

	second.State = model.Stopped
	if err := s.Finish(second, "hello.localhost"); err != nil {
		t.Fatal(err)
	}
	routes, err = s.Routes()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Fatalf("Finish left route behind: %#v", routes)
	}
	gotSecond, err = s.Deployment(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSecond.State != model.Stopped {
		t.Fatalf("finished deployment state = %q, want %q", gotSecond.State, model.Stopped)
	}
}

func TestStoreServicesAndBindings(t *testing.T) {
	s := openTestStore(t)
	app, err := s.CreateApp("testapp")
	if err != nil {
		t.Fatal(err)
	}

	svc := model.Service{
		ID:        "svc_test_db",
		Name:      "db",
		Type:      "postgres",
		Version:   "16",
		Port:      5432,
		State:     model.ServiceStarting,
		VolumeDir: "/data/svc/db",
		CreatedAt: time.Now().UTC(),
	}
	if err := s.CreateService(svc); err != nil {
		t.Fatalf("create service: %v", err)
	}

	gotSvc, err := s.GetService("svc_test_db")
	if err != nil {
		t.Fatalf("get service by id: %v", err)
	}
	if gotSvc.Name != "db" || gotSvc.Port != 5432 {
		t.Fatalf("unexpected service: %+v", gotSvc)
	}

	gotSvcName, err := s.GetService("db")
	if err != nil {
		t.Fatalf("get service by name: %v", err)
	}
	if gotSvcName.ID != "svc_test_db" {
		t.Fatalf("unexpected service by name: %+v", gotSvcName)
	}

	if err := s.UpdateServiceState("svc_test_db", model.ServiceReady); err != nil {
		t.Fatalf("update service state: %v", err)
	}
	gotSvc, _ = s.GetService("svc_test_db")
	if gotSvc.State != model.ServiceReady {
		t.Fatalf("expected state %s, got %s", model.ServiceReady, gotSvc.State)
	}

	svcs, err := s.ListServices()
	if err != nil {
		t.Fatalf("list services: %v", err)
	}
	if len(svcs) != 1 {
		t.Fatalf("expected 1 service, got %d", len(svcs))
	}

	b := model.Binding{
		ID:           "bind_1",
		AppID:        app.ID,
		ServiceID:    svc.ID,
		DatabaseName: "testdb",
		Username:     "testuser",
		Password:     "secret",
		EnvKey:       "DATABASE_URL",
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.CreateBinding(b); err != nil {
		t.Fatalf("create binding: %v", err)
	}

	bindings, err := s.GetBindingsForApp(app.ID)
	if err != nil {
		t.Fatalf("get bindings: %v", err)
	}
	if len(bindings) != 1 || bindings[0].DatabaseName != "testdb" {
		t.Fatalf("unexpected bindings: %+v", bindings)
	}
}

func TestStoreEvents(t *testing.T) {
	s := openTestStore(t)
	app, err := s.CreateApp("testapp_events")
	if err != nil {
		t.Fatal(err)
	}
	release := saveTestRelease(t, s, app.ID, "rel_evt", time.Now().UTC())
	dep := testDeployment(app, release.ID, "dep_123", time.Now().UTC())
	if err := s.CreateDeployment(dep); err != nil {
		t.Fatal(err)
	}

	if err := s.RecordEvent("dep_123", app.ID, model.EventSandboxStarting, "starting sandbox"); err != nil {
		t.Fatalf("record event 1: %v", err)
	}

	if err := s.RecordEvent("dep_123", app.ID, model.EventRouteSwitched, "route published"); err != nil {
		t.Fatalf("record event 2: %v", err)
	}

	events, err := s.Events("dep_123")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != model.EventSandboxStarting || events[1].Type != model.EventRouteSwitched {
		t.Fatalf("unexpected events ordering: %+v", events)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "paas.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}

func saveTestRelease(t *testing.T, s *Store, appID, id string, createdAt time.Time) *contracts.Release {
	t.Helper()
	r := &contracts.Release{
		ID:          id,
		SourceHash:  "sha256:" + id,
		Runtime:     "go",
		ArtifactDir: "/releases/" + id,
		Command:     []string{"/app/server"},
		CreatedAt:   createdAt,
	}
	if err := s.SaveRelease(appID, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func testDeployment(app *model.Application, releaseID, id string, createdAt time.Time) model.Deployment {
	return model.Deployment{
		ID:        id,
		AppID:     app.ID,
		AppName:   app.Name,
		ReleaseID: releaseID,
		Port:      41000,
		State:     model.Queued,
		CreatedAt: createdAt,
	}
}

func assertForeignKeyError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("operation succeeded, want foreign key error")
	}
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) {
		t.Fatalf("error %v is not a SQLite error", err)
	}
	if sqliteErr.ExtendedCode != sqlite3.ErrConstraintForeignKey {
		t.Fatalf("SQLite extended code = %v, want foreign key constraint", sqliteErr.ExtendedCode)
	}
}

func TestStoreUpgradesExistingDeploymentSchemasWithoutInventingBackend(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("version %d", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "upgrade.sqlite")
			db, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			app, err := db.CreateApp("legacy")
			if err != nil {
				t.Fatal(err)
			}
			d := model.Deployment{ID: "old", AppID: app.ID, State: model.Stopped, CreatedAt: time.Now().UTC(), LogPath: "/tmp/old.log"}
			if err := db.CreateDeployment(d); err != nil {
				t.Fatal(err)
			}
			columns := []string{"sandbox_type", "health_path", "promoted_at"}
			if version == 1 {
				columns = append(columns, "systemd_unit", "sandbox_id", "memory_limit", "cpu_quota", "tasks_limit")
			}
			for _, column := range columns {
				if _, err := db.db.Exec("ALTER TABLE deployments DROP COLUMN " + column); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			upgraded, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer upgraded.Close()
			got, err := upgraded.Deployment(d.ID)
			if err != nil || got.LogPath != d.LogPath || got.SandboxType != "" {
				t.Fatalf("legacy record damaged or backend fabricated: %+v %v", got, err)
			}
			var schema int
			if err := upgraded.db.QueryRow("PRAGMA user_version").Scan(&schema); err != nil || schema != schemaVersion {
				t.Fatalf("schema=%d err=%v", schema, err)
			}
		})
	}
}
func TestStoreCycle3Features(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "cycle3.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	app, err := s.CreateApp("testapp")
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.ActiveDeployment("testapp")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ActiveDeployment before publish = %v, want ErrNotFound", err)
	}

	rel1 := &contracts.Release{
		ID:          "rel_1",
		SourceHash:  "hash1",
		Runtime:     "go",
		ArtifactDir: "/tmp/rel1",
		Command:     []string{"server1"},
		CreatedAt:   time.Now().Add(-10 * time.Minute).UTC(),
	}
	if err := s.SaveRelease(app.ID, rel1); err != nil {
		t.Fatal(err)
	}

	dep1 := testDeployment(app, rel1.ID, "dep_1", rel1.CreatedAt)
	dep1.Port = 41001
	if err := s.CreateDeployment(dep1); err != nil {
		t.Fatal(err)
	}

	if err := s.Publish(dep1, "testapp.localhost"); err != nil {
		t.Fatal(err)
	}

	active, err := s.ActiveDeployment("testapp")
	if err != nil || active.ID != "dep_1" {
		t.Fatalf("ActiveDeployment = %#v, %v; want dep_1", active, err)
	}

	// Create candidate deployment dep2
	rel2 := &contracts.Release{
		ID:          "rel_2",
		SourceHash:  "hash2",
		Runtime:     "go",
		ArtifactDir: "/tmp/rel2",
		Command:     []string{"server2"},
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.SaveRelease(app.ID, rel2); err != nil {
		t.Fatal(err)
	}

	dep2 := testDeployment(app, rel2.ID, "dep_2", rel2.CreatedAt)
	dep2.Port = 41002
	if err := s.CreateDeployment(dep2); err != nil {
		t.Fatal(err)
	}

	// Active deployment should still be dep_1
	active, err = s.ActiveDeployment("testapp")
	if err != nil || active.ID != "dep_1" {
		t.Fatalf("ActiveDeployment during candidate = %#v, %v; want dep_1", active, err)
	}

	// Promote dep2
	prevID, err := s.Promote(dep2, "testapp.localhost")
	if err != nil {
		t.Fatalf("Promote() error = %v", err)
	}
	if prevID != "dep_1" {
		t.Fatalf("Promote() prevID = %q, want dep_1", prevID)
	}

	active, err = s.ActiveDeployment("testapp")
	if err != nil || active.ID != "dep_2" || active.State != model.Running {
		t.Fatalf("ActiveDeployment after promote = %#v, %v; want running dep_2", active, err)
	}

	// Test ReleasesForApp
	releases, err := s.ReleasesForApp("testapp")
	if err != nil {
		t.Fatalf("ReleasesForApp() error = %v", err)
	}
	if len(releases) != 2 {
		t.Fatalf("Releases count = %d, want 2", len(releases))
	}
	// Ordered by created_at DESC: rel_2 should be first and active
	if releases[0].ID != "rel_2" || !releases[0].IsActive {
		t.Fatalf("newest release = %#v, want rel_2 active", releases[0])
	}
	if releases[1].ID != "rel_1" || releases[1].IsActive {
		t.Fatalf("older release = %#v, want rel_1 inactive", releases[1])
	}

	// Test Events
	if err := s.RecordEvent(dep2.ID, app.ID, model.EventBuildStarted, "test payload"); err != nil {
		t.Fatalf("RecordEvent() error = %v", err)
	}
	if err := s.RecordEvent(dep2.ID, app.ID, model.EventRouteSwitched, "upstream=http://127.0.0.1:41002"); err != nil {
		t.Fatalf("RecordEvent() error = %v", err)
	}
	events, err := s.Events(dep2.ID)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("Events count = %d, want 2", len(events))
	}
	if events[0].Type != model.EventBuildStarted || events[1].Type != model.EventRouteSwitched {
		t.Fatalf("Events = %#v, want BuildStarted and RouteSwitched", events)
	}

	// Test PreviousRelease
	prevRel, err := s.PreviousRelease("testapp")
	if err != nil {
		t.Fatalf("PreviousRelease() error = %v", err)
	}
	if prevRel.ID != "rel_1" {
		t.Fatalf("PreviousRelease() = %q, want rel_1", prevRel.ID)
	}

	// Candidate failure must not pollute PreviousRelease
	rel3 := &contracts.Release{
		ID:          "rel_3",
		SourceHash:  "hash3",
		Runtime:     "go",
		ArtifactDir: "/tmp/rel3",
		Command:     []string{"server3"},
		CreatedAt:   time.Now().Add(time.Minute).UTC(),
	}
	if err := s.SaveRelease(app.ID, rel3); err != nil {
		t.Fatal(err)
	}
	dep3 := testDeployment(app, rel3.ID, "dep_3", rel3.CreatedAt)
	dep3.State = model.Unhealthy
	dep3.Error = "health check failed"
	if err := s.CreateDeployment(dep3); err != nil {
		t.Fatal(err)
	}
	prevRelAfterFailedCandidate, err := s.PreviousRelease("testapp")
	if err != nil {
		t.Fatalf("PreviousRelease() after failed candidate error = %v", err)
	}
	if prevRelAfterFailedCandidate.ID != "rel_1" {
		t.Fatalf("PreviousRelease() after failed candidate = %q, want rel_1 (NOT rel_3)", prevRelAfterFailedCandidate.ID)
	}

	// Test DeploymentForRelease
	depForRel2, err := s.DeploymentForRelease("rel_2")
	if err != nil {
		t.Fatalf("DeploymentForRelease(rel_2) error = %v", err)
	}
	if depForRel2.ID != "dep_2" {
		t.Fatalf("DeploymentForRelease(rel_2) = %q, want dep_2", depForRel2.ID)
	}

	// Test nonexistent app
	if _, err := s.ReleasesForApp("nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReleasesForApp(nonexistent) error = %v, want ErrNotFound", err)
	}
	if _, err := s.PreviousRelease("nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PreviousRelease(nonexistent) error = %v, want ErrNotFound", err)
	}
}

func TestMigrationReconcilesLocalVersionThree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-v3.sqlite")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	d := testDeployment(app, "", "local", time.Now().UTC())
	d.SandboxType = "host"
	if err := db.CreateDeployment(d); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`DROP TABLE events`, `ALTER TABLE deployments DROP COLUMN health_path`,
		`ALTER TABLE deployments DROP COLUMN promoted_at`, `PRAGMA user_version = 3`,
	} {
		if _, err := db.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	got, err := upgraded.Deployment(d.ID)
	if err != nil || got.SandboxType != "host" {
		t.Fatalf("lost backend: %+v %v", got, err)
	}
	if err := upgraded.RecordEvent(d.ID, app.ID, model.EventBuildStarted, ""); err != nil {
		t.Fatal(err)
	}
}

func TestPreviousReleaseTracksPromotionEvenAfterCrash(t *testing.T) {
	s := openTestStore(t)
	app, err := s.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"good", "current", "failed"} {
		r := saveTestRelease(t, s, app.ID, id, time.Now().Add(time.Duration(i)*time.Second))
		d := testDeployment(app, r.ID, "dep-"+id, r.CreatedAt)
		if err := s.CreateDeployment(d); err != nil {
			t.Fatal(err)
		}
		if id != "failed" {
			if _, err := s.Promote(d, "demo.localhost"); err != nil {
				t.Fatal(err)
			}
			d, err := s.Deployment(d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if id == "good" {
				d.State = model.Crashed
				d.Error = "later crash"
			}
			if err := s.UpdateDeployment(*d); err != nil {
				t.Fatal(err)
			}
		} else {
			// A stopped/canceled candidate never passed readiness.
			d.State = model.Stopped
			if err := s.UpdateDeployment(d); err != nil {
				t.Fatal(err)
			}
		}
	}
	r, err := s.PreviousRelease("demo")
	if err != nil || r.ID != "good" {
		t.Fatalf("previous=%+v err=%v", r, err)
	}
}

func TestMigrationUsesRemotePromotionEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-v3.sqlite")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"published", "canceled"} {
		r := saveTestRelease(t, db, app.ID, "rel-"+id, time.Now().UTC())
		d := testDeployment(app, r.ID, "dep-"+id, r.CreatedAt)
		d.State = model.Stopped
		if id == "published" {
			d.State = model.Crashed
			d.Error = "runtime crashed after publication"
		}
		if err := db.CreateDeployment(d); err != nil {
			t.Fatal(err)
		}
		if id == "published" {
			if err := db.RecordEvent(d.ID, app.ID, model.EventDeploymentSucceeded, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, column := range []string{"sandbox_type", "health_path", "promoted_at"} {
		if _, err := db.db.Exec("ALTER TABLE deployments DROP COLUMN " + column); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.db.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	published, err := upgraded.Deployment("dep-published")
	if err != nil || published.PromotedAt == nil || published.ReadinessKnown {
		t.Fatalf("bad published migration: %+v %v", published, err)
	}
	canceled, err := upgraded.Deployment("dep-canceled")
	if err != nil || canceled.PromotedAt != nil {
		t.Fatalf("canceled candidate became rollback target: %+v %v", canceled, err)
	}
}
