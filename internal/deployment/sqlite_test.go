package deployment

import (
	"context"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"paas/internal/model"
	managedservice "paas/internal/service"
)

func TestDeployInjectsWritableSQLiteBindingAtSamePath(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	_, svc, _, databasePath := addSQLiteBinding(t, h, "svc_sqlite", "DATABASE_URL", true)

	d, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if d.State != model.Running {
		t.Fatalf("Deploy() state = %q, want %q", d.State, model.Running)
	}

	h.sandbox.mu.Lock()
	defer h.sandbox.mu.Unlock()
	if len(h.sandbox.specs) != 1 {
		t.Fatalf("sandbox starts = %d, want 1", len(h.sandbox.specs))
	}
	spec := h.sandbox.specs[0]
	gotURL := envValue(spec.Env, "DATABASE_URL")
	parsedURL, err := url.Parse(gotURL)
	if err != nil || parsedURL.Scheme != "file" || parsedURL.Path != databasePath {
		t.Fatalf("DATABASE_URL = %q, parsed=%+v, err=%v; want SQLite file URI for %q", gotURL, parsedURL, err, databasePath)
	}
	if !strings.Contains(parsedURL.RawQuery, "mode=rw") || !strings.Contains(parsedURL.RawQuery, "_busy_timeout=5000") || !strings.Contains(parsedURL.RawQuery, "_foreign_keys=on") {
		t.Fatalf("DATABASE_URL query = %q; missing required SQLite settings", parsedURL.RawQuery)
	}
	if len(spec.Binds) != 1 {
		t.Fatalf("bind mounts = %+v, want one SQLite binding directory", spec.Binds)
	}
	bind := spec.Binds[0]
	if bind.HostPath != filepath.Dir(databasePath) || bind.MountPath != bind.HostPath || bind.ReadOnly {
		t.Fatalf("SQLite bind = %+v, want writable directory mounted at its host path %q", bind, filepath.Dir(databasePath))
	}
	if svc.Port != 0 {
		t.Fatalf("SQLite service port = %d, want no port", svc.Port)
	}

	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		runtimeUser, err := user.Lookup("nobody")
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Dir(databasePath))
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(mustAtoi(t, runtimeUser.Uid)) || info.Mode().Perm() != 0700 {
			t.Fatalf("SQLite binding directory ownership/mode = %+v, want nobody and 0700", info)
		}
		fileInfo, err := os.Stat(databasePath)
		if err != nil {
			t.Fatal(err)
		}
		fileStat, ok := fileInfo.Sys().(*syscall.Stat_t)
		if !ok || fileStat.Uid != uint32(mustAtoi(t, runtimeUser.Uid)) || fileInfo.Mode().Perm() != 0600 {
			t.Fatalf("SQLite database ownership/mode = %+v, want nobody and 0600", fileInfo)
		}
	}
}

