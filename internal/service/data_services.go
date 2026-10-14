package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
	"paas/internal/config"
	"paas/internal/model"
	"paas/internal/store"

	_ "github.com/mattn/go-sqlite3"
)

const (
	maxBackupBytes         = int64(2 << 30)
	maxDownloadBackupBytes = int64(2 << 20)
	maxExportBytes         = 48 << 20
	maxExportApps          = 20
	maxExportAppDBBytes    = 24 << 20
	maxExportAppDBTotal    = 40 << 20
	maxExportControlDB     = 24 << 20
	maxExportObserveDB     = 16 << 20
	maxArchiveEntries      = 128
	maxPassphraseBytes     = 1024
	minPassphraseBytes     = 12
	backupStaleAfter       = 30 * 24 * time.Hour
	scheduledBackupTick    = time.Minute
	scryptN                = 1 << 15
	scryptR                = 8
	scryptP                = 1
)

var backupIDPattern = regexp.MustCompile(`^bkp_[a-f0-9]{16}$`)
var serviceIDPattern = regexp.MustCompile(`^svc_[a-f0-9]{16}$`)
var appIDPattern = regexp.MustCompile(`^app_[a-f0-9]{32}$`)

// DataServiceList returns user-safe service views, including binding and
// storage metrics without exposing any filesystem paths.
func (m *Manager) DataServiceList(ctx context.Context) ([]model.DataServiceSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	services, err := m.store.ListServices()
	if err != nil {
		return nil, err
	}
	result := make([]model.DataServiceSummary, 0, len(services))
	for _, svc := range services {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		summary := model.DataServiceSummary{
			ID: svc.ID, Name: svc.Name, Type: svc.Type, Version: svc.Version,
			State: svc.State, CreatedAt: svc.CreatedAt, Bindings: make([]model.DataBinding, 0),
			Backups: make([]model.DataBackup, 0),
		}
		if svc.Type == "sqlite" {
			if err := m.validateManagedVolume(&svc); err != nil {
				return nil, err
			}
			bindings, appNames, err := m.store.BindingsForService(svc.ID)
			if err != nil {
				return nil, err
			}
			for _, binding := range bindings {
				path, err := bindingDatabasePath(&binding, &svc)
				if err != nil {
					return nil, fmt.Errorf("validate SQLite binding: %w", err)
				}
				dbBytes, walBytes, err := sqliteFileSizes(path)
				if err != nil {
					return nil, err
				}
				summary.DatabaseBytes += dbBytes
				summary.WALBytes += walBytes
				summary.Bindings = append(summary.Bindings, model.DataBinding{
					ID: binding.ID, AppName: appNames[binding.AppID], ServiceID: binding.ServiceID,
					CreatedAt: binding.CreatedAt, DatabaseBytes: dbBytes, WALBytes: walBytes,
				})
			}
			backups, err := m.store.DataBackups(svc.ID)
			if err != nil {
				return nil, err
			}
			for i := range backups {
				backups[i] = m.refreshBackupMetadata(backups[i], &svc)
			}
			summary.Backups = backups
			if policy, err := m.store.DataBackupPolicy(svc.ID); err == nil {
				summary.BackupPolicy = policy
			} else if !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
		}
		result = append(result, summary)
	}
	return result, nil
}

