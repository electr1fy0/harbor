package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"paas/internal/model"
	"paas/internal/store"

	_ "github.com/mattn/go-sqlite3"
)

func TestConsistentBackupVerifyRestoreAndUnlinkRelinkPreserveData(t *testing.T) {
	state := openServiceStore(t)
	manager := NewManager(state, t.TempDir())
	ctx := context.Background()

	svc, err := manager.AddService(ctx, "sqlite", "primary")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := manager.Link(ctx, "production", svc.Name)
	if err != nil {
		t.Fatal(err)
	}
	storedService, err := state.GetService(svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	databasePath, err := DatabasePath(binding, storedService)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", databaseURI(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE records (value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO records(value) VALUES ('wal-committed-value')`); err != nil {
		t.Fatal(err)
	}
	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal mode = %q, want wal", journalMode)
	}
	if info, err := os.Lstat(databasePath + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("live WAL was not present: info=%v err=%v", info, err)
	}

	backup, err := manager.CreateBackup(ctx, "production", svc.Name, false)
	if err != nil {
		t.Fatal(err)
	}
	if backup.Verification != "verified" || backup.SizeBytes <= 0 || backup.ID == "" {
		t.Fatalf("backup metadata = %+v", backup)
	}
	verified, err := manager.VerifyBackup(ctx, backup.ID)
	if err != nil || verified.Verification != "verified" {
		t.Fatalf("VerifyBackup() = %+v, %v", verified, err)
	}

	restored, err := manager.RestoreBackup(ctx, backup.ID, "recovered", svc.Name)
	if err != nil {
		t.Fatal(err)
	}
	if restored.AppID == binding.AppID || restored.DatabaseName == binding.DatabaseName {
		t.Fatalf("restore reused production binding identity: production=%+v restored=%+v", binding, restored)
	}
	restoredPath, err := DatabasePath(restored, storedService)
	if err != nil {
		t.Fatal(err)
	}
	if got := readDatabaseRow(t, restoredPath); got != "wal-committed-value" {
		t.Fatalf("restored value = %q", got)
	}
	if got := readDatabaseRow(t, databasePath); got != "wal-committed-value" {
		t.Fatalf("production data changed during restore: %q", got)
	}
	if _, err := manager.RestoreBackup(ctx, backup.ID, "production", svc.Name); err == nil {
		t.Fatal("restore overwrote an existing production app")
	}

	if err := manager.Unlink(ctx, "production", svc.Name); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(databasePath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("unlink removed durable database: info=%v err=%v", info, err)
	}
	relinked, err := manager.Link(ctx, "production", svc.Name)
	if err != nil {
		t.Fatal(err)
	}
	relinkedPath, err := DatabasePath(relinked, storedService)
	if err != nil {
		t.Fatal(err)
	}
	if relinkedPath != databasePath || readDatabaseRow(t, relinkedPath) != "wal-committed-value" {
		t.Fatal("relink did not reconnect to the original durable database")
	}

	services, err := manager.DataServiceList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || len(services[0].Bindings) != 2 || services[0].DatabaseBytes <= 0 || services[0].WALBytes <= 0 {
		t.Fatalf("safe data service summary = %+v", services)
	}
}