func TestDeployDoesNotMountUnboundSQLiteService(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	volume := filepath.Join(h.service.opts.DataDir, "volumes", "svc_unbound", "data")
	if err := os.MkdirAll(volume, 0711); err != nil {
		t.Fatal(err)
	}
	if err := h.db.CreateService(model.Service{
		ID: "svc_unbound", Name: "unbound", Type: "sqlite", State: model.ServiceReady,
		VolumeDir: volume, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Deploy(context.Background(), h.source, "demo"); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	h.sandbox.mu.Lock()
	defer h.sandbox.mu.Unlock()
	if len(h.sandbox.specs) != 1 || len(h.sandbox.specs[0].Binds) != 0 {
		t.Fatalf("sandbox specs = %+v, want no bind mounts", h.sandbox.specs)
	}
	if got := envValue(h.sandbox.specs[0].Env, "DATABASE_URL"); got != "<missing>" {
		t.Fatalf("DATABASE_URL = %q, want no injected binding", got)
	}
}

func TestDeployRejectsDuplicateSQLiteEnvironmentKeys(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	addSQLiteBinding(t, h, "svc_first", "DATABASE_URL", true)
	addSQLiteBinding(t, h, "svc_second", "DATABASE_URL", true)

	d, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err == nil || d.State != model.StartFailed || !strings.Contains(err.Error(), "multiple managed service bindings") {
		t.Fatalf("Deploy() = %+v, %v; want duplicate environment key failure", d, err)
	}
	h.sandbox.mu.Lock()
	defer h.sandbox.mu.Unlock()
	if len(h.sandbox.specs) != 0 {
		t.Fatalf("sandbox starts = %d, want no start with ambiguous DATABASE_URL", len(h.sandbox.specs))
	}
}

func TestDeployEscapesSQLiteBindingPathInURI(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	app, err := h.db.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	volume := filepath.Join(h.service.opts.DataDir, "volumes", "sqlite #? service", "data")
	if err := os.MkdirAll(volume, 0711); err != nil {
		t.Fatal(err)
	}
	svc := model.Service{
		ID: "svc_escaped", Name: "escaped", Type: "sqlite", State: model.ServiceReady,
		VolumeDir: volume, CreatedAt: time.Now().UTC(),
	}
	if err := h.db.CreateService(svc); err != nil {
		t.Fatal(err)
	}
	binding := model.Binding{
		ID: "binding_escaped", AppID: app.ID, ServiceID: svc.ID, DatabaseName: "db_" + app.ID,
		EnvKey: "DATABASE_URL", CreatedAt: time.Now().UTC(),
	}
	if err := h.db.CreateBinding(binding); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(volume, binding.DatabaseName, "database.sqlite")
	if err := os.MkdirAll(filepath.Dir(databasePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(databasePath, []byte("sqlite fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Deploy(context.Background(), h.source, "demo"); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	h.sandbox.mu.Lock()
	defer h.sandbox.mu.Unlock()
	got := envValue(h.sandbox.specs[0].Env, "DATABASE_URL")
	parsed, err := url.Parse(got)
	if err != nil || parsed.Path != databasePath || !strings.Contains(got, "%23%3F") || strings.Contains(parsed.RawPath, " ") {
		t.Fatalf("DATABASE_URL = %q, parsed=%+v, err=%v; want escaped URI for %q", got, parsed, err, databasePath)
	}
}

func TestDeployFailsInsteadOfRecreatingMissingSQLiteFile(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	_, _, _, _ = addSQLiteBinding(t, h, "svc_missing", "DATABASE_URL", false)

	d, err := h.service.Deploy(context.Background(), h.source, "demo")
	if err == nil || d == nil || d.State != model.StartFailed {
		t.Fatalf("Deploy() = %+v, %v; want a persisted START_FAILED attempt", d, err)
	}
	if !strings.Contains(err.Error(), "database.sqlite") {
		t.Fatalf("Deploy() error = %v, want missing database path", err)
	}
	h.sandbox.mu.Lock()
	defer h.sandbox.mu.Unlock()
	if len(h.sandbox.specs) != 0 {
		t.Fatalf("sandbox starts = %d, want no start with a missing database", len(h.sandbox.specs))
	}
}

func TestDeployRejectsNonReadyAndLegacyPostgresBindings(t *testing.T) {
	t.Run("not ready", func(t *testing.T) {
		h := newServiceHarness(t, testConfig)
		_, _, _, _ = addSQLiteBinding(t, h, "svc_stopped", "DATABASE_URL", true)
		if err := h.db.UpdateServiceState("svc_stopped", model.ServiceStopped); err != nil {
			t.Fatal(err)
		}
		d, err := h.service.Deploy(context.Background(), h.source, "demo")
		if err == nil || d.State != model.StartFailed || !strings.Contains(err.Error(), "not ready") {
			t.Fatalf("Deploy() = %+v, %v; want a not-ready failure", d, err)
		}
	})
	t.Run("legacy PostgreSQL", func(t *testing.T) {
		h := newServiceHarness(t, testConfig)
		app, err := h.db.CreateApp("demo")
		if err != nil {
			t.Fatal(err)
		}
		if err := h.db.CreateService(model.Service{
			ID: "svc_postgres", Name: "legacy", Type: "postgres", Port: 5432,
			State: model.ServiceReady, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := h.db.CreateBinding(model.Binding{
			ID: "binding_postgres", AppID: app.ID, ServiceID: "svc_postgres", DatabaseName: "legacydb",
			Username: "legacy", Password: "legacy-secret", EnvKey: "DATABASE_URL", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		d, err := h.service.Deploy(context.Background(), h.source, "demo")
		if err == nil || d.State != model.StartFailed || !strings.Contains(err.Error(), "migrate its PostgreSQL data") {
			t.Fatalf("Deploy() = %+v, %v; want explicit PostgreSQL migration failure", d, err)
		}
	})
}

func TestRedactionSupportsSQLiteAndUnavailableLegacyPostgres(t *testing.T) {
	h := newServiceHarness(t, testConfig)
	app, err := h.db.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	volume := filepath.Join(h.service.opts.DataDir, "volumes", "svc_sqlite_redact", "data")
	databaseDir := filepath.Join(volume, "db_"+app.ID)
	if err := os.MkdirAll(databaseDir, 0700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(databaseDir, "database.sqlite")
	if err := os.WriteFile(databasePath, []byte("sqlite fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	sqliteService := model.Service{
		ID: "svc_sqlite_redact", Name: "sqlite", Type: "sqlite", State: model.ServiceReady,
		VolumeDir: volume, CreatedAt: time.Now().UTC(),
	}
	if err := h.db.CreateService(sqliteService); err != nil {
		t.Fatal(err)
	}
	sqliteBinding := model.Binding{
		ID: "binding_sqlite_redact", AppID: app.ID, ServiceID: "svc_sqlite_redact", DatabaseName: "db_" + app.ID,
		EnvKey: "DATABASE_URL", CreatedAt: time.Now().UTC(),
	}
	if err := h.db.CreateBinding(sqliteBinding); err != nil {
		t.Fatal(err)
	}
	// Old PostgreSQL bindings may refer to services that have no usable port;
	// their stored password is still redacted and Diagnostics remains readable.
	if err := h.db.CreateService(model.Service{
		ID: "svc_pg_unavailable", Name: "legacy", Type: "postgres", State: model.ServiceFailed,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.CreateBinding(model.Binding{
		ID: "binding_pg_unavailable", AppID: app.ID, ServiceID: "svc_pg_unavailable", DatabaseName: "olddb",
		Username: "olduser", Password: "old-password", EnvKey: "DATABASE_URL_OLD", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	databaseURL, err := managedservice.DatabaseURL(&sqliteBinding, &sqliteService)
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.service.redactText("demo", "old-password "+databaseURL)
	if err != nil {
		t.Fatalf("redactText() error = %v", err)
	}
	if strings.Contains(got, "old-password") || strings.Contains(got, databaseURL) {
		t.Fatalf("redactText() leaked a binding value: %q", got)
	}
	if _, err := h.service.Diagnostics("demo", true); err != nil {
		t.Fatalf("Diagnostics() with unavailable legacy PostgreSQL service: %v", err)
	}
}

func TestSQLiteRuntimeAccessRejectsFIFOAndHardlinkedFiles(t *testing.T) {
	for _, scenario := range []string{"FIFO sidecar", "hardlinked database"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			databaseDir := filepath.Join(root, "binding")
			if err := os.Mkdir(databaseDir, 0700); err != nil {
				t.Fatal(err)
			}
			databasePath := filepath.Join(databaseDir, "database.sqlite")
			if err := os.WriteFile(databasePath, []byte("database"), 0600); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "FIFO sidecar":
				if err := syscall.Mkfifo(databasePath+"-wal", 0600); err != nil {
					t.Skipf("cannot create FIFO on this platform: %v", err)
				}
			case "hardlinked database":
				if err := os.Link(databasePath, filepath.Join(root, "other-link")); err != nil {
					t.Skipf("cannot create hard link on this filesystem: %v", err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- assignSQLiteBinding(databasePath, os.Geteuid(), os.Getegid()) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("assignSQLiteBinding() succeeded for unsafe file")
				}
			case <-ctx.Done():
				t.Fatal("assignSQLiteBinding() blocked opening a nonregular sidecar")
			}
		})
	}
}

func addSQLiteBinding(t *testing.T, h *serviceHarness, id, envKey string, createFile bool) (model.Application, model.Service, model.Binding, string) {
	t.Helper()
	app, err := h.db.CreateApp("demo")
	if err != nil {
		t.Fatal(err)
	}
	volume := filepath.Join(h.service.opts.DataDir, "volumes", id, "data")
	if err := os.MkdirAll(volume, 0711); err != nil {
		t.Fatal(err)
	}
	svc := model.Service{
		ID: id, Name: strings.TrimPrefix(id, "svc_"), Type: "sqlite", State: model.ServiceReady,
		VolumeDir: volume, CreatedAt: time.Now().UTC(),
	}
	if err := h.db.CreateService(svc); err != nil {
		t.Fatal(err)
	}
	binding := model.Binding{
		ID: "binding_" + strings.TrimPrefix(id, "svc_"), AppID: app.ID, ServiceID: id,
		DatabaseName: "db_" + app.ID, EnvKey: envKey, CreatedAt: time.Now().UTC(),
	}
	if err := h.db.CreateBinding(binding); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(volume, binding.DatabaseName, "database.sqlite")
	if createFile {
		if err := os.MkdirAll(filepath.Dir(databasePath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(databasePath, []byte("sqlite fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return *app, svc, binding, databasePath
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()
	parsed, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
