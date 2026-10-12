package service

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"paas/internal/contracts"
	"paas/internal/model"
	"paas/internal/store"
)

func openServiceStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLiteServiceLifecycleAndIsolatedPersistentBindings(t *testing.T) {
	state := openServiceStore(t)
	dataDir := t.TempDir()
	manager := NewManager(state, dataDir)

	svc, err := manager.AddService(context.Background(), "sqlite", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if svc.Type != "sqlite" || svc.Version != "3" || svc.Port != 0 || svc.State != model.ServiceReady {
		t.Fatalf("SQLite service = %+v", svc)
	}
	for _, check := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Join(dataDir, "volumes"), 0711}, {svc.VolumeDir, 0711}} {
		info, err := os.Stat(check.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != check.mode {
			t.Fatalf("permissions for %s = %04o, want %04o", check.path, got, check.mode)
		}
	}

	first, err := manager.Link(context.Background(), "first-app", svc.Name)
	if err != nil {
		t.Fatal(err)
	}
	firstSvc, err := state.GetService(svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstPath, err := DatabasePath(first, firstSvc)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(firstPath) != filepath.Join(svc.VolumeDir, first.DatabaseName) {
		t.Fatalf("binding database path = %s", firstPath)
	}
	for _, check := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Dir(firstPath), 0700}, {firstPath, 0600}} {
		info, err := os.Stat(check.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != check.mode {
			t.Fatalf("permissions for %s = %04o, want %04o", check.path, got, check.mode)
		}
	}
	urlText, err := DatabaseURL(first, firstSvc)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(urlText)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Scheme != "file" || parsed.Path != firstPath || query.Get("mode") != "rw" || query.Get("_busy_timeout") != "5000" || query.Get("_foreign_keys") != "on" {
		t.Fatalf("SQLite URL = %q", urlText)
	}
	writeDatabaseRow(t, urlText, "persisted")

	second, err := manager.Link(context.Background(), "second-app", svc.Name)
	if err != nil {
		t.Fatal(err)
	}
	secondSvc, err := state.GetService(svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondPath, err := DatabasePath(second, secondSvc)
	if err != nil {
		t.Fatal(err)
	}
	if secondPath == firstPath || second.DatabaseName == first.DatabaseName {
		t.Fatalf("applications share a database: %q", secondPath)
	}
	if _, err := DatabaseURL(second, secondSvc); err != nil {
		t.Fatal(err)
	}
	if err := assertDatabaseRow(urlText, "persisted"); err != nil {
		t.Fatalf("initial database data was not preserved: %v", err)
	}

	var wg sync.WaitGroup
	results := make(chan *model.Binding, 8)
	errCh := make(chan error, 8)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			binding, err := manager.Link(context.Background(), "first-app", svc.Name)
			if err != nil {
				errCh <- err
				return
			}
			results <- binding
		}()
	}
	wg.Wait()
	close(results)
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	for binding := range results {
		if binding.ID != first.ID || binding.DatabaseName != first.DatabaseName {
			t.Fatalf("re-link changed binding identity: %+v; original %+v", binding, first)
		}
	}
	if err := assertDatabaseRow(urlText, "persisted"); err != nil {
		t.Fatalf("re-link replaced application data: %v", err)
	}
}

