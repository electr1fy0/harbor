package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"paas/internal/model"
)

// Data-service metadata is created lazily so existing control-plane schemas
// remain compatible without making a storage feature a daemon startup gate.
func (s *Store) ensureDataServicesSchema() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS data_backups (
		id TEXT PRIMARY KEY NOT NULL,
		service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
		app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
		size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
		created_at TEXT NOT NULL,
		scheduled INTEGER NOT NULL DEFAULT 0 CHECK (scheduled IN (0, 1)),
		verification TEXT NOT NULL DEFAULT 'stale',
		verified_at TEXT,
		verification_error TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		return fmt.Errorf("store: initialize data backup metadata: %w", err)
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS data_backups_binding_idx ON data_backups(service_id, app_id, created_at DESC)`)
	if err != nil {
		return fmt.Errorf("store: index data backup metadata: %w", err)
	}
	_, err = s.db.Exec(`CREATE TABLE IF NOT EXISTS data_backup_policies (
		service_id TEXT PRIMARY KEY NOT NULL REFERENCES services(id) ON DELETE CASCADE,
		frequency TEXT NOT NULL,
		retention INTEGER NOT NULL CHECK (retention >= 1 AND retention <= 100),
		enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
		updated_at TEXT NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("store: initialize data backup policies: %w", err)
	}
	return nil
}

// BindingsForService returns safe-to-use binding records with their owning app
// names. The password field remains empty because SQLite bindings have none.
func (s *Store) BindingsForService(serviceID string) ([]model.Binding, map[string]string, error) {
	rows, err := s.db.Query(`SELECT b.id, b.app_id, b.service_id, b.database_name, b.env_key, b.created_at, a.name
		FROM bindings b JOIN apps a ON a.id=b.app_id WHERE b.service_id=? ORDER BY a.name`, serviceID)
	if err != nil {
		return nil, nil, fmt.Errorf("store: list service bindings: %w", err)
	}
	defer rows.Close()
	var bindings []model.Binding
	appNames := make(map[string]string)
	for rows.Next() {
		var b model.Binding
		var createdAt string
		var appName string
		if err := rows.Scan(&b.ID, &b.AppID, &b.ServiceID, &b.DatabaseName, &b.EnvKey, &createdAt, &appName); err != nil {
			return nil, nil, fmt.Errorf("store: scan service binding: %w", err)
		}
		b.CreatedAt, err = decodeTime(createdAt)
		if err != nil {
			return nil, nil, fmt.Errorf("store: decode service binding time: %w", err)
		}
		bindings = append(bindings, b)
		appNames[b.AppID] = appName
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: list service bindings: %w", err)
	}
	return bindings, appNames, nil
}

// DeleteBinding removes only the control-plane link. Durable database files
// and backups are deliberately preserved.
func (s *Store) DeleteBinding(appID, serviceID string) error {
	result, err := s.db.Exec(`DELETE FROM bindings WHERE app_id=? AND service_id=?`, appID, serviceID)
	if err != nil {
		return fmt.Errorf("store: unlink application from service: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: confirm service unlink: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: service binding", ErrNotFound)
	}
	return nil
}

