package store

import (
	"errors"
	"path/filepath"
	"reflect"
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
		ID:        "dep_1",
		AppID:     app.ID,
		AppName:   app.Name,
		ReleaseID: release.ID,
		Port:      43210,
		State:     model.Stopped,
		Error:     "process exited",
		PID:         1234,
		LogPath:     "/var/log/paas/dep_1.log",
		SystemdUnit: "paas-deploy-dep_1.service",
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