func TestDatabaseHelpersEscapePathsAndRejectUnsafeOrMissingFiles(t *testing.T) {
	volume := filepath.Join(t.TempDir(), "service dir #?")
	if err := os.MkdirAll(filepath.Join(volume, "db_app_abc"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(volume, "db_app_abc", "database.sqlite")
	if err := os.WriteFile(path, []byte("SQLite format 3\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	svc := &model.Service{ID: "svc_one", Type: "sqlite", VolumeDir: volume}
	binding := &model.Binding{AppID: "app_abc", ServiceID: svc.ID, DatabaseName: "db_app_abc"}
	got, err := DatabasePath(binding, svc)
	if err != nil || got != path {
		t.Fatalf("DatabasePath() = %q, %v; want %q", got, err, path)
	}
	uri, err := DatabaseURL(binding, svc)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != path || !strings.Contains(uri, "%20") || !strings.Contains(uri, "%23") || !strings.Contains(uri, "%3F") {
		t.Fatalf("database URI did not escape the path: %q", uri)
	}

	missing := *binding
	missing.DatabaseName = "db_app_missing"
	if _, err := DatabasePath(&missing, svc); err == nil {
		t.Fatal("DatabasePath accepted a missing durable database")
	}
	unsafe := *binding
	unsafe.DatabaseName = "../outside"
	if _, err := DatabasePath(&unsafe, svc); err == nil {
		t.Fatal("DatabasePath accepted a traversal path")
	}
	wrongOwner := *binding
	wrongOwner.ServiceID = "svc_other"
	if _, err := DatabasePath(&wrongOwner, svc); err == nil {
		t.Fatal("DatabasePath accepted a binding owned by another service")
	}
	wrongApp := *binding
	wrongApp.AppID = "app_other"
	if _, err := DatabasePath(&wrongApp, svc); err == nil {
		t.Fatal("DatabasePath accepted a database owned by another application")
	}
	legacy := *svc
	legacy.Type = "postgres"
	if _, err := DatabasePath(binding, &legacy); err == nil {
		t.Fatal("DatabasePath accepted a non-SQLite service")
	}
	if err := os.Rename(path, path+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".real", path); err != nil {
		t.Fatal(err)
	}
	if _, err := DatabasePath(binding, svc); err == nil {
		t.Fatal("DatabasePath accepted a symlinked database")
	}
}

func TestLinkRejectsDuplicateDatabaseURLAndNonReadyService(t *testing.T) {
	state := openServiceStore(t)
	manager := NewManager(state, t.TempDir())
	first, err := manager.AddService(context.Background(), "sqlite", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.AddService(context.Background(), "sqlite", "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Link(context.Background(), "app", first.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Link(context.Background(), "app", second.Name); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("second database binding error = %v", err)
	}
	if err := state.UpdateServiceState(second.ID, model.ServiceFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Link(context.Background(), "other-app", second.Name); err == nil || !strings.Contains(err.Error(), "READY") {
		t.Fatalf("non-ready binding error = %v", err)
	}
}

func TestRestorePreservesDataAndFailsMissingOrCorruptDatabases(t *testing.T) {
	for _, corruption := range []string{"missing", "corrupt"} {
		t.Run(corruption, func(t *testing.T) {
			state := openServiceStore(t)
			manager := NewManager(state, t.TempDir())
			svc, err := manager.AddService(context.Background(), "sqlite", "primary")
			if err != nil {
				t.Fatal(err)
			}
			binding, err := manager.Link(context.Background(), "app", svc.Name)
			if err != nil {
				t.Fatal(err)
			}
			path, err := DatabasePath(binding, svc)
			if err != nil {
				t.Fatal(err)
			}
			corruptContents := []byte("not a SQLite database")
			if corruption == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, corruptContents, 0600); err != nil {
				t.Fatal(err)
			}
			if err := manager.Restore(context.Background()); err == nil || !strings.Contains(err.Error(), "database") {
				t.Fatalf("Restore() error = %v; want database validation error", err)
			}
			persisted, err := state.GetService(svc.ID)
			if err != nil || persisted.State != model.ServiceFailed {
				t.Fatalf("service after failed restore = %+v, %v", persisted, err)
			}
			if _, err := os.Stat(path); corruption == "missing" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("restore recreated missing database: %v", err)
			}
			if corruption == "corrupt" {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != string(corruptContents) {
					t.Fatalf("restore replaced corrupt database: %q, %v", got, err)
				}
			}
		})
	}

	state := openServiceStore(t)
	manager := NewManager(state, t.TempDir())
	svc, err := manager.AddService(context.Background(), "sqlite", "persistent")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := manager.Link(context.Background(), "app", svc.Name)
	if err != nil {
		t.Fatal(err)
	}
	serviceRecord, err := state.GetService(svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := DatabaseURL(binding, serviceRecord)
	if err != nil {
		t.Fatal(err)
	}
	writeDatabaseRow(t, uri, "survives-restore")
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	serviceRecord, err = state.GetService(svc.ID)
	if err != nil || serviceRecord.State != model.ServiceReady {
		t.Fatalf("restored service = %+v, %v", serviceRecord, err)
	}
	if err := assertDatabaseRow(uri, "survives-restore"); err != nil {
		t.Fatalf("restore lost database contents: %v", err)
	}
}

func TestRestoreFailsMissingOrCorruptServiceMarkerWithoutRepairingIt(t *testing.T) {
	for _, test := range []struct {
		name        string
		markerValue string
		missing     bool
	}{
		{name: "missing", missing: true},
		{name: "corrupt", markerValue: "another-service\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := openServiceStore(t)
			dataDir := t.TempDir()
			creator := NewManager(state, dataDir)
			svc, err := creator.AddService(context.Background(), "sqlite", "primary")
			if err != nil {
				t.Fatal(err)
			}
			markerPath := filepath.Join(svc.VolumeDir, ".harbor-sqlite-service")
			if test.missing {
				if err := os.Remove(markerPath); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(markerPath, []byte(test.markerValue), 0600); err != nil {
				t.Fatal(err)
			}
			restored := NewManager(state, dataDir)
			if err := restored.Restore(context.Background()); err == nil || !strings.Contains(strings.ToLower(err.Error()), "marker") {
				t.Fatalf("Restore() error = %v; want marker validation error", err)
			}
			persisted, err := state.GetService(svc.ID)
			if err != nil || persisted.State != model.ServiceFailed {
				t.Fatalf("service after invalid marker restore = %+v, %v", persisted, err)
			}
			if test.missing {
				if _, err := os.Lstat(markerPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("restore recreated missing marker: %v", err)
				}
			} else if got, err := os.ReadFile(markerPath); err != nil || string(got) != test.markerValue {
				t.Fatalf("restore replaced corrupt marker: %q, %v", got, err)
			}
		})
	}
}

func TestCanceledRestoreClearsReadinessForServicesItDoesNotValidate(t *testing.T) {
	state := openServiceStore(t)
	manager := NewManager(state, t.TempDir())
	first, err := manager.AddService(context.Background(), "sqlite", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.AddService(context.Background(), "sqlite", "second")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Restore(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Restore() error = %v; want cancellation", err)
	}
	for _, id := range []string{first.ID, second.ID} {
		persisted, err := state.GetService(id)
		if err != nil || persisted.State != model.ServiceFailed {
			t.Fatalf("service %s retained readiness after incomplete restore: %+v, %v", id, persisted, err)
		}
	}
}

func TestFailedDatabaseProvisioningCleansDirectoryForRetry(t *testing.T) {
	state := openServiceStore(t)
	manager := NewManager(state, t.TempDir())
	svc, err := manager.AddService(context.Background(), "sqlite", "primary")
	if err != nil {
		t.Fatal(err)
	}
	manager.bindingMgr.initializeDatabase = func(context.Context, string) error {
		return errors.New("simulated SQLite initialization failure")
	}
	if _, err := manager.Link(context.Background(), "retry-app", svc.Name); err == nil || !strings.Contains(err.Error(), "simulated SQLite initialization failure") {
		t.Fatalf("first Link() error = %v; want initialization failure", err)
	}
	app, err := state.CreateApp("retry-app")
	if err != nil {
		t.Fatal(err)
	}
	databaseDir := filepath.Join(svc.VolumeDir, "db_"+app.ID)
	if _, err := os.Lstat(databaseDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed provisioning left an unbound database directory: %v", err)
	}
	manager.bindingMgr.initializeDatabase = initializeSQLiteDatabase
	if _, err := manager.Link(context.Background(), app.Name, svc.Name); err != nil {
		t.Fatalf("Link() retry failed after cleanup: %v", err)
	}
}

func TestRestoreMarksLegacyPostgresFailedAndPreservesItsVolume(t *testing.T) {
	state := openServiceStore(t)
	volume := filepath.Join(t.TempDir(), "old-postgres-volume")
	if err := os.Mkdir(volume, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(volume, "PG_VERSION")
	if err := os.WriteFile(sentinel, []byte("16\n"), 0600); err != nil {
		t.Fatal(err)
	}
	legacy := model.Service{ID: "svc_legacy", Name: "legacy", Type: "postgres", Version: "16", Port: 5432, State: model.ServiceReady, VolumeDir: volume, CreatedAt: time.Now().UTC()}
	if err := state.CreateService(legacy); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(state, t.TempDir())
	err := manager.Restore(context.Background())
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "migrate") || !strings.Contains(err.Error(), "SQLite") {
		t.Fatalf("Restore() error = %v; want actionable PostgreSQL migration error", err)
	}
	persisted, err := state.GetService(legacy.ID)
	if err != nil || persisted.State != model.ServiceFailed {
		t.Fatalf("legacy service after restore = %+v, %v", persisted, err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "16\n" {
		t.Fatalf("legacy PostgreSQL volume changed: %q, %v", got, err)
	}
}

func TestLinkRejectsLegacyPostgresAndCancellation(t *testing.T) {
	state := openServiceStore(t)
	legacy := model.Service{ID: "svc_legacy", Name: "legacy", Type: "postgres", Version: "16", Port: 5432, State: model.ServiceReady, VolumeDir: filepath.Join(t.TempDir(), "pg"), CreatedAt: time.Now().UTC()}
	if err := state.CreateService(legacy); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(state, t.TempDir())
	if _, err := manager.Link(context.Background(), "legacy-app", legacy.Name); err == nil || !strings.Contains(strings.ToLower(err.Error()), "migrate") {
		t.Fatalf("legacy PostgreSQL link error = %v", err)
	}
	if _, err := manager.AddService(context.Background(), "sqlite", "sqlite"); err != nil {
		t.Fatal(err)
	}
	legacyApp, err := state.CreateApp("legacy-bound-app")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.CreateBinding(model.Binding{ID: "legacy_binding", AppID: legacyApp.ID, ServiceID: legacy.ID, DatabaseName: "db_old", EnvKey: "DATABASE_URL", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Link(context.Background(), legacyApp.Name, "sqlite"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "migrate") {
		t.Fatalf("switching away from a legacy PostgreSQL binding error = %v", err)
	}
	if _, err := manager.AddService(context.Background(), "postgres", "new-pg"); err == nil || !strings.Contains(err.Error(), "only sqlite") {
		t.Fatalf("AddService(postgres) error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.AddService(canceled, "sqlite", "canceled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled AddService() error = %v", err)
	}
	if _, err := manager.Link(canceled, "canceled-app", legacy.Name); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Link() error = %v", err)
	}
	if _, err := state.GetService("canceled"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("canceled service persisted: %v", err)
	}
	if _, err := state.AppSettings("canceled-app"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("canceled application persisted: %v", err)
	}
}

func writeDatabaseRow(t *testing.T, databaseURL, value string) {
	t.Helper()
	db, err := sql.Open("sqlite3", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sample (value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sample (value) VALUES (?)`, value); err != nil {
		t.Fatal(err)
	}
}

func assertDatabaseRow(databaseURL, want string) error {
	db, err := sql.Open("sqlite3", databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT value FROM sample LIMIT 1`).Scan(&got); err != nil {
		return err
	}
	if got != want {
		return errors.New("database contents changed")
	}
	return nil
}

type testServiceProvider struct {
	created   *contracts.Service
	createErr error
	startErr  error
	stopErr   error
	creates   int
	starts    int
	stops     int
}

func (p *testServiceProvider) Create(context.Context, contracts.ServiceRequest) (*contracts.Service, error) {
	p.creates++
	return p.created, p.createErr
}

func (p *testServiceProvider) Start(context.Context, *contracts.Service) error {
	p.starts++
	return p.startErr
}

func (p *testServiceProvider) Stop(context.Context, string) error {
	p.stops++
	return p.stopErr
}

func (*testServiceProvider) ConnectionInfo(context.Context, *contracts.Service) (map[string]string, error) {
	return nil, nil
}

func TestAddServiceRecordsOwnershipBeforeStarting(t *testing.T) {
	for _, test := range []struct {
		name      string
		startErr  error
		wantState model.ServiceState
	}{
		{name: "startup succeeds", wantState: model.ServiceReady},
		{name: "startup fails", startErr: errors.New("SQLite volume marker missing"), wantState: model.ServiceFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openServiceStore(t)
			provider := &testServiceProvider{
				created:  &contracts.Service{ID: "svc_owned", Name: "db", Type: "sqlite", Version: "3", Port: 0, VolumeDir: "/data/volumes/svc_owned"},
				startErr: test.startErr,
			}
			manager := &Manager{store: db, provider: provider}
			created, err := manager.AddService(context.Background(), "sqlite", "db")
			if test.startErr == nil {
				if err != nil || created == nil || created.State != model.ServiceReady {
					t.Fatalf("AddService() = %+v, %v", created, err)
				}
			} else if err == nil || created != nil {
				t.Fatalf("AddService() = %+v, %v; want startup error", created, err)
			}
			persisted, lookupErr := db.GetService("db")
			if lookupErr != nil || persisted.State != test.wantState {
				t.Fatalf("persisted service = %+v, %v; want state %s", persisted, lookupErr, test.wantState)
			}
			if provider.creates != 1 || provider.starts != 1 {
				t.Fatalf("provider calls = creates:%d starts:%d", provider.creates, provider.starts)
			}
		})
	}
}

func TestAddServiceDoesNotStartWhenIdentityCannotBePersisted(t *testing.T) {
	db := openServiceStore(t)
	if err := db.CreateService(model.Service{ID: "svc_duplicate", Name: "existing", Type: "sqlite", Version: "3", State: model.ServiceReady, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	provider := &testServiceProvider{created: &contracts.Service{ID: "svc_duplicate", Name: "new", Type: "sqlite", Version: "3"}}
	manager := &Manager{store: db, provider: provider}
	if _, err := manager.AddService(context.Background(), "sqlite", "new"); err == nil {
		t.Fatal("AddService unexpectedly succeeded with a conflicting persisted identity")
	}
	if provider.starts != 0 {
		t.Fatalf("provider started an unowned service %d times", provider.starts)
	}
}

func TestAddServiceConflictDoesNotDiscardExistingSQLiteVolume(t *testing.T) {
	state := openServiceStore(t)
	dataDir := t.TempDir()
	manager := NewManager(state, dataDir)
	original, err := manager.AddService(context.Background(), "sqlite", "original")
	if err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(original.VolumeDir, ".harbor-sqlite-service")
	provider := &testServiceProvider{created: &contracts.Service{
		ID: original.ID, Name: "conflicting", Type: "sqlite", Version: "3", VolumeDir: original.VolumeDir,
	}}
	manager.provider = provider
	if _, err := manager.AddService(context.Background(), "sqlite", "conflicting"); err == nil {
		t.Fatal("AddService unexpectedly succeeded with a conflicting service id")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("existing service volume was removed after id conflict: %v", err)
	}
	persisted, err := state.GetService(original.ID)
	if err != nil || persisted.Name != "original" || persisted.State != model.ServiceReady {
		t.Fatalf("original service after id conflict = %+v, %v", persisted, err)
	}
}

func TestAddServicePropagatesExistingLookupErrors(t *testing.T) {
	db := openServiceStore(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	provider := &testServiceProvider{}
	manager := &Manager{store: db, provider: provider}
	if _, err := manager.AddService(context.Background(), "sqlite", "db"); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("AddService() error = %v; want database lookup failure", err)
	}
	if provider.creates != 0 {
		t.Fatalf("provider Create called %d times after lookup failure", provider.creates)
	}
}