// CreateRestoredApp atomically reserves a new app identity and its first
// binding. It never returns an existing application when the requested name
// is already taken, which lets callers stage restored files before making the
// app visible to deployment workers.
func (s *Store) CreateRestoredApp(app model.Application, binding model.Binding) (bool, error) {
	if app.ID == "" || app.Name == "" || binding.AppID != app.ID || binding.ServiceID == "" {
		return false, errors.New("store: restored application identity and binding are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: begin restored application creation: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO apps (id, name, created_at) VALUES (?, ?, ?) ON CONFLICT(name) DO NOTHING`,
		app.ID, app.Name, encodeTime(app.CreatedAt))
	if err != nil {
		return false, fmt.Errorf("store: reserve restored application name: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: check restored application name: %w", err)
	}
	if inserted == 0 {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO app_settings (app_id) VALUES (?)`, app.ID); err != nil {
		return false, fmt.Errorf("store: initialize restored application settings: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO bindings (id, app_id, service_id, database_name, username, password, env_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, binding.ID, binding.AppID, binding.ServiceID, binding.DatabaseName,
		binding.Username, binding.Password, binding.EnvKey, encodeTime(binding.CreatedAt)); err != nil {
		return false, fmt.Errorf("store: create restored application binding: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit restored application creation: %w", err)
	}
	return true, nil
}

// CreateDataBackup records a completed, durable snapshot.
func (s *Store) CreateDataBackup(backup model.DataBackup, appID string) error {
	if err := s.ensureDataServicesSchema(); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO data_backups (id, service_id, app_id, size_bytes, created_at, scheduled, verification, verified_at, verification_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, backup.ID, backup.ServiceID, appID, backup.SizeBytes,
		encodeTime(backup.CreatedAt), backup.Scheduled, backup.Verification, nullableTime(backup.VerifiedAt), backup.Error)
	if err != nil {
		return fmt.Errorf("store: record data backup: %w", err)
	}
	return nil
}

// DataBackup returns backup metadata by its opaque id.
func (s *Store) DataBackup(id string) (*model.DataBackup, string, error) {
	if err := s.ensureDataServicesSchema(); err != nil {
		return nil, "", err
	}
	var backup model.DataBackup
	var appID, createdAt string
	var scheduled int
	var verifiedAt sql.NullString
	err := s.db.QueryRow(`SELECT b.id,b.service_id,s.name,a.name,b.size_bytes,b.created_at,b.scheduled,b.verification,b.verified_at,b.verification_error,b.app_id
		FROM data_backups b JOIN services s ON s.id=b.service_id JOIN apps a ON a.id=b.app_id WHERE b.id=?`, id).
		Scan(&backup.ID, &backup.ServiceID, &backup.ServiceName, &backup.AppName, &backup.SizeBytes, &createdAt,
			&scheduled, &backup.Verification, &verifiedAt, &backup.Error, &appID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", fmt.Errorf("%w: data backup", ErrNotFound)
	}
	if err != nil {
		return nil, "", fmt.Errorf("store: read data backup: %w", err)
	}
	backup.Scheduled = scheduled != 0
	backup.CreatedAt, err = decodeTime(createdAt)
	if err != nil {
		return nil, "", fmt.Errorf("store: decode data backup time: %w", err)
	}
	if verifiedAt.Valid {
		value, err := decodeTime(verifiedAt.String)
		if err != nil {
			return nil, "", fmt.Errorf("store: decode verification time: %w", err)
		}
		backup.VerifiedAt = &value
	}
	return &backup, appID, nil
}

// DataBackups lists recent backup records for a service.
func (s *Store) DataBackups(serviceID string) ([]model.DataBackup, error) {
	if err := s.ensureDataServicesSchema(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT b.id,b.service_id,s.name,a.name,b.size_bytes,b.created_at,b.scheduled,b.verification,b.verified_at,b.verification_error
		FROM data_backups b JOIN services s ON s.id=b.service_id JOIN apps a ON a.id=b.app_id WHERE b.service_id=? ORDER BY b.created_at DESC,b.id LIMIT 100`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("store: list data backups: %w", err)
	}
	defer rows.Close()
	backups := make([]model.DataBackup, 0)
	for rows.Next() {
		var backup model.DataBackup
		var createdAt string
		var scheduled int
		var verifiedAt sql.NullString
		if err := rows.Scan(&backup.ID, &backup.ServiceID, &backup.ServiceName, &backup.AppName, &backup.SizeBytes,
			&createdAt, &scheduled, &backup.Verification, &verifiedAt, &backup.Error); err != nil {
			return nil, fmt.Errorf("store: scan data backup: %w", err)
		}
		backup.CreatedAt, err = decodeTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("store: decode data backup time: %w", err)
		}
		backup.Scheduled = scheduled != 0
		if verifiedAt.Valid {
			value, err := decodeTime(verifiedAt.String)
			if err != nil {
				return nil, fmt.Errorf("store: decode verification time: %w", err)
			}
			backup.VerifiedAt = &value
		}
		backups = append(backups, backup)
	}
	return backups, rows.Err()
}

// UpdateDataBackupVerification records the latest verification result.
func (s *Store) UpdateDataBackupVerification(id, state, message string, verifiedAt *time.Time) error {
	if err := s.ensureDataServicesSchema(); err != nil {
		return err
	}
	result, err := s.db.Exec(`UPDATE data_backups SET verification=?,verification_error=?,verified_at=? WHERE id=?`, state, message, nullableTime(verifiedAt), id)
	if err != nil {
		return fmt.Errorf("store: update backup verification: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: confirm backup verification update: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: data backup", ErrNotFound)
	}
	return nil
}