func TestBackupVerificationMarksCorruptSnapshotFailed(t *testing.T) {
	state := openServiceStore(t)
	manager := NewManager(state, t.TempDir())
	ctx := context.Background()
	svc, err := manager.AddService(ctx, "sqlite", "verify")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Link(ctx, "verify-app", svc.Name); err != nil {
		t.Fatal(err)
	}
	backup, err := manager.CreateBackup(ctx, "verify-app", svc.Name, false)
	if err != nil {
		t.Fatal(err)
	}
	storedService, err := state.GetService(svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := backupSnapshotPath(storedService, backup.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged-sqlite-file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifyBackup(ctx, backup.ID); err == nil {
		t.Fatal("verification accepted a damaged backup")
	}
	listed, _, err := state.DataBackup(backup.ID)
	if err != nil {
		t.Fatal(err)
	}
	if listed.Verification != "failed" || listed.Error == "" {
		t.Fatalf("failed verification metadata = %+v", listed)
	}
	services, err := manager.DataServiceList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || len(services[0].Backups) != 1 || services[0].Backups[0].Verification != "failed" {
		t.Fatalf("list view replaced failed backup status: %+v", services)
	}
}

func TestCanceledBackupVerificationDoesNotMarkSnapshotFailed(t *testing.T) {
	state := openServiceStore(t)
	manager := NewManager(state, t.TempDir())
	ctx := context.Background()
	svc, err := manager.AddService(ctx, "sqlite", "verify-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Link(ctx, "verify-cancel-app", svc.Name); err != nil {
		t.Fatal(err)
	}
	backup, err := manager.CreateBackup(ctx, "verify-cancel-app", svc.Name, false)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := manager.VerifyBackup(canceled, backup.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("VerifyBackup() with canceled context = %v", err)
	}
	stored, _, err := state.DataBackup(backup.ID)
	if err != nil || stored.Verification != "verified" {
		t.Fatalf("canceled verification changed backup metadata: %+v %v", stored, err)
	}
}

func TestRestoredAppReservationNeverReusesAnExistingName(t *testing.T) {
	state := openServiceStore(t)
	manager := NewManager(state, t.TempDir())
	svc, err := manager.AddService(context.Background(), "sqlite", "restore-reservation")
	if err != nil {
		t.Fatal(err)
	}
	existing, err := state.CreateApp("restore-target")
	if err != nil {
		t.Fatal(err)
	}
	originalBinding, err := manager.Link(context.Background(), existing.Name, svc.Name)
	if err != nil {
		t.Fatal(err)
	}
	candidateID := "app_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	candidate := model.Application{ID: candidateID, Name: existing.Name, CreatedAt: time.Now().UTC()}
	binding := model.Binding{ID: "bind_bbbbbbbbbbbbbbbb", AppID: candidateID, ServiceID: svc.ID,
		DatabaseName: "db_" + candidateID, EnvKey: "DATABASE_URL", CreatedAt: time.Now().UTC()}
	created, err := state.CreateRestoredApp(candidate, binding)
	if err != nil || created {
		t.Fatalf("CreateRestoredApp() = %v, %v; want a conflict without creation", created, err)
	}
	bindings, err := state.GetBindingsForApp(existing.ID)
	if err != nil || len(bindings) != 1 || bindings[0].ID != originalBinding.ID {
		t.Fatalf("existing app binding changed: %+v %v", bindings, err)
	}
	if _, err := state.AppSettings(existing.Name); err != nil {
		t.Fatalf("existing app settings disappeared: %v", err)
	}
}

func TestScheduledBackupsAndRetentionArePersisted(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.sqlite")
	state, err := store.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	dataDir := t.TempDir()
	manager := NewManager(state, dataDir)
	ctx := context.Background()
	svc, err := manager.AddService(ctx, "sqlite", "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Link(ctx, "scheduled-app", svc.Name); err != nil {
		t.Fatal(err)
	}
	policy, err := manager.SetBackupPolicy(ctx, svc.Name, "hourly", 1, true)
	if err != nil || !policy.Enabled || policy.Retention != 1 {
		t.Fatalf("SetBackupPolicy() = %+v, %v", policy, err)
	}
	if err := manager.RunScheduledBackups(ctx); err != nil {
		t.Fatal(err)
	}
	if err := manager.RunScheduledBackups(ctx); err != nil {
		t.Fatal(err)
	}
	backups, err := state.DataBackups(svc.ID)
	if err != nil || len(backups) != 1 || !backups[0].Scheduled {
		t.Fatalf("scheduled backups after duplicate pass = %+v, %v", backups, err)
	}
	manual, err := manager.CreateBackup(ctx, "scheduled-app", svc.Name, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateBackup(ctx, "scheduled-app", svc.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateBackup(ctx, "scheduled-app", svc.Name, true); err != nil {
		t.Fatal(err)
	}
	backups, err = state.DataBackups(svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	var manualCount, scheduledCount int
	for _, backup := range backups {
		if backup.Scheduled {
			scheduledCount++
		} else if backup.ID == manual.ID {
			manualCount++
		}
	}
	if scheduledCount != 1 || manualCount != 1 {
		t.Fatalf("retention removed the wrong backups: %+v", backups)
	}

	reopened, err := store.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	storedPolicy, err := reopened.DataBackupPolicy(svc.ID)
	if err != nil || storedPolicy.Frequency != "hourly" || storedPolicy.Retention != 1 {
		t.Fatalf("reopened backup policy = %+v, %v", storedPolicy, err)
	}
	storedBackups, err := reopened.DataBackups(svc.ID)
	if err != nil || len(storedBackups) != 2 {
		t.Fatalf("reopened backups = %+v, %v", storedBackups, err)
	}
}

func TestStartBackupsRunsOnAndStopsWithLifecycleContext(t *testing.T) {
	state := openServiceStore(t)
	manager := NewManager(state, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, err := manager.AddService(context.Background(), "sqlite", "background")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Link(context.Background(), "background-app", svc.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetBackupPolicy(context.Background(), svc.Name, "hourly", 1, true); err != nil {
		t.Fatal(err)
	}
	manager.StartBackups(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		backups, err := state.DataBackups(svc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(backups) == 1 && backups[0].Scheduled {
			cancel()
			manager.backupLoopMu.Lock()
			started := manager.backupLoopStarted
			manager.backupLoopMu.Unlock()
			if !started {
				t.Fatal("background backup loop did not record startup")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background backup loop did not create its due snapshot")
}

func TestEncryptedHarborExportSnapshotsSelectedDataAndRequiredKeys(t *testing.T) {
	state := openServiceStore(t)
	dataDir := t.TempDir()
	manager := NewManager(state, dataDir)
	ctx := context.Background()
	svc, err := manager.AddService(ctx, "sqlite", "exported")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := manager.Link(ctx, "selected-app", svc.Name)
	if err != nil {
		t.Fatal(err)
	}
	storedService, _ := state.GetService(svc.ID)
	databasePath, _ := DatabasePath(binding, storedService)
	writeDatabaseRow(t, databaseURI(databasePath), "exported-db-data")

	if err := os.WriteFile(filepath.Join(dataDir, "environment.key"), bytes.Repeat([]byte{0x21}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	observabilityPath := filepath.Join(dataDir, "observability.sqlite")
	obs, err := sql.Open("sqlite3", observabilityPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := obs.Exec(`CREATE TABLE events (value TEXT); INSERT INTO events(value) VALUES ('protected-observability')`); err != nil {
		_ = obs.Close()
		t.Fatal(err)
	}
	if err := obs.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "observability.key"), bytes.Repeat([]byte{0x31}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "github"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "github", "github-connection.key"), bytes.Repeat([]byte{0x41}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "github", "github-connection.json"), []byte(`{"login":"example"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "onboarding-automation.key"), bytes.Repeat([]byte{0x51}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "onboarding-automation.enc"), []byte("protected-automation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "onboarding-sources.json"), []byte(`{"sources":{}}`), 0600); err != nil {
		t.Fatal(err)
	}

	export, err := manager.ExportEncrypted(ctx, "long-enough-export-passphrase", []string{"selected-app"})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := base64.StdEncoding.DecodeString(export.EncryptedData)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := decryptHarborArchive("long-enough-export-passphrase", encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decryptHarborArchive("incorrect-passphrase-value", encrypted); err == nil {
		t.Fatal("export accepted a wrong passphrase")
	}
	compressed, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	archive := tar.NewReader(compressed)
	members := make(map[string][]byte)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg || strings.HasPrefix(header.Name, "/") || strings.Contains(header.Name, "..") {
			t.Fatalf("unsafe archive member: %+v", header)
		}
		data, err := io.ReadAll(io.LimitReader(archive, maxExportBytes+1))
		if err != nil {
			t.Fatal(err)
		}
		members[header.Name] = data
	}
	for _, name := range []string{
		"control/control.sqlite", "manifest.json", "app-data/" + binding.AppID + "/binding-01.sqlite",
		"keys/environment.key", "keys/observability.key", "control/observability.sqlite",
		"keys/github-connection.key", "credentials/github-connection.json",
		"keys/onboarding-automation.key", "control/onboarding-automation.enc", "metadata/onboarding-sources.json",
	} {
		if len(members[name]) == 0 {
			t.Fatalf("export omitted %q", name)
		}
	}
	if _, err := state.GetService(svc.ID); err != nil {
		t.Fatal(err)
	}
	if err := assertDatabaseRow(databaseURI(databasePath), "exported-db-data"); err != nil {
		t.Fatalf("export modified source database: %v", err)
	}
}

func TestEncryptedHarborExportRejectsSymlinkedKeyFiles(t *testing.T) {
	state := openServiceStore(t)
	dataDir := t.TempDir()
	manager := NewManager(state, dataDir)
	outside := filepath.Join(t.TempDir(), "outside.key")
	secret := []byte("must-not-be-followed")
	if err := os.WriteFile(outside, secret, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dataDir, "environment.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ExportEncrypted(context.Background(), "long-enough-export-passphrase", nil); err == nil {
		t.Fatal("export followed a symlinked encryption key")
	}
}

func readDatabaseRow(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", databaseURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow(`SELECT value FROM records`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
