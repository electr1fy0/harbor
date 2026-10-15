package store

import (
	"errors"
	"testing"
	"time"

	"paas/internal/contracts"
	"paas/internal/model"
)

func auditStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCreateAppRollsBackWhenSettingsInitializationFails(t *testing.T) {
	db := auditStore(t)
	if _, err := db.db.Exec(`CREATE TRIGGER refuse_settings BEFORE INSERT ON app_settings BEGIN SELECT RAISE(ABORT, 'injected settings failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateApp("atomic"); err == nil {
		t.Fatal("expected settings initialization failure")
	}
	apps, err := db.Apps()
	if err != nil || len(apps) != 0 {
		t.Fatalf("partial application persisted: %v %v", apps, err)
	}
}

func TestDeploymentCannotUseAnotherApplicationsRelease(t *testing.T) {
	db := auditStore(t)
	owner, err := db.CreateApp("owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateApp("other")
	if err != nil {
		t.Fatal(err)
	}
	release := &contracts.Release{ID: "release", Command: []string{"app"}, CreatedAt: time.Now()}
	if err := db.SaveRelease(owner.ID, release); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE releases SET app_id = ? WHERE id = ?`, other.ID, release.ID); err == nil {
		t.Fatal("immutable release could move to another application")
	}
	d := model.Deployment{ID: "deployment", AppID: other.ID, ReleaseID: release.ID, State: model.Queued, CreatedAt: time.Now()}
	if err := db.CreateDeployment(d); err == nil {
		t.Fatal("cross-application release accepted")
	}
	d.AppID = owner.ID
	if err := db.CreateDeployment(d); err != nil {
		t.Fatal(err)
	}
	d.AppID = other.ID
	if err := db.UpdateDeployment(d); err == nil {
		t.Fatal("cross-application update accepted")
	}
	got, err := db.Deployment(d.ID)
	if err != nil || got.AppID != owner.ID {
		t.Fatalf("deployment owner changed: %+v %v", got, err)
	}
}

func TestLifecycleEventsCannotLeakAcrossApplications(t *testing.T) {
	db := auditStore(t)
	owner, err := db.CreateApp("owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateApp("other")
	if err != nil {
		t.Fatal(err)
	}
	d := model.Deployment{ID: "deployment", AppID: owner.ID, State: model.Queued, CreatedAt: time.Now()}
	if err := db.CreateDeployment(d); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordEvent(d.ID, other.ID, model.EventBuildStarted, "private log"); err == nil {
		t.Fatal("cross-application event accepted")
	}
	if err := db.RecordEvent(d.ID, owner.ID, model.EventBuildStarted, "private log"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE events SET app_id = ? WHERE deployment_id = ?`, other.ID, d.ID); err == nil {
		t.Fatal("cross-application event reassignment accepted")
	}
	events, err := db.Events(d.ID)
	if err != nil || len(events) != 1 || events[0].AppID != owner.ID {
		t.Fatalf("events %+v %v", events, err)
	}
}

func TestDeleteAppDoesNotRequireOptionalOperationsSchema(t *testing.T) {
	db := auditStore(t)
	if _, err := db.CreateApp("delete-me"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteApp("delete-me"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppSettings("delete-me"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted app settings: %v", err)
	}
}

func TestDeploymentHistoryRejectsUnboundedPagination(t *testing.T) {
	db := auditStore(t)
	if _, err := db.CreateApp("history"); err != nil {
		t.Fatal(err)
	}
	for _, page := range [][2]int{{-1, 0}, {0, 0}, {1, -1}, {1_000_001, 0}, {10, 10_000_001}} {
		if _, _, err := db.DeploymentHistory("history", page[0], page[1]); err == nil {
			t.Fatalf("app history accepted %v", page)
		}
		if _, _, err := db.DeploymentHistoryAll(page[0], page[1]); err == nil {
			t.Fatalf("global history accepted %v", page)
		}
	}
}

func TestExistingCrashLoopDoesNotRequestAnotherRestart(t *testing.T) {
	db := auditStore(t)
	if err := db.InitOperations(); err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp("crash")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetRestartPolicy(app.Name, true, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDesiredRunning(app.ID, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.RecordAutomaticRestart(app.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := db.RecordAutomaticRestart(app.ID, time.Now())
	if err != nil || !plan.CrashLoop || plan.ShouldRun || plan.RestartAt != nil {
		t.Fatalf("crash-loop plan %+v %v", plan, err)
	}
}

func TestRetentionKeepsRollbackReleaseOfStoppedApp(t *testing.T) {
	db := auditStore(t)
	if err := db.InitOperations(); err != nil {
		t.Fatal(err)
	}
	app, err := db.CreateApp("stopped")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, id := range []string{"rollback", "production", "failed-newest"} {
		at := now.Add(time.Duration(i) * time.Second)
		if err := db.SaveRelease(app.ID, &contracts.Release{ID: id, CreatedAt: at, Command: []string{"app"}}); err != nil {
			t.Fatal(err)
		}
		d := model.Deployment{ID: "dep-" + id, AppID: app.ID, ReleaseID: id, State: model.Stopped, CreatedAt: at, StoppedAt: &at}
		if i < 2 {
			d.PromotedAt = &at
		}
		if err := db.CreateDeployment(d); err != nil {
			t.Fatal(err)
		}
	}
	candidates, err := db.ReleasePruneCandidates(app.Name, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("stopped app lost rollback artifacts: %+v", candidates)
	}
}

func TestDeploymentCannotMoveExistingEventsToAnotherApp(t *testing.T) {
	db := auditStore(t)
	owner, err := db.CreateApp("owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateApp("other")
	if err != nil {
		t.Fatal(err)
	}
	d := model.Deployment{ID: "queued-with-events", AppID: owner.ID, State: model.Queued, CreatedAt: time.Now()}
	if err := db.CreateDeployment(d); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordEvent(d.ID, owner.ID, model.EventBuildStarted, "owner log"); err != nil {
		t.Fatal(err)
	}
	d.AppID = other.ID
	if err := db.UpdateDeployment(d); err == nil {
		t.Fatal("deployment and its existing events could be split between applications")
	}
}

func TestImplicitRollbackAfterStopExcludesLastProductionRelease(t *testing.T) {
	db := auditStore(t)
	app, err := db.CreateApp("rollback-stopped")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, id := range []string{"previous", "last-production", "failed-candidate"} {
		at := now.Add(time.Duration(i) * time.Second)
		if err := db.SaveRelease(app.ID, &contracts.Release{ID: id, Command: []string{"app"}, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		d := model.Deployment{ID: "dep-" + id, AppID: app.ID, ReleaseID: id, State: model.Stopped, CreatedAt: at, StoppedAt: &at}
		if i < 2 {
			d.PromotedAt = &at
		}
		if err := db.CreateDeployment(d); err != nil {
			t.Fatal(err)
		}
	}
	release, err := db.PreviousRelease(app.Name)
	if err != nil || release.ID != "previous" {
		t.Fatalf("rollback target=%+v err=%v", release, err)
	}
}