// DeleteDataBackup removes only backup metadata. Callers remove the validated
// snapshot file first.
func (s *Store) DeleteDataBackup(id string) error {
	if err := s.ensureDataServicesSchema(); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM data_backups WHERE id=?`, id)
	return err
}

// SetDataBackupPolicy persists a service policy.
func (s *Store) SetDataBackupPolicy(policy model.BackupPolicy) error {
	if err := s.ensureDataServicesSchema(); err != nil {
		return err
	}
	_, err := s.db.Exec(`INSERT INTO data_backup_policies (service_id,frequency,retention,enabled,updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT(service_id) DO UPDATE SET frequency=excluded.frequency,retention=excluded.retention,enabled=excluded.enabled,updated_at=excluded.updated_at`,
		policy.ServiceID, policy.Frequency, policy.Retention, policy.Enabled, encodeTime(policy.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: save data backup policy: %w", err)
	}
	return nil
}

// DataBackupPolicy reads a service policy, returning ErrNotFound when none is set.
func (s *Store) DataBackupPolicy(serviceID string) (*model.BackupPolicy, error) {
	if err := s.ensureDataServicesSchema(); err != nil {
		return nil, err
	}
	var policy model.BackupPolicy
	var enabled int
	var updatedAt string
	err := s.db.QueryRow(`SELECT service_id,frequency,retention,enabled,updated_at FROM data_backup_policies WHERE service_id=?`, serviceID).
		Scan(&policy.ServiceID, &policy.Frequency, &policy.Retention, &enabled, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: data backup policy", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read data backup policy: %w", err)
	}
	policy.Enabled = enabled != 0
	policy.UpdatedAt, err = decodeTime(updatedAt)
	if err != nil {
		return nil, fmt.Errorf("store: decode data backup policy time: %w", err)
	}
	return &policy, nil
}

// DataBackupPolicies returns enabled and disabled policies.
func (s *Store) DataBackupPolicies() ([]model.BackupPolicy, error) {
	if err := s.ensureDataServicesSchema(); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT service_id,frequency,retention,enabled,updated_at FROM data_backup_policies ORDER BY service_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list data backup policies: %w", err)
	}
	defer rows.Close()
	policies := make([]model.BackupPolicy, 0)
	for rows.Next() {
		var p model.BackupPolicy
		var enabled int
		var updatedAt string
		if err := rows.Scan(&p.ServiceID, &p.Frequency, &p.Retention, &enabled, &updatedAt); err != nil {
			return nil, fmt.Errorf("store: scan data backup policy: %w", err)
		}
		p.Enabled = enabled != 0
		p.UpdatedAt, err = decodeTime(updatedAt)
		if err != nil {
			return nil, fmt.Errorf("store: decode data backup policy time: %w", err)
		}
		policies = append(policies, p)
	}
	return policies, rows.Err()
}

// HasEncryptedSettings reports whether the control database contains encrypted
// application configuration that requires the environment.key file.
func (s *Store) HasEncryptedSettings() (bool, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM app_settings WHERE length(environment) > 0 OR length(redaction_history) > 0`).Scan(&count); err != nil {
		return false, fmt.Errorf("store: inspect encrypted application settings: %w", err)
	}
	return count > 0, nil
}

// ControlDatabaseBytes reports the size of the main control database and its
// WAL without following symlinks. It is used to bound portable exports.
func (s *Store) ControlDatabaseBytes() (int64, error) {
	var path string
	if err := s.db.QueryRow(`PRAGMA database_list`).Scan(new(int), new(string), &path); err != nil {
		return 0, fmt.Errorf("store: inspect control database path: %w", err)
	}
	if path == "" {
		return 0, errors.New("store: in-memory control database cannot be exported")
	}
	var total int64
	for _, item := range []string{path, path + "-wal"} {
		info, err := os.Lstat(item)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return 0, errors.New("store: control database has an unsafe file")
		}
		total += info.Size()
	}
	return total, nil
}

// SnapshotControlDB creates a consistent standalone snapshot using SQLite's
// VACUUM INTO operation. It does not copy a live database file or its WAL.
func (s *Store) SnapshotControlDB(ctx context.Context, destination string) error {
	if destination == "" || !filepath.IsAbs(destination) {
		return errors.New("store: control snapshot destination must be absolute")
	}
	info, err := os.Lstat(filepath.Dir(destination))
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("store: control snapshot destination directory is unavailable")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("store: control snapshot destination already exists")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, destination); err != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("store: create consistent control snapshot: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(destination)
		return err
	}
	info, err = os.Lstat(destination)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 {
		_ = os.Remove(destination)
		return errors.New("store: consistent control snapshot is not a regular database file")
	}
	if err := os.Chmod(destination, 0600); err != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("store: secure control snapshot: %w", err)
	}
	return nil
}