// Unlink removes only a link. The SQLite database and its backup files remain
// available for explicit relinking or recovery.
func (m *Manager) Unlink(ctx context.Context, appName, serviceName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	app, err := m.findApp(appName)
	if err != nil {
		return err
	}
	svc, err := m.store.GetService(serviceName)
	if err != nil {
		return err
	}
	if svc.Type != "sqlite" {
		return errors.New("only SQLite service bindings can be removed")
	}
	bindings, err := m.store.GetBindingsForApp(app.ID)
	if err != nil {
		return err
	}
	found := false
	for _, binding := range bindings {
		if binding.ServiceID == svc.ID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: service binding", store.ErrNotFound)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.store.DeleteBinding(app.ID, svc.ID)
}

// CreateBackup makes a standalone, consistent snapshot using SQLite VACUUM
// INTO, then verifies the resulting database before it becomes visible.
func (m *Manager) CreateBackup(ctx context.Context, appName, serviceName string, scheduled bool) (*model.DataBackup, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	app, err := m.findApp(appName)
	if err != nil {
		return nil, err
	}
	svc, binding, err := m.findBinding(app.ID, serviceName)
	if err != nil {
		return nil, err
	}
	if err := m.validateManagedVolume(svc); err != nil {
		return nil, err
	}
	databasePath, err := DatabasePath(binding, svc)
	if err != nil {
		return nil, fmt.Errorf("validate SQLite database: %w", err)
	}
	databaseBytes, walBytes, err := sqliteFileSizes(databasePath)
	if err != nil || databaseBytes+walBytes > maxBackupBytes {
		return nil, errors.New("SQLite database exceeds the 2 GiB backup limit or has unsafe sidecars")
	}
	backupDir, err := serviceBackupDir(svc, true)
	if err != nil {
		return nil, err
	}
	id, err := newBackupID()
	if err != nil {
		return nil, err
	}
	finalPath := filepath.Join(backupDir, id+".sqlite")
	temporaryPath := filepath.Join(backupDir, "."+id+".tmp.sqlite")
	if err := sqliteVacuumInto(ctx, databasePath, temporaryPath); err != nil {
		_ = os.Remove(temporaryPath)
		return nil, fmt.Errorf("create consistent SQLite backup: %w", err)
	}
	if err := os.Chmod(temporaryPath, 0600); err != nil {
		_ = os.Remove(temporaryPath)
		return nil, fmt.Errorf("secure SQLite backup: %w", err)
	}
	if err := validateSQLiteDatabase(ctx, temporaryPath); err != nil {
		_ = os.Remove(temporaryPath)
		return nil, fmt.Errorf("verify SQLite backup: %w", err)
	}
	info, err := os.Lstat(temporaryPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxBackupBytes {
		_ = os.Remove(temporaryPath)
		return nil, errors.New("SQLite backup size or file type is invalid")
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		_ = os.Remove(temporaryPath)
		return nil, fmt.Errorf("publish SQLite backup: %w", err)
	}
	now := time.Now().UTC()
	backup := model.DataBackup{
		ID: id, ServiceID: svc.ID, ServiceName: svc.Name, AppName: app.Name,
		SizeBytes: info.Size(), CreatedAt: now, Scheduled: scheduled,
		Verification: "verified", VerifiedAt: &now,
	}
	if err := m.store.CreateDataBackup(backup, app.ID); err != nil {
		_ = os.Remove(finalPath)
		return nil, err
	}
	if scheduled {
		if policy, err := m.store.DataBackupPolicy(svc.ID); err == nil {
			if err := m.pruneScheduledBackups(svc, app.ID, policy.Retention); err != nil {
				return &backup, err
			}
		}
	}
	return &backup, nil
}

// VerifyBackup performs a fresh SQLite quick_check and persists the result.
func (m *Manager) VerifyBackup(ctx context.Context, backupID string) (*model.DataBackup, error) {
	if !backupIDPattern.MatchString(backupID) {
		return nil, fmt.Errorf("%w: data backup", store.ErrNotFound)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	backup, _, err := m.store.DataBackup(backupID)
	if err != nil {
		return nil, err
	}
	svc, err := m.store.GetService(backup.ServiceID)
	if err != nil {
		return nil, err
	}
	if err := m.validateManagedVolume(svc); err != nil {
		return nil, err
	}
	path, err := backupSnapshotPath(svc, backup.ID)
	if err == nil {
		info, infoErr := os.Lstat(path)
		if infoErr != nil {
			err = infoErr
		} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != backup.SizeBytes {
			err = errors.New("backup snapshot file failed size or file type validation")
		} else if info.Size() <= 0 || info.Size() > maxBackupBytes {
			err = errors.New("backup snapshot size is outside the supported limit")
		} else {
			err = validateSQLiteDatabase(ctx, path)
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	now := time.Now().UTC()
	state, message := "verified", ""
	var verifiedAt *time.Time = &now
	if err != nil {
		state, message, verifiedAt = "failed", "backup snapshot is missing or failed SQLite integrity checks", nil
	}
	if updateErr := m.store.UpdateDataBackupVerification(backup.ID, state, message, verifiedAt); updateErr != nil {
		return nil, errors.Join(err, updateErr)
	}
	backup.Verification, backup.Error, backup.VerifiedAt = state, message, verifiedAt
	return backup, err
}

// DownloadBackup returns only a recently integrity-checked snapshot and keeps
// inline API responses within the local gateway's bounded response size.
func (m *Manager) DownloadBackup(ctx context.Context, backupID string) (*model.DataBackup, []byte, error) {
	backup, err := m.VerifyBackup(ctx, backupID)
	if err != nil {
		return nil, nil, err
	}
	if backup.SizeBytes > maxDownloadBackupBytes {
		return nil, nil, errors.New("backup exceeds the 2 MiB inline download limit")
	}
	svc, err := m.store.GetService(backup.ServiceID)
	if err != nil {
		return nil, nil, err
	}
	path, err := backupSnapshotPath(svc, backup.ID)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != backup.SizeBytes {
		return nil, nil, errors.New("backup snapshot is unavailable or changed")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, errors.New("backup snapshot could not be opened safely")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, nil, errors.New("backup snapshot changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDownloadBackupBytes+1))
	if err != nil || int64(len(data)) != backup.SizeBytes || int64(len(data)) > maxDownloadBackupBytes {
		return nil, nil, errors.New("backup snapshot exceeded the inline download limit")
	}
	return backup, data, nil
}

// SetBackupPolicy stores a per-service hourly, daily, or weekly policy.
func (m *Manager) SetBackupPolicy(ctx context.Context, serviceName, frequency string, retention int, enabled bool) (*model.BackupPolicy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	svc, err := m.store.GetService(serviceName)
	if err != nil {
		return nil, err
	}
	if svc.Type != "sqlite" {
		return nil, errors.New("scheduled backups are supported only for SQLite services")
	}
	if retention < 1 || retention > 100 {
		return nil, errors.New("backup retention must be between 1 and 100")
	}
	if enabled && frequency != "hourly" && frequency != "daily" && frequency != "weekly" {
		return nil, errors.New("backup frequency must be hourly, daily, or weekly")
	}
	if !enabled && frequency == "" {
		frequency = "daily"
	}
	if frequency != "hourly" && frequency != "daily" && frequency != "weekly" {
		return nil, errors.New("backup frequency must be hourly, daily, or weekly")
	}
	policy := model.BackupPolicy{ServiceID: svc.ID, Frequency: frequency, Retention: retention, Enabled: enabled, UpdatedAt: time.Now().UTC()}
	if err := m.store.SetDataBackupPolicy(policy); err != nil {
		return nil, err
	}
	return &policy, nil
}

// StartBackups starts the scheduled snapshot loop exactly once and ties its
// lifetime to the daemon's context.
func (m *Manager) StartBackups(ctx context.Context) {
	if ctx == nil {
		return
	}
	m.backupLoopMu.Lock()
	if m.backupLoopStarted {
		m.backupLoopMu.Unlock()
		return
	}
	m.backupLoopStarted = true
	m.backupLoopMu.Unlock()
	go func() {
		_ = m.RunScheduledBackups(ctx)
		ticker := time.NewTicker(scheduledBackupTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = m.RunScheduledBackups(ctx)
			}
		}
	}()
}

// RunScheduledBackups runs due policy work. It is public so lifecycle and
// deterministic tests can trigger the same scheduler pass.
func (m *Manager) RunScheduledBackups(ctx context.Context) error {
	policies, err := m.store.DataBackupPolicies()
	if err != nil {
		return err
	}
	var failures []error
	for _, policy := range policies {
		if !policy.Enabled {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		svc, err := m.store.GetService(policy.ServiceID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		bindings, appNames, err := m.store.BindingsForService(svc.ID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, binding := range bindings {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failures, err)...)
			}
			backups, err := m.store.DataBackups(svc.ID)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			latest := time.Time{}
			for _, backup := range backups {
				if backup.Scheduled && backup.AppName == appNames[binding.AppID] && backup.CreatedAt.After(latest) {
					latest = backup.CreatedAt
				}
			}
			interval, err := backupFrequencyDuration(policy.Frequency)
			if err != nil {
				failures = append(failures, err)
				break
			}
			if !latest.IsZero() && time.Since(latest) < interval {
				continue
			}
			backup, err := m.CreateBackup(ctx, appNames[binding.AppID], svc.Name, true)
			if err != nil {
				failures = append(failures, fmt.Errorf("scheduled backup for application %q failed: %w", appNames[binding.AppID], err))
				continue
			}
			_ = backup
		}
	}
	return errors.Join(failures...)
}

// RestoreBackup creates a new application and a distinct database binding
// populated from a verified snapshot. It never writes into an existing app.
func (m *Manager) RestoreBackup(ctx context.Context, backupID, newAppName, serviceName string) (*model.Binding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !backupIDPattern.MatchString(backupID) {
		return nil, fmt.Errorf("%w: data backup", store.ErrNotFound)
	}
	newAppName = strings.TrimSpace(newAppName)
	if !config.ValidName(newAppName) {
		return nil, errors.New("a valid new application name is required")
	}
	apps, err := m.store.Apps()
	if err != nil {
		return nil, err
	}
	for _, app := range apps {
		if app.Name == newAppName {
			return nil, errors.New("restore target application already exists; choose a new application name")
		}
	}
	backup, _, err := m.store.DataBackup(backupID)
	if err != nil {
		return nil, err
	}
	svc, err := m.store.GetService(serviceName)
	if err != nil {
		return nil, err
	}
	if svc.Type != "sqlite" || svc.State != model.ServiceReady {
		return nil, errors.New("restore target must be a ready SQLite service")
	}
	if err := m.validateManagedVolume(svc); err != nil {
		return nil, err
	}
	if backup.ServiceID != svc.ID {
		return nil, errors.New("backup belongs to a different SQLite service")
	}
	verified, verifyErr := m.VerifyBackup(ctx, backupID)
	if verifyErr != nil || verified.Verification != "verified" {
		return nil, errors.New("backup must pass integrity verification before restore")
	}
	backupPath, err := backupSnapshotPath(svc, backupID)
	if err != nil {
		return nil, err
	}
	appIDBytes := make([]byte, 16)
	if _, err := rand.Read(appIDBytes); err != nil {
		return nil, fmt.Errorf("generate restored application id: %w", err)
	}
	rawID := make([]byte, 8)
	if _, err := rand.Read(rawID); err != nil {
		return nil, fmt.Errorf("generate restore binding id: %w", err)
	}
	newAppID := "app_" + hex.EncodeToString(appIDBytes)
	binding := model.Binding{
		ID: "bind_" + hex.EncodeToString(rawID), AppID: newAppID, ServiceID: svc.ID,
		DatabaseName: "db_" + newAppID, EnvKey: "DATABASE_URL", CreatedAt: time.Now().UTC(),
	}
	targetPath, err := bindingDatabasePath(&binding, svc)
	if err != nil {
		return nil, err
	}
	m.bindingMgr.mu.Lock()
	defer m.bindingMgr.mu.Unlock()
	databaseDir := filepath.Dir(targetPath)
	if err := os.Mkdir(databaseDir, 0700); err != nil {
		return nil, fmt.Errorf("create restored application database directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(databaseDir)
		}
	}()
	stagedPath := targetPath + ".restore.tmp"
	if err := copyRegularFile(ctx, backupPath, stagedPath, maxBackupBytes); err != nil {
		return nil, fmt.Errorf("copy verified backup into new binding: %w", err)
	}
	if err := validateSQLiteDatabase(ctx, stagedPath); err != nil {
		_ = os.Remove(stagedPath)
		return nil, fmt.Errorf("verify restored SQLite database: %w", err)
	}
	if err := os.Rename(stagedPath, targetPath); err != nil {
		_ = os.Remove(stagedPath)
		return nil, fmt.Errorf("publish restored SQLite database: %w", err)
	}
	if _, err := DatabaseURL(&binding, svc); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	app := model.Application{ID: newAppID, Name: newAppName, CreatedAt: time.Now().UTC()}
	created, err := m.store.CreateRestoredApp(app, binding)
	if err != nil {
		return nil, err
	}
	if !created {
		return nil, errors.New("restore target application already exists; choose a new application name")
	}
	committed = true
	return &binding, nil
}

// ExportEncrypted returns a passphrase-protected Harbor archive. Export data
// is bounded to 48 MiB before encryption and includes consistent SQLite
// snapshots, selected application databases, and present encryption keys.
func (m *Manager) ExportEncrypted(ctx context.Context, passphrase string, appNames []string) (*model.DataServiceExport, error) {
	if !m.exportMu.TryLock() {
		return nil, errors.New("another encrypted Harbor export is already running")
	}
	defer m.exportMu.Unlock()
	if len(passphrase) < minPassphraseBytes || len(passphrase) > maxPassphraseBytes || strings.ContainsRune(passphrase, '\x00') {
		return nil, errors.New("export passphrase must be 12 to 1024 bytes")
	}
	if len(appNames) > maxExportApps {
		return nil, fmt.Errorf("at most %d applications may be included", maxExportApps)
	}
	selected := make(map[string]bool, len(appNames))
	for _, name := range appNames {
		if strings.TrimSpace(name) == "" || len(name) > 128 || selected[name] {
			return nil, errors.New("selected application names must be unique and valid")
		}
		selected[name] = true
	}
	if m.dataDir == "" || !filepath.IsAbs(m.dataDir) {
		return nil, errors.New("Harbor data directory is unavailable")
	}
	tempDir, err := os.MkdirTemp(m.dataDir, ".harbor-export-")
	if err != nil {
		return nil, fmt.Errorf("create private export workspace: %w", err)
	}
	defer os.RemoveAll(tempDir)
	if err := os.Chmod(tempDir, 0700); err != nil {
		return nil, fmt.Errorf("secure export workspace: %w", err)
	}
	entries := make(map[string]string)
	controlBytes, err := m.store.ControlDatabaseBytes()
	if err != nil {
		return nil, err
	}
	if controlBytes > maxExportControlDB {
		return nil, errors.New("control database exceeds the 24 MiB export limit")
	}
	controlPath := filepath.Join(tempDir, "control.sqlite")
	if err := m.store.SnapshotControlDB(ctx, controlPath); err != nil {
		return nil, err
	}
	entries["control/control.sqlite"] = controlPath
	manifest := struct {
		FormatVersion int       `json:"formatVersion"`
		CreatedAt     time.Time `json:"createdAt"`
		AppNames      []string  `json:"appNames"`
	}{FormatVersion: 1, CreatedAt: time.Now().UTC(), AppNames: append([]string(nil), appNames...)}
	manifestBytes, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(tempDir, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestBytes, 0600); err != nil {
		return nil, fmt.Errorf("write export manifest: %w", err)
	}
	entries["manifest.json"] = manifestPath

	apps, err := m.store.Apps()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(apps))
	var appDatabaseBytes int64
	for _, app := range apps {
		seen[app.Name] = true
		if !selected[app.Name] {
			continue
		}
		if !appIDPattern.MatchString(app.ID) {
			return nil, errors.New("selected app identity is invalid")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		bindings, err := m.store.GetBindingsForApp(app.ID)
		if err != nil {
			return nil, err
		}
		for index := range bindings {
			if len(entries) >= maxArchiveEntries {
				return nil, errors.New("export contains too many database files")
			}
			binding := &bindings[index]
			svc, err := m.store.GetService(binding.ServiceID)
			if err != nil {
				return nil, err
			}
			if svc.Type != "sqlite" {
				return nil, errors.New("selected app has a managed database format that Harbor cannot export")
			}
			if err := m.validateManagedVolume(svc); err != nil {
				return nil, err
			}
			sourcePath, err := DatabasePath(binding, svc)
			if err != nil {
				return nil, fmt.Errorf("validate selected app database: %w", err)
			}
			sourceDBBytes, sourceWALBytes, err := sqliteFileSizes(sourcePath)
			if err != nil || sourceDBBytes+sourceWALBytes > maxExportAppDBBytes {
				return nil, errors.New("selected app database exceeds the 24 MiB export limit or has unsafe sidecars")
			}
			name := filepath.ToSlash(filepath.Join("app-data", app.ID, fmt.Sprintf("binding-%02d.sqlite", index+1)))
			destination := filepath.Join(tempDir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
				return nil, fmt.Errorf("create private app snapshot directory: %w", err)
			}
			if err := sqliteVacuumInto(ctx, sourcePath, destination); err != nil {
				return nil, fmt.Errorf("snapshot selected app database: %w", err)
			}
			info, err := os.Lstat(destination)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxExportAppDBBytes {
				return nil, errors.New("selected app database exceeds export limits or is not a regular file")
			}
			appDatabaseBytes += info.Size()
			if appDatabaseBytes > maxExportAppDBTotal {
				return nil, errors.New("selected app databases exceed the 40 MiB export limit")
			}
			entries[name] = destination
		}
	}
	for name := range selected {
		if !seen[name] {
			return nil, fmt.Errorf("selected application %q was not found", name)
		}
	}

	environmentKeyIncluded, err := m.addPrivateRegularFile(filepath.Join(m.dataDir, "environment.key"), "keys/environment.key", entries, 4096)
	if err != nil {
		return nil, err
	}
	if hasEncrypted, err := m.store.HasEncryptedSettings(); err != nil {
		return nil, err
	} else if hasEncrypted && !environmentKeyIncluded {
		return nil, errors.New("application encryption key is unavailable; export would be incomplete")
	}
	githubKeyIncluded, err := m.addPrivateRegularFile(filepath.Join(m.dataDir, "github", "github-connection.key"), "keys/github-connection.key", entries, 4096)
	if err != nil {
		return nil, err
	}
	githubCredentialIncluded, err := m.addPrivateRegularFile(filepath.Join(m.dataDir, "github", "github-connection.json"), "credentials/github-connection.json", entries, 16<<10)
	if err != nil {
		return nil, err
	}
	if githubCredentialIncluded && !githubKeyIncluded {
		return nil, errors.New("GitHub credential encryption key is unavailable; export would be incomplete")
	}
	automationKeyIncluded, err := m.addPrivateRegularFile(filepath.Join(m.dataDir, "onboarding-automation.key"), "keys/onboarding-automation.key", entries, 4096)
	if err != nil {
		return nil, err
	}
	automationStateIncluded, err := m.addPrivateRegularFile(filepath.Join(m.dataDir, "onboarding-automation.enc"), "control/onboarding-automation.enc", entries, 32<<20)
	if err != nil {
		return nil, err
	}
	if automationStateIncluded && !automationKeyIncluded {
		return nil, errors.New("automation encryption key is unavailable; export would be incomplete")
	}
	if _, err := m.addPrivateRegularFile(filepath.Join(m.dataDir, "onboarding-sources.json"), "metadata/onboarding-sources.json", entries, 1<<20); err != nil {
		return nil, err
	}
	// Observability uses its own live SQLite database, so snapshot it through
	// SQLite and include its key instead of archiving the active database file.
	observabilityPath := filepath.Join(m.dataDir, "observability.sqlite")
	observabilityInfo, observabilityErr := os.Lstat(observabilityPath)
	observabilityKeyPath := filepath.Join(m.dataDir, "observability.key")
	observabilityKeyIncluded, keyErr := m.addPrivateRegularFile(observabilityKeyPath, "keys/observability.key", entries, 4096)
	if keyErr != nil {
		return nil, keyErr
	}
	if observabilityErr == nil {
		if observabilityInfo.Mode()&os.ModeSymlink != 0 || !observabilityInfo.Mode().IsRegular() || !observabilityKeyIncluded {
			return nil, errors.New("observability database encryption key is unavailable; export would be incomplete")
		}
		observeBytes, observeWALBytes, err := sqliteFileSizes(observabilityPath)
		if err != nil || observeBytes+observeWALBytes > maxExportObserveDB {
			return nil, errors.New("observability database exceeds the 16 MiB export limit or has unsafe sidecars")
		}
		snapshotPath := filepath.Join(tempDir, "observability.sqlite")
		if err := sqliteVacuumInto(ctx, observabilityPath, snapshotPath); err != nil {
			return nil, fmt.Errorf("snapshot observability database: %w", err)
		}
		entries["control/observability.sqlite"] = snapshotPath
	} else if !errors.Is(observabilityErr, os.ErrNotExist) {
		return nil, errors.New("observability database cannot be safely included in export")
	} else if observabilityKeyIncluded {
		return nil, errors.New("observability key has no matching database; export would be incomplete")
	}

	archive, err := makeBoundedArchive(ctx, entries, maxExportBytes)
	if err != nil {
		return nil, err
	}
	encrypted, err := encryptHarborArchive(passphrase, archive)
	if err != nil {
		return nil, err
	}
	return &model.DataServiceExport{
		FileName:    "harbor-export-" + time.Now().UTC().Format("20060102") + ".harbor.enc",
		ContentType: "application/vnd.harbor.export+encrypted", EncryptedData: base64.StdEncoding.EncodeToString(encrypted),
		SizeBytes: int64(len(encrypted)),
	}, nil
}

func (m *Manager) findApp(name string) (*model.Application, error) {
	if strings.TrimSpace(name) == "" || len(name) > 128 || strings.ContainsAny(name, "/\\\x00\r\n") {
		return nil, errors.New("application name is invalid")
	}
	apps, err := m.store.Apps()
	if err != nil {
		return nil, err
	}
	for index := range apps {
		if apps[index].Name == name {
			return &apps[index], nil
		}
	}
	return nil, fmt.Errorf("%w: application", store.ErrNotFound)
}

func (m *Manager) findBinding(appID, serviceName string) (*model.Service, *model.Binding, error) {
	svc, err := m.store.GetService(serviceName)
	if err != nil {
		return nil, nil, err
	}
	if svc.Type != "sqlite" {
		return nil, nil, errors.New("data backups are supported only for SQLite services")
	}
	bindings, err := m.store.GetBindingsForApp(appID)
	if err != nil {
		return nil, nil, err
	}
	for index := range bindings {
		if bindings[index].ServiceID == svc.ID {
			return svc, &bindings[index], nil
		}
	}
	return nil, nil, fmt.Errorf("%w: service binding", store.ErrNotFound)
}

func sqliteFileSizes(databasePath string) (int64, int64, error) {
	var dbBytes, walBytes int64
	for _, item := range []struct {
		path string
		into *int64
	}{{databasePath, &dbBytes}, {databasePath + "-wal", &walBytes}} {
		info, err := os.Lstat(item.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return 0, 0, errors.New("SQLite database storage contains an unsafe file")
		}
		*item.into = info.Size()
	}
	return dbBytes, walBytes, nil
}

func serviceBackupDir(svc *model.Service, create bool) (string, error) {
	if svc == nil || svc.Type != "sqlite" || !serviceIDPattern.MatchString(svc.ID) {
		return "", errors.New("SQLite service identity is invalid")
	}
	volumeDir := filepath.Clean(svc.VolumeDir)
	if !filepath.IsAbs(volumeDir) || volumeDir == string(filepath.Separator) || volumeDir != svc.VolumeDir {
		return "", errors.New("SQLite service volume is invalid")
	}
	volumeInfo, err := os.Lstat(volumeDir)
	if err != nil || volumeInfo.Mode()&os.ModeSymlink != 0 || !volumeInfo.IsDir() {
		return "", errors.New("SQLite service volume is unavailable")
	}
	backupDir := filepath.Join(volumeDir, "backups")
	info, err := os.Lstat(backupDir)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := os.Mkdir(backupDir, 0700); err != nil {
			return "", fmt.Errorf("create SQLite backup directory: %w", err)
		}
		info, err = os.Lstat(backupDir)
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("SQLite backup directory is unsafe")
	}
	if info.Mode().Perm()&0077 != 0 {
		if err := os.Chmod(backupDir, 0700); err != nil {
			return "", errors.New("SQLite backup directory permissions are unsafe")
		}
	}
	return backupDir, nil
}

func (m *Manager) validateManagedVolume(svc *model.Service) error {
	if svc == nil || m.dataDir == "" || !filepath.IsAbs(m.dataDir) || !serviceIDPattern.MatchString(svc.ID) {
		return errors.New("SQLite service volume identity is invalid")
	}
	dataDir := filepath.Clean(m.dataDir)
	if dataDir == string(filepath.Separator) {
		return errors.New("SQLite service data directory is invalid")
	}
	expected := filepath.Join(dataDir, "volumes", svc.ID)
	if filepath.Clean(svc.VolumeDir) != expected || svc.VolumeDir != expected {
		return errors.New("SQLite service volume is outside its managed storage directory")
	}
	for _, dir := range []string{dataDir, filepath.Join(dataDir, "volumes"), expected} {
		info, err := os.Lstat(dir)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("SQLite service volume is unavailable or unsafe")
		}
	}
	markerPath := filepath.Join(expected, ".harbor-sqlite-service")
	markerInfo, err := os.Lstat(markerPath)
	if err != nil || markerInfo.Mode()&os.ModeSymlink != 0 || !markerInfo.Mode().IsRegular() {
		return errors.New("SQLite service volume identity marker is unavailable")
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil || strings.TrimSpace(string(marker)) != svc.ID {
		return errors.New("SQLite service volume identity marker is invalid")
	}
	return nil
}

func backupSnapshotPath(svc *model.Service, id string) (string, error) {
	if !backupIDPattern.MatchString(id) {
		return "", errors.New("backup identity is invalid")
	}
	dir, err := serviceBackupDir(svc, false)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".sqlite"), nil
}

func newBackupID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate backup id: %w", err)
	}
	return "bkp_" + hex.EncodeToString(raw[:]), nil
}

func sqliteVacuumInto(ctx context.Context, sourcePath, destination string) error {
	info, err := os.Lstat(sourcePath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 {
		return errors.New("SQLite source is not a regular database file")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("SQLite snapshot destination already exists")
	}
	db, err := sql.Open("sqlite3", databaseURI(sourcePath))
	if err != nil {
		return fmt.Errorf("open SQLite database for snapshot: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to SQLite database for snapshot: %w", err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, destination); err != nil {
		return fmt.Errorf("SQLite VACUUM INTO failed: %w", err)
	}
	return ctx.Err()
}

func copyRegularFile(ctx context.Context, source, destination string, maxBytes int64) error {
	info, err := os.Lstat(source)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxBytes {
		return errors.New("source file is missing or outside the supported size limit")
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	openedInfo, err := in.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) || openedInfo.Size() != info.Size() || openedInfo.Size() > maxBytes {
		return errors.New("source file changed while opening")
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	copied, copyErr := io.Copy(out, io.LimitReader(in, maxBytes+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || copied != info.Size() || copied > maxBytes {
		_ = os.Remove(destination)
		return errors.Join(copyErr, closeErr, errors.New("source file size changed while copying"))
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return nil
}

func backupFrequencyDuration(frequency string) (time.Duration, error) {
	switch frequency {
	case "hourly":
		return time.Hour, nil
	case "daily":
		return 24 * time.Hour, nil
	case "weekly":
		return 7 * 24 * time.Hour, nil
	default:
		return 0, errors.New("stored backup frequency is invalid")
	}
}

func (m *Manager) pruneScheduledBackups(svc *model.Service, appID string, retention int) error {
	bindings, appNames, err := m.store.BindingsForService(svc.ID)
	if err != nil {
		return err
	}
	appName := appNames[appID]
	if appName == "" {
		// The just-created record might be for a binding that was concurrently
		// unlinked; keep snapshots in that case rather than guessing ownership.
		return nil
	}
	backups, err := m.store.DataBackups(svc.ID)
	if err != nil {
		return err
	}
	filtered := make([]model.DataBackup, 0)
	for _, backup := range backups {
		if backup.Scheduled && backup.AppName == appName {
			filtered = append(filtered, backup)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].CreatedAt.After(filtered[j].CreatedAt) })
	for _, backup := range filtered[min(retention, len(filtered)):] {
		path, err := backupSnapshotPath(svc, backup.ID)
		if err != nil {
			return err
		}
		if info, err := os.Lstat(path); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return errors.New("refusing to prune an unsafe backup file")
			}
			if err := os.Remove(path); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := m.store.DeleteDataBackup(backup.ID); err != nil {
			return err
		}
	}
	_ = bindings
	return nil
}

func (m *Manager) refreshBackupMetadata(backup model.DataBackup, svc *model.Service) model.DataBackup {
	path, err := backupSnapshotPath(svc, backup.ID)
	if err == nil {
		var info os.FileInfo
		info, err = os.Lstat(path)
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != backup.SizeBytes) {
			err = errors.New("backup snapshot metadata does not match its file")
		}
	}
	if err != nil {
		backup.Verification = "failed"
		backup.Error = "backup snapshot is unavailable or has changed"
		backup.VerifiedAt = nil
		_ = m.store.UpdateDataBackupVerification(backup.ID, backup.Verification, backup.Error, nil)
		return backup
	}
	if backup.Verification != "failed" && (backup.Verification != "verified" || backup.VerifiedAt == nil || time.Since(*backup.VerifiedAt) > backupStaleAfter) {
		backup.Verification = "stale"
		backup.Error = "backup has not been integrity checked recently"
		_ = m.store.UpdateDataBackupVerification(backup.ID, backup.Verification, backup.Error, backup.VerifiedAt)
	}
	return backup
}

func (m *Manager) addPrivateRegularFile(source, archiveName string, entries map[string]string, maxBytes int64) (bool, error) {
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxBytes {
		return false, fmt.Errorf("required Harbor file %q is unavailable or unsafe", filepath.Base(source))
	}
	entries[archiveName] = source
	return true, nil
}

func makeBoundedArchive(ctx context.Context, entries map[string]string, limit int64) ([]byte, error) {
	if len(entries) == 0 || len(entries) > maxArchiveEntries {
		return nil, errors.New("export archive entry count is outside the supported limit")
	}
	var output boundedBuffer
	output.limit = limit
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	names := make([]string, 0, len(entries))
	for name := range entries {
		if filepath.IsAbs(name) || strings.Contains(name, "..") || strings.Contains(name, "\\") {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return nil, errors.New("export archive contains an unsafe member name")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return nil, err
		}
		path := entries[name]
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
			_ = tarWriter.Close()
			_ = gzipWriter.Close()
			return nil, fmt.Errorf("export member %q is not a supported regular file", name)
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		openedInfo, statErr := file.Stat()
		if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) || openedInfo.Size() != info.Size() {
			_ = file.Close()
			return nil, fmt.Errorf("export member %q changed while opening", name)
		}
		header := &tar.Header{Name: name, Mode: 0600, Size: info.Size(), ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
		if err := tarWriter.WriteHeader(header); err != nil {
			_ = file.Close()
			return nil, err
		}
		copied, copyErr := io.Copy(tarWriter, io.LimitReader(file, info.Size()))
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || copied != info.Size() {
			return nil, errors.Join(copyErr, closeErr, fmt.Errorf("export member %q changed while copying", name))
		}
		if output.exceeded {
			return nil, errors.New("export archive exceeds the 48 MiB size limit")
		}
	}
	if err := tarWriter.Close(); err != nil {
		return nil, err
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, err
	}
	if output.exceeded {
		return nil, errors.New("export archive exceeds the 48 MiB size limit")
	}
	return output.Bytes(), nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int64
	exceeded bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if int64(b.Len()+len(p)) > b.limit {
		b.exceeded = true
		remaining := int(b.limit) - b.Len()
		if remaining > 0 {
			_, _ = b.Buffer.Write(p[:remaining])
		}
		return 0, errors.New("bounded export archive limit exceeded")
	}
	return b.Buffer.Write(p)
}

func encryptHarborArchive(passphrase string, plain []byte) ([]byte, error) {
	if len(plain) > maxExportBytes {
		return nil, errors.New("export archive exceeds the supported size limit")
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, errors.New("could not secure Harbor export")
	}
	key, err := scrypt.Key([]byte(passphrase), salt, scryptN, scryptR, scryptP, 32)
	if err != nil {
		return nil, errors.New("could not derive Harbor export key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("could not initialize Harbor export encryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("could not initialize Harbor export encryption")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, errors.New("could not secure Harbor export")
	}
	ciphertext := aead.Seal(nil, nonce, plain, []byte("harbor-export-v1"))
	// Format: magic(16), scrypt parameters(12), salt(16), nonce(12), ciphertext.
	result := make([]byte, 0, 56+len(ciphertext))
	result = append(result, []byte("HARBOR-EXPORT-1\x00")...)
	params := make([]byte, 12)
	binary.BigEndian.PutUint32(params[0:4], scryptN)
	binary.BigEndian.PutUint32(params[4:8], scryptR)
	binary.BigEndian.PutUint32(params[8:12], scryptP)
	result = append(result, params...)
	result = append(result, salt...)
	result = append(result, nonce...)
	result = append(result, ciphertext...)
	return result, nil
}

// decryptHarborArchive is kept private and is also used by tests to exercise
// passphrase validation and authenticated corruption handling.
func decryptHarborArchive(passphrase string, encrypted []byte) ([]byte, error) {
	const headerLength = 16 + 12 + 16 + 12
	if len(passphrase) < minPassphraseBytes || len(passphrase) > maxPassphraseBytes || len(encrypted) < headerLength+16 || string(encrypted[:16]) != "HARBOR-EXPORT-1\x00" {
		return nil, errors.New("encrypted Harbor export is invalid")
	}
	params := encrypted[16:28]
	if binary.BigEndian.Uint32(params[:4]) != scryptN || binary.BigEndian.Uint32(params[4:8]) != scryptR || binary.BigEndian.Uint32(params[8:12]) != scryptP {
		return nil, errors.New("encrypted Harbor export parameters are unsupported")
	}
	salt := encrypted[28:44]
	nonce := encrypted[44:56]
	key, err := scrypt.Key([]byte(passphrase), salt, scryptN, scryptR, scryptP, 32)
	if err != nil {
		return nil, errors.New("could not derive Harbor export key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("could not initialize Harbor export decryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("could not initialize Harbor export decryption")
	}
	plain, err := aead.Open(nil, nonce, encrypted[headerLength:], []byte("harbor-export-v1"))
	if err != nil {
		return nil, errors.New("Harbor export passphrase is incorrect or archive is damaged")
	}
	return plain, nil
}
