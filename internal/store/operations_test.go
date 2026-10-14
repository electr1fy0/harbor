package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"paas/internal/model"
)

func TestOperationSettingsRevisionAndRestartStatePersist(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "operations.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitOperations(); err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp("revision-demo")
	if err != nil {
		t.Fatal(err)
	}
	initial, err := db.OperationSettings(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	if initial.SettingsRevision != initial.AppliedSettingsRevision || initial.PendingSettings {
		t.Fatalf("new app settings revisions = %+v", initial)
	}
	settings, err := db.AppSettings(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	settings.MemoryLimit = "256Mi"
	if _, _, err := db.UpdateAppSettings(app.Name, *settings, nil); err != nil {
		t.Fatal(err)
	}
	changed, err := db.OperationSettings(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	if changed.SettingsRevision <= initial.SettingsRevision || !changed.PendingSettings {
		t.Fatalf("settings update did not create a pending revision: %+v", changed)
	}
	if err := db.MarkSettingsApplied(app.ID); err != nil {
		t.Fatal(err)
	}
	applied, err := db.OperationSettings(app.Name)
	if err != nil || applied.PendingSettings || applied.AppliedSettingsRevision != applied.SettingsRevision {
		t.Fatalf("applied settings = %+v, %v", applied, err)
	}
	if err := db.SetHealthPolicy(app.Name, 10, 2, 4); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateOperationMetadata(app.Name, true, true, []string{"backend"}, []string{"API_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	updated, err := db.OperationSettings(app.Name)
	if err != nil || !updated.Favorite || !updated.Archived || updated.HealthProbeIntervalSeconds != 10 || updated.HealthFailureThreshold != 4 || len(updated.Tags) != 1 || len(updated.RequiredEnvironmentKeys) != 1 {
		t.Fatalf("persisted operation metadata = %+v, %v", updated, err)
	}
	if err := db.SetDesiredRunning(app.ID, true); err != nil {
		t.Fatal(err)
	}

	plan, err := db.RecordAutomaticRestart(app.ID, time.Now().UTC())
	if err != nil || plan.ShouldRun || plan.CrashLoop || plan.Attempt != 0 || plan.RestartAt != nil {
		t.Fatalf("disabled restart plan = %+v, %v", plan, err)
	}
	if err := db.SetRestartPolicy(app.Name, true, 5, 1); err != nil {
		t.Fatal(err)
	}
	plan, err = db.RecordAutomaticRestart(app.ID, time.Now().UTC())
	if err != nil || !plan.ShouldRun || plan.CrashLoop || plan.Attempt != 1 || plan.RestartAt == nil {
		t.Fatalf("first enabled restart plan = %+v, %v", plan, err)
	}
	if err := db.SetDesiredRunning(app.ID, false); err != nil {
		t.Fatal(err)
	}
	plan, err = db.RecordAutomaticRestart(app.ID, time.Now().UTC())
	if err != nil || plan.ShouldRun {
		t.Fatalf("explicit stop did not suppress automatic restart: %+v, %v", plan, err)
	}
}

func TestMarkSettingsAppliedRevisionLeavesNewerChangesPending(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "revision-snapshot.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitOperations(); err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp("revision-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	before, err := db.OperationSettings(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := db.AppSettings(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	settings.MemoryLimit = "128Mi"
	if _, _, err := db.UpdateAppSettings(app.Name, *settings, nil); err != nil {
		t.Fatal(err)
	}
	consumed, err := db.OperationSettings(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	settings, err = db.AppSettings(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	settings.MemoryLimit = "256Mi"
	if _, _, err := db.UpdateAppSettings(app.Name, *settings, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkSettingsAppliedRevision(app.ID, consumed.SettingsRevision); err != nil {
		t.Fatal(err)
	}
	after, err := db.OperationSettings(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !after.PendingSettings || after.AppliedSettingsRevision != consumed.SettingsRevision || after.SettingsRevision <= consumed.SettingsRevision || consumed.SettingsRevision <= before.SettingsRevision {
		t.Fatalf("captured settings revision did not remain pending: before=%+v consumed=%+v after=%+v", before, consumed, after)
	}
}

func TestBindingChangesAdvanceDeploymentSettingsRevision(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "binding-revision.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitOperations(); err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp("binding-revision")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateService(model.Service{ID: "svc_revision", Name: "revision-db", Type: "sqlite", State: model.ServiceReady, VolumeDir: t.TempDir(), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	initial, err := db.OperationSettings(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	binding := model.Binding{ID: "bind_revision", AppID: app.ID, ServiceID: "svc_revision", DatabaseName: "db_revision", Username: "user", Password: "secret", EnvKey: "DATABASE_URL", CreatedAt: time.Now().UTC()}
	if err := db.CreateBinding(binding); err != nil {
		t.Fatal(err)
	}
	afterInsert, err := db.OperationSettings(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	if afterInsert.SettingsRevision <= initial.SettingsRevision {
		t.Fatalf("binding insert did not advance revision: before=%+v after=%+v", initial, afterInsert)
	}
	binding.EnvKey = "PRIMARY_DATABASE_URL"
	if err := db.CreateBinding(binding); err != nil {
		t.Fatal(err)
	}
	afterUpdate, err := db.OperationSettings(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	if afterUpdate.SettingsRevision <= afterInsert.SettingsRevision {
		t.Fatalf("binding update did not advance revision: before=%+v after=%+v", afterInsert, afterUpdate)
	}
	snapshot, err := db.DeploymentSettingsSnapshot(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SettingsRevision != afterUpdate.SettingsRevision || len(snapshot.Bindings) != 1 || snapshot.Bindings[0].EnvKey != binding.EnvKey {
		t.Fatalf("deployment snapshot = %+v, want latest revision and binding", snapshot)
	}
}

func TestAutomaticRestartStopsAtConfiguredCrashLoopLimit(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "restart-limit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitOperations(); err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp("restart-limit")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetDesiredRunning(app.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRestartPolicy(app.Name, true, 2, 1); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, err := db.RecordAutomaticRestart(app.ID, now)
	if err != nil || first.Attempt != 1 || !first.ShouldRun || first.RestartAt == nil {
		t.Fatalf("first restart plan = %+v, %v", first, err)
	}
	second, err := db.RecordAutomaticRestart(app.ID, now.Add(time.Second))
	if err != nil || second.Attempt != 2 || !second.ShouldRun || second.RestartAt == nil {
		t.Fatalf("second restart plan = %+v, %v", second, err)
	}
	third, err := db.RecordAutomaticRestart(app.ID, now.Add(2*time.Second))
	if err != nil || third.ShouldRun || !third.CrashLoop || third.RestartAt != nil {
		t.Fatalf("restart beyond configured limit = %+v, %v", third, err)
	}
	settings, err := db.OperationSettings(app.Name)
	if err != nil || !settings.CrashLoop || settings.NextRestartAt != nil || settings.RestartAttempt != 2 {
		t.Fatalf("persisted crash-loop state = %+v, %v", settings, err)
	}
}

func TestInitOperationsAddsNewColumnsToAnOlderTable(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "old-operations.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.db.Exec(`CREATE TABLE app_operations (
		app_id TEXT PRIMARY KEY NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
		favorite INTEGER NOT NULL DEFAULT 0,
		tags TEXT NOT NULL DEFAULT '[]',
		archived INTEGER NOT NULL DEFAULT 0,
		required_environment_keys TEXT NOT NULL DEFAULT '[]'
	)`); err != nil {
		t.Fatal(err)
	}
	if err := db.InitOperations(); err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp("migrated-demo")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := db.OperationSettings(app.Name)
	if err != nil {
		t.Fatal(err)
	}
	if settings.AutomaticRestartEnabled || settings.AutomaticRestartMaxAttempts != 5 || settings.AutomaticRestartBaseDelaySeconds != 1 || settings.RetainReleaseCount != 5 {
		t.Fatalf("migrated runtime defaults = %+v", settings)
	}
}

func TestOperationHistoryAndDeletePreserveForeignKeyOrder(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "delete.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitOperations(); err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp("delete-demo")
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC()
	d := model.Deployment{ID: "dep_history", AppID: app.ID, AppName: app.Name, State: model.Stopped, LogPath: "/tmp/test.log", CreatedAt: created}
	if err := db.CreateDeployment(d); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordEvent(d.ID, app.ID, model.EventType(model.Stopped), "stopped"); err != nil {
		t.Fatal(err)
	}
	history, total, err := db.DeploymentHistory(app.Name, 10, 0)
	if err != nil || total != 1 || len(history) != 1 || history[0].ID != d.ID {
		t.Fatalf("history = %+v total=%d err=%v", history, total, err)
	}
	events, err := db.EventsForApp(app.Name, d.ID)
	if err != nil || len(events) != 1 || events[0].Payload != "stopped" {
		t.Fatalf("events = %+v, %v", events, err)
	}
	other, err := db.CreateApp("other-history")
	if err != nil {
		t.Fatal(err)
	}
	otherDeployment := model.Deployment{ID: "dep_other", AppID: other.ID, AppName: other.Name, State: model.BuildFailed, CreatedAt: created.Add(time.Second)}
	if err := db.CreateDeployment(otherDeployment); err != nil {
		t.Fatal(err)
	}
	allHistory, allTotal, err := db.DeploymentHistoryAll(10, 0)
	if err != nil || allTotal != 2 || len(allHistory) != 2 || allHistory[0].AppName != other.Name {
		t.Fatalf("all history = %+v total=%d err=%v", allHistory, allTotal, err)
	}
	if err := db.DeleteApp(app.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := db.OperationSettings(app.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted app operation state error = %v", err)
	}
	if _, err := db.Deployment(d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted app deployment error = %v", err)
	}
	allHistory, allTotal, err = db.DeploymentHistoryAll(10, 0)
	if err != nil || allTotal != 1 || len(allHistory) != 1 || allHistory[0].AppName != other.Name {
		t.Fatalf("history after app deletion = %+v total=%d err=%v", allHistory, allTotal, err)
	}
}
