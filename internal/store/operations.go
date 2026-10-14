package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"paas/internal/model"
)

// InitOperations creates additive runtime metadata tables and triggers. These
// tables are intentionally outside the numbered core schema migration so this
// feature can be rolled out without rewriting existing database history.
func (s *Store) InitOperations() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS app_operations (
			app_id TEXT PRIMARY KEY NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
			favorite INTEGER NOT NULL DEFAULT 0 CHECK (favorite IN (0, 1)),
			tags TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(tags)),
			archived INTEGER NOT NULL DEFAULT 0 CHECK (archived IN (0, 1)),
			required_environment_keys TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(required_environment_keys)),
			settings_revision INTEGER NOT NULL DEFAULT 1,
			applied_settings_revision INTEGER NOT NULL DEFAULT 1,
			health_interval_seconds INTEGER NOT NULL DEFAULT 30,
			health_timeout_seconds INTEGER NOT NULL DEFAULT 2,
			health_failure_threshold INTEGER NOT NULL DEFAULT 3,
			health_status TEXT NOT NULL DEFAULT 'unknown',
			health_failure_count INTEGER NOT NULL DEFAULT 0,
			last_health_check_at TEXT,
			restart_attempt INTEGER NOT NULL DEFAULT 0,
			restart_window_started TEXT,
			next_restart_at TEXT,
			crash_loop INTEGER NOT NULL DEFAULT 0 CHECK (crash_loop IN (0, 1)),
			automatic_restart_enabled INTEGER NOT NULL DEFAULT 0 CHECK (automatic_restart_enabled IN (0, 1)),
			automatic_restart_max_attempts INTEGER NOT NULL DEFAULT 5,
			automatic_restart_base_delay_seconds INTEGER NOT NULL DEFAULT 1,
			retain_release_count INTEGER NOT NULL DEFAULT 5
		)`,
		`CREATE TABLE IF NOT EXISTS pruned_release_artifacts (
			release_id TEXT PRIMARY KEY NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
			pruned_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("store: initialize operation metadata: %w", err)
		}
	}
	// Remove legacy custom-domain revision triggers from older databases.
	for _, trigger := range []string{"app_operations_domain_insert", "app_operations_domain_delete"} {
		if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS ` + trigger); err != nil {
			return fmt.Errorf("store: remove obsolete operation metadata trigger: %w", err)
		}
	}
	if err := s.ensureOperationColumns(); err != nil {
		return err
	}
	triggers := []string{
		`CREATE TRIGGER IF NOT EXISTS app_operations_create AFTER INSERT ON apps BEGIN
			INSERT INTO app_operations (app_id) VALUES (NEW.id) ON CONFLICT(app_id) DO NOTHING;
		END`,
		`CREATE INDEX IF NOT EXISTS events_timing_idx ON events(deployment_id,type,created_at,id)`,
		`DROP TRIGGER IF EXISTS app_operations_settings_revision`,
		`CREATE TRIGGER app_operations_settings_revision AFTER UPDATE OF memory_limit, cpu_quota, tasks_limit, environment ON app_settings
			WHEN OLD.memory_limit IS NOT NEW.memory_limit OR OLD.cpu_quota IS NOT NEW.cpu_quota OR OLD.tasks_limit IS NOT NEW.tasks_limit OR OLD.environment IS NOT NEW.environment
			BEGIN UPDATE app_operations SET settings_revision = settings_revision + 1 WHERE app_id = NEW.app_id; END`,
		`CREATE TRIGGER IF NOT EXISTS app_operations_binding_insert AFTER INSERT ON bindings BEGIN
			UPDATE app_operations SET settings_revision = settings_revision + 1 WHERE app_id = NEW.app_id;
		END`,
		`CREATE TRIGGER IF NOT EXISTS app_operations_binding_update AFTER UPDATE OF app_id, service_id, database_name, username, password, env_key ON bindings
			WHEN OLD.app_id IS NOT NEW.app_id OR OLD.service_id IS NOT NEW.service_id OR OLD.database_name IS NOT NEW.database_name OR OLD.username IS NOT NEW.username OR OLD.password IS NOT NEW.password OR OLD.env_key IS NOT NEW.env_key
			BEGIN
			UPDATE app_operations SET settings_revision = settings_revision + 1 WHERE app_id = OLD.app_id;
			UPDATE app_operations SET settings_revision = settings_revision + 1 WHERE app_id = NEW.app_id AND NEW.app_id IS NOT OLD.app_id;
		END`,
		`CREATE TRIGGER IF NOT EXISTS app_operations_binding_delete AFTER DELETE ON bindings BEGIN
			UPDATE app_operations SET settings_revision = settings_revision + 1 WHERE app_id = OLD.app_id;
		END`,
	}
	for _, statement := range triggers {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("store: initialize operation metadata trigger: %w", err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO app_operations (app_id) SELECT id FROM apps WHERE true ON CONFLICT(app_id) DO NOTHING`); err != nil {
		return fmt.Errorf("store: seed operation metadata: %w", err)
	}
	return nil
}

// DeploymentSettingsSnapshot reads runtime-affecting app settings, the exact
// revision covering those settings, and bindings from one SQLite snapshot.
// Callers can safely mark only this revision applied after promotion.
type DeploymentSettingsSnapshot struct {
	AppSettings             *model.AppSettings
	SettingsRevision        int64
	RequiredEnvironmentKeys []string
	Bindings                []model.Binding
}

func (s *Store) DeploymentSettingsSnapshot(name string) (*DeploymentSettingsSnapshot, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin deployment settings snapshot: %w", err)
	}
	defer tx.Rollback()

	snapshot := &DeploymentSettingsSnapshot{AppSettings: &model.AppSettings{}, Bindings: []model.Binding{}}
	var desired int
	var requiredJSON string
	if err := tx.QueryRow(`SELECT a.id, a.name, st.desired_running, st.memory_limit, st.cpu_quota, st.tasks_limit,
		st.environment, st.redaction_history, o.settings_revision, o.required_environment_keys
		FROM apps a JOIN app_settings st ON st.app_id = a.id JOIN app_operations o ON o.app_id = a.id WHERE a.name = ?`, name).
		Scan(&snapshot.AppSettings.AppID, &snapshot.AppSettings.AppName, &desired, &snapshot.AppSettings.MemoryLimit,
			&snapshot.AppSettings.CPUQuota, &snapshot.AppSettings.TasksLimit, &snapshot.AppSettings.EncryptedEnvironment,
			&snapshot.AppSettings.EncryptedRedactionHistory, &snapshot.SettingsRevision, &requiredJSON); errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: application %q", ErrNotFound, name)
	} else if err != nil {
		return nil, fmt.Errorf("store: read deployment settings snapshot: %w", err)
	}
	snapshot.AppSettings.DesiredRunning = desired != 0
	if err := json.Unmarshal([]byte(requiredJSON), &snapshot.RequiredEnvironmentKeys); err != nil {
		return nil, errors.New("store: stored required environment keys are invalid")
	}
	if snapshot.RequiredEnvironmentKeys == nil {
		snapshot.RequiredEnvironmentKeys = []string{}
	}
	rows, err := tx.Query(`SELECT id, app_id, service_id, database_name, username, password, env_key, created_at
		FROM bindings WHERE app_id = ? ORDER BY created_at ASC`, snapshot.AppSettings.AppID)
	if err != nil {
		return nil, fmt.Errorf("store: read deployment binding snapshot: %w", err)
	}
	for rows.Next() {
		var binding model.Binding
		var createdAt string
		if err := rows.Scan(&binding.ID, &binding.AppID, &binding.ServiceID, &binding.DatabaseName, &binding.Username, &binding.Password, &binding.EnvKey, &createdAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan deployment binding snapshot: %w", err)
		}
		if binding.CreatedAt, err = decodeTime(createdAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: decode deployment binding snapshot: %w", err)
		}
		snapshot.Bindings = append(snapshot.Bindings, binding)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: read deployment binding snapshot: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: close deployment binding snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit deployment settings snapshot: %w", err)
	}
	return snapshot, nil
}

func (s *Store) ensureOperationColumns() error {
	rows, err := s.db.Query(`PRAGMA table_info(app_operations)`)
	if err != nil {
		return fmt.Errorf("store: inspect operation metadata schema: %w", err)
	}
	present := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan operation metadata schema: %w", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: read operation metadata schema: %w", err)
	}
	rows.Close()
	columns := []struct{ name, declaration string }{
		{"settings_revision", "INTEGER NOT NULL DEFAULT 1"},
		{"applied_settings_revision", "INTEGER NOT NULL DEFAULT 1"},
		{"health_interval_seconds", "INTEGER NOT NULL DEFAULT 30"},
		{"health_timeout_seconds", "INTEGER NOT NULL DEFAULT 2"},
		{"health_failure_threshold", "INTEGER NOT NULL DEFAULT 3"},
		{"health_status", "TEXT NOT NULL DEFAULT 'unknown'"},
		{"health_failure_count", "INTEGER NOT NULL DEFAULT 0"},
		{"last_health_check_at", "TEXT"},
		{"restart_attempt", "INTEGER NOT NULL DEFAULT 0"},
		{"restart_window_started", "TEXT"},
		{"next_restart_at", "TEXT"},
		{"crash_loop", "INTEGER NOT NULL DEFAULT 0"},
		{"automatic_restart_enabled", "INTEGER NOT NULL DEFAULT 0"},
		{"automatic_restart_max_attempts", "INTEGER NOT NULL DEFAULT 5"},
		{"automatic_restart_base_delay_seconds", "INTEGER NOT NULL DEFAULT 1"},
		{"retain_release_count", "INTEGER NOT NULL DEFAULT 5"},
	}
	for _, column := range columns {
		if present[column.name] {
			continue
		}
		if _, err := s.db.Exec(`ALTER TABLE app_operations ADD COLUMN ` + column.name + ` ` + column.declaration); err != nil {
			return fmt.Errorf("store: add operation metadata column %s: %w", column.name, err)
		}
	}
	return nil
}

func (s *Store) OperationSettingsByAppID(appID string) (*model.OperationSettings, error) {
	var name string
	if err := s.db.QueryRow(`SELECT name FROM apps WHERE id = ?`, appID).Scan(&name); errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: application %q", ErrNotFound, appID)
	} else if err != nil {
		return nil, fmt.Errorf("store: read app for operation settings: %w", err)
	}
	return s.OperationSettings(name)
}

// OperationSettings returns safe app metadata and its persisted supervision
// policy. Environment values and secrets are never selected by this query.
func (s *Store) OperationSettings(name string) (*model.OperationSettings, error) {
	var result model.OperationSettings
	var favorite, archived, crashLoop, restartEnabled int
	var tagsJSON, requiredJSON, healthStatus string
	var checkedAt, restartAt sql.NullString
	err := s.db.QueryRow(`SELECT o.favorite, o.tags, o.archived, o.required_environment_keys,
		o.settings_revision, o.applied_settings_revision, o.health_interval_seconds,
		o.health_timeout_seconds, o.health_failure_threshold, o.health_status,
		o.health_failure_count, o.last_health_check_at, o.restart_attempt,
		o.next_restart_at, o.crash_loop, o.automatic_restart_enabled,
		o.automatic_restart_max_attempts, o.automatic_restart_base_delay_seconds,
		o.retain_release_count
		FROM app_operations o JOIN apps a ON a.id = o.app_id WHERE a.name = ?`, name).Scan(
		&favorite, &tagsJSON, &archived, &requiredJSON,
		&result.SettingsRevision, &result.AppliedSettingsRevision, &result.HealthProbeIntervalSeconds,
		&result.HealthProbeTimeoutSeconds, &result.HealthFailureThreshold, &healthStatus,
		&result.HealthFailureCount, &checkedAt, &result.RestartAttempt,
		&restartAt, &crashLoop, &restartEnabled, &result.AutomaticRestartMaxAttempts,
		&result.AutomaticRestartBaseDelaySeconds, &result.RetainReleaseCount,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: operation settings for application %q", ErrNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read operation settings: %w", err)
	}
	if err := json.Unmarshal([]byte(tagsJSON), &result.Tags); err != nil {
		return nil, errors.New("store: stored application tags are invalid")
	}
	if err := json.Unmarshal([]byte(requiredJSON), &result.RequiredEnvironmentKeys); err != nil {
		return nil, errors.New("store: stored required environment keys are invalid")
	}
	if result.Tags == nil {
		result.Tags = []string{}
	}
	if result.RequiredEnvironmentKeys == nil {
		result.RequiredEnvironmentKeys = []string{}
	}
	result.Favorite, result.Archived, result.CrashLoop = favorite != 0, archived != 0, crashLoop != 0
	result.AutomaticRestartEnabled = restartEnabled != 0
	result.PendingSettings = result.SettingsRevision != result.AppliedSettingsRevision
	result.HealthProbeEnabled = result.HealthProbeIntervalSeconds > 0
	result.HealthStatus = healthStatus
	if checkedAt.Valid {
		value, err := decodeTime(checkedAt.String)
		if err != nil {
			return nil, fmt.Errorf("store: decode health check time: %w", err)
		}
		result.LastHealthCheckAt = &value
	}
	if restartAt.Valid {
		value, err := decodeTime(restartAt.String)
		if err != nil {
			return nil, fmt.Errorf("store: decode automatic restart time: %w", err)
		}
		result.NextRestartAt = &value
	}
	return &result, nil
}

// UpdateOperationMetadata replaces user-facing app metadata in one statement.
func (s *Store) UpdateOperationMetadata(name string, favorite, archived bool, tags, required []string) error {
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return errors.New("store: encode application tags")
	}
	requiredJSON, err := json.Marshal(required)
	if err != nil {
		return errors.New("store: encode required environment keys")
	}
	result, err := s.db.Exec(`UPDATE app_operations SET favorite = ?, archived = ?, tags = ?, required_environment_keys = ?
		WHERE app_id = (SELECT id FROM apps WHERE name = ?)`, boolInt(favorite), boolInt(archived), string(tagsJSON), string(requiredJSON), name)
	if err != nil {
		return fmt.Errorf("store: update application operation metadata: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: check application operation metadata: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: application %q", ErrNotFound, name)
	}
	return nil
}

func (s *Store) SetHealthPolicy(name string, intervalSeconds, timeoutSeconds, failureThreshold int) error {
	result, err := s.db.Exec(`UPDATE app_operations SET health_interval_seconds = ?, health_timeout_seconds = ?, health_failure_threshold = ?,
		health_status = CASE WHEN ? = 0 THEN 'disabled' ELSE 'unknown' END, health_failure_count = 0, last_health_check_at = NULL
		WHERE app_id = (SELECT id FROM apps WHERE name = ?)`, intervalSeconds, timeoutSeconds, failureThreshold, intervalSeconds, name)
	if err != nil {
		return fmt.Errorf("store: update health policy: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: check health policy: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: application %q", ErrNotFound, name)
	}
	return nil
}

func (s *Store) SetRestartPolicy(name string, enabled bool, maxAttempts, baseDelaySeconds int) error {
	result, err := s.db.Exec(`UPDATE app_operations SET automatic_restart_enabled = ?, automatic_restart_max_attempts = ?, automatic_restart_base_delay_seconds = ?, next_restart_at = NULL, crash_loop = 0, restart_attempt = 0, restart_window_started = NULL
		WHERE app_id = (SELECT id FROM apps WHERE name = ?)`, boolInt(enabled), maxAttempts, baseDelaySeconds, name)
	if err != nil {
		return fmt.Errorf("store: update automatic restart policy: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: check automatic restart policy: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: application %q", ErrNotFound, name)
	}
	return nil
}

func (s *Store) SetReleaseRetention(name string, retain int) error {
	result, err := s.db.Exec(`UPDATE app_operations SET retain_release_count = ? WHERE app_id = (SELECT id FROM apps WHERE name = ?)`, retain, name)
	if err != nil {
		return fmt.Errorf("store: update release retention: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: check release retention: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: application %q", ErrNotFound, name)
	}
	return nil
}

func (s *Store) MarkSettingsApplied(appID string) error {
	result, err := s.db.Exec(`UPDATE app_operations SET applied_settings_revision = settings_revision WHERE app_id = ?`, appID)
	if err != nil {
		return fmt.Errorf("store: mark application settings applied: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: check applied settings revision: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: operation settings %q", ErrNotFound, appID)
	}
	return nil
}

// MarkSettingsAppliedRevision records only the configuration snapshot consumed
// by a deployment. A later edit made while it built remains pending.
func (s *Store) MarkSettingsAppliedRevision(appID string, revision int64) error {
	result, err := s.db.Exec(`UPDATE app_operations SET applied_settings_revision = CASE
		WHEN applied_settings_revision > ? THEN applied_settings_revision ELSE ? END WHERE app_id = ?`, revision, revision, appID)
	if err != nil {
		return fmt.Errorf("store: mark application settings revision applied: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: check applied settings revision: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: operation settings %q", ErrNotFound, appID)
	}
	return nil
}

func (s *Store) UpdateHealthStatus(appID, status string, failures int, at time.Time) error {
	_, err := s.db.Exec(`UPDATE app_operations SET health_status = ?, health_failure_count = ?, last_health_check_at = ? WHERE app_id = ?`, status, failures, encodeTime(at.UTC()), appID)
	if err != nil {
		return fmt.Errorf("store: update ongoing health status: %w", err)
	}
	return nil
}

// RecordAutomaticRestart persists a bounded exponential backoff decision.
// The configured maximum applies inside a ten-minute crash window.
func (s *Store) RecordAutomaticRestart(appID string, now time.Time) (model.RestartPlan, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return model.RestartPlan{}, fmt.Errorf("store: begin automatic restart update: %w", err)
	}
	defer tx.Rollback()
	var desired, attempt, crashLoop, enabled, maxAttempts, baseDelaySeconds int
	var window sql.NullString
	if err := tx.QueryRow(`SELECT st.desired_running, o.restart_attempt, o.crash_loop, o.restart_window_started,
		o.automatic_restart_enabled, o.automatic_restart_max_attempts, o.automatic_restart_base_delay_seconds
		FROM app_settings st JOIN app_operations o ON o.app_id = st.app_id WHERE st.app_id = ?`, appID).
		Scan(&desired, &attempt, &crashLoop, &window, &enabled, &maxAttempts, &baseDelaySeconds); errors.Is(err, sql.ErrNoRows) {
		return model.RestartPlan{}, fmt.Errorf("%w: app operation state %q", ErrNotFound, appID)
	} else if err != nil {
		return model.RestartPlan{}, fmt.Errorf("store: read automatic restart state: %w", err)
	}
	plan := model.RestartPlan{Attempt: attempt, CrashLoop: crashLoop != 0, ShouldRun: desired != 0 && enabled != 0}
	if desired == 0 || enabled == 0 || crashLoop != 0 {
		return plan, tx.Commit()
	}
	now = now.UTC()
	if window.Valid {
		started, err := decodeTime(window.String)
		if err != nil {
			return model.RestartPlan{}, fmt.Errorf("store: decode restart window: %w", err)
		}
		if now.Sub(started) >= 10*time.Minute || now.Before(started) {
			attempt = 0
			window = sql.NullString{}
		}
	}
	if attempt >= maxAttempts {
		if _, err := tx.Exec(`UPDATE app_operations SET crash_loop = 1, next_restart_at = NULL WHERE app_id = ?`, appID); err != nil {
			return model.RestartPlan{}, fmt.Errorf("store: mark crash loop: %w", err)
		}
		plan.CrashLoop, plan.ShouldRun = true, false
		return plan, tx.Commit()
	}
	if !window.Valid {
		window = sql.NullString{String: encodeTime(now), Valid: true}
	}
	attempt++
	delay := time.Second * time.Duration(float64(baseDelaySeconds)*math.Pow(2, float64(attempt-1)))
	if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	restartAt := now.Add(delay)
	if _, err := tx.Exec(`UPDATE app_operations SET restart_attempt = ?, restart_window_started = ?, next_restart_at = ?, health_status = 'unhealthy' WHERE app_id = ?`, attempt, window.String, encodeTime(restartAt), appID); err != nil {
		return model.RestartPlan{}, fmt.Errorf("store: schedule automatic restart: %w", err)
	}
	plan.Attempt, plan.RestartAt = attempt, &restartAt
	return plan, tx.Commit()
}

func (s *Store) ClearAutomaticRestart(appID string) error {
	_, err := s.db.Exec(`UPDATE app_operations SET restart_attempt = 0, restart_window_started = NULL, next_restart_at = NULL, crash_loop = 0 WHERE app_id = ?`, appID)
	if err != nil {
		return fmt.Errorf("store: clear automatic restart state: %w", err)
	}
	return nil
}

func (s *Store) SetNextRestart(appID string, when *time.Time) error {
	var encoded any
	if when != nil {
		encoded = encodeTime(when.UTC())
	}
	_, err := s.db.Exec(`UPDATE app_operations SET next_restart_at = ? WHERE app_id = ?`, encoded, appID)
	if err != nil {
		return fmt.Errorf("store: update automatic restart time: %w", err)
	}
	return nil
}

func (s *Store) DeploymentHistory(name string, limit, offset int) ([]model.Deployment, int, error) {
	var appExists int
	if err := s.db.QueryRow(`SELECT 1 FROM apps WHERE name = ?`, name).Scan(&appExists); errors.Is(err, sql.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: application %q", ErrNotFound, name)
	} else if err != nil {
		return nil, 0, fmt.Errorf("store: check application for deployment history: %w", err)
	}
	var total int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM deployments d JOIN apps a ON a.id = d.app_id WHERE a.name = ?`, name).Scan(&total)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: application %q", ErrNotFound, name)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("store: count deployment history: %w", err)
	}
	rows, err := s.queryDeployments(deploymentSelect+`WHERE a.name = ? ORDER BY d.created_at DESC, d.id DESC LIMIT ? OFFSET ?`, name, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

func (s *Store) DeploymentHistoryAll(limit, offset int) ([]model.Deployment, int, error) {
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count deployment history: %w", err)
	}
	rows, err := s.queryDeployments(deploymentSelect+`ORDER BY d.created_at DESC, d.id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

func (s *Store) EventsForApp(name, deploymentID string) ([]model.Event, error) {
	rows, _, err := s.EventsForAppPage(name, deploymentID, 1_000_000, 0)
	return rows, err
}

func (s *Store) EventsForAppPage(name, deploymentID string, limit, offset int) ([]model.Event, int, error) {
	if limit < 1 || limit > 1_000_000 || offset < 0 || offset > 10_000_000 {
		return nil, 0, errors.New("store: event pagination is out of range")
	}
	var appID string
	if err := s.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, name).Scan(&appID); errors.Is(err, sql.ErrNoRows) {
		return nil, 0, fmt.Errorf("%w: application %q", ErrNotFound, name)
	} else if err != nil {
		return nil, 0, fmt.Errorf("store: read app for events: %w", err)
	}
	countQuery := `SELECT COUNT(*) FROM events WHERE app_id = ?`
	query := `SELECT id, deployment_id, app_id, type, payload, created_at FROM events WHERE app_id = ?`
	args := []any{appID}
	countArgs := []any{appID}
	if deploymentID != "" {
		query += ` AND deployment_id = ?`
		args = append(args, deploymentID)
		countQuery += ` AND deployment_id = ?`
		countArgs = append(countArgs, deploymentID)
	}
	var total int
	if err := s.db.QueryRow(countQuery, countArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count app events: %w", err)
	}
	query += ` ORDER BY id ASC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list app events: %w", err)
	}
	defer rows.Close()
	result := make([]model.Event, 0)
	for rows.Next() {
		var event model.Event
		var kind, createdAt string
		if err := rows.Scan(&event.ID, &event.DeploymentID, &event.AppID, &kind, &event.Payload, &createdAt); err != nil {
			return nil, 0, fmt.Errorf("store: scan app event: %w", err)
		}
		event.Type = model.EventType(kind)
		event.CreatedAt, err = decodeTime(createdAt)
		if err != nil {
			return nil, 0, fmt.Errorf("store: decode app event time: %w", err)
		}
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: list app events: %w", err)
	}
	return result, total, nil
}

func (s *Store) IsReleaseArtifactPruned(releaseID string) (bool, error) {
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM pruned_release_artifacts WHERE release_id = ?`, releaseID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: inspect release retention: %w", err)
	}
	return true, nil
}

// PrunedReleaseIDs returns release metadata whose artifacts were removed by
// the retention policy, preserving historical records without implying that
// rollback is still possible.
func (s *Store) PrunedReleaseIDs(appName string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT p.release_id FROM pruned_release_artifacts p
		JOIN releases r ON r.id = p.release_id JOIN apps a ON a.id = r.app_id WHERE a.name = ?`, appName)
	if err != nil {
		return nil, fmt.Errorf("store: list pruned release artifacts: %w", err)
	}
	defer rows.Close()
	result := make(map[string]bool)
	for rows.Next() {
		var releaseID string
		if err := rows.Scan(&releaseID); err != nil {
			return nil, fmt.Errorf("store: scan pruned release artifact: %w", err)
		}
		result[releaseID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read pruned release artifacts: %w", err)
	}
	return result, nil
}

// ReleasePruneCandidates excludes newest releases, active/previous promoted
// releases, and anything referenced by an unfinished deployment.
func (s *Store) ReleasePruneCandidates(name string, retain int) ([]model.ReleasePruneCandidate, error) {
	var appID string
	if err := s.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, name).Scan(&appID); errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: application %q", ErrNotFound, name)
	} else if err != nil {
		return nil, fmt.Errorf("store: read app for release retention: %w", err)
	}
	rows, err := s.db.Query(`SELECT r.id, r.artifact_dir FROM releases r WHERE r.app_id = ? ORDER BY r.created_at DESC, r.id DESC`, appID)
	if err != nil {
		return nil, fmt.Errorf("store: list releases for retention: %w", err)
	}
	type artifact struct{ id, dir string }
	all := make([]artifact, 0)
	for rows.Next() {
		var item artifact
		if err := rows.Scan(&item.id, &item.dir); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan release retention candidate: %w", err)
		}
		all = append(all, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: list release retention candidates: %w", err)
	}
	rows.Close()
	keep := make(map[string]bool)
	for i := 0; i < retain && i < len(all); i++ {
		keep[all[i].id] = true
	}
	var current, previous sql.NullString
	if err := s.db.QueryRow(`SELECT d.release_id FROM routes r JOIN deployments d ON d.id = r.deployment_id WHERE r.hostname = ? AND d.app_id = ?`, name+".localhost", appID).Scan(&current); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: read active release for retention: %w", err)
	}
	if current.Valid {
		keep[current.String] = true
	}
	if current.Valid {
		if err := s.db.QueryRow(`SELECT release_id FROM deployments WHERE app_id = ? AND promoted_at IS NOT NULL AND release_id != ? ORDER BY promoted_at DESC, created_at DESC, id DESC LIMIT 1`, appID, current.String).Scan(&previous); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store: read previous release for retention: %w", err)
		}
	} else {
		if err := s.db.QueryRow(`SELECT release_id FROM deployments WHERE app_id = ? AND promoted_at IS NOT NULL ORDER BY promoted_at DESC, created_at DESC, id DESC LIMIT 1`, appID).Scan(&previous); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store: read latest promoted release for retention: %w", err)
		}
	}
	if previous.Valid {
		keep[previous.String] = true
	}
	activeRows, err := s.db.Query(`SELECT DISTINCT release_id FROM deployments WHERE app_id = ? AND release_id IS NOT NULL AND
		(state IN ('QUEUED','BUILDING','BUILT','STARTING','HEALTH_CHECKING','RUNNING') OR (sandbox_id != '' AND stopped_at IS NULL))`, appID)
	if err != nil {
		return nil, fmt.Errorf("store: read in-flight releases for retention: %w", err)
	}
	for activeRows.Next() {
		var id string
		if err := activeRows.Scan(&id); err != nil {
			activeRows.Close()
			return nil, fmt.Errorf("store: scan in-flight release: %w", err)
		}
		keep[id] = true
	}
	if err := activeRows.Err(); err != nil {
		activeRows.Close()
		return nil, fmt.Errorf("store: read in-flight releases: %w", err)
	}
	activeRows.Close()
	result := make([]model.ReleasePruneCandidate, 0)
	for _, item := range all {
		if keep[item.id] {
			continue
		}
		var pruned int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pruned_release_artifacts WHERE release_id = ?`, item.id).Scan(&pruned); err != nil {
			return nil, fmt.Errorf("store: inspect pruned release: %w", err)
		}
		if pruned == 0 {
			result = append(result, model.ReleasePruneCandidate{ID: item.id, ArtifactDir: item.dir})
		}
	}
	return result, nil
}

func (s *Store) MarkReleaseArtifactPruned(releaseID string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO pruned_release_artifacts (release_id, pruned_at) VALUES (?, ?) ON CONFLICT(release_id) DO NOTHING`, releaseID, encodeTime(at.UTC()))
	if err != nil {
		return fmt.Errorf("store: record pruned release artifact: %w", err)
	}
	return nil
}

// DeleteApp removes all app-scoped control-plane records after the caller has
// stopped runtimes and handled source/artifact data according to the choice.
func (s *Store) DeleteApp(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin application deletion: %w", err)
	}
	defer tx.Rollback()
	var appID string
	if err := tx.QueryRow(`SELECT id FROM apps WHERE name = ?`, name).Scan(&appID); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: application %q", ErrNotFound, name)
	} else if err != nil {
		return fmt.Errorf("store: read application for deletion: %w", err)
	}
	for _, statement := range []string{
		`DELETE FROM routes WHERE deployment_id IN (SELECT id FROM deployments WHERE app_id = ?)`,
		`DELETE FROM events WHERE app_id = ?`,
		`DELETE FROM deployments WHERE app_id = ?`,
		`DELETE FROM releases WHERE app_id = ?`,
		`DELETE FROM app_settings WHERE app_id = ?`,
		`DELETE FROM app_operations WHERE app_id = ?`,
		`DELETE FROM apps WHERE id = ?`,
	} {
		if _, err := tx.Exec(statement, appID); err != nil {
			return fmt.Errorf("store: remove application data: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit application deletion: %w", err)
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// DeploymentTimingEvents returns only the earliest timing boundary per type.
// Ongoing probe events and repeated runtime failures do not inflate history reads.
func (s *Store) DeploymentTimingEvents(deploymentID string) ([]model.Event, error) {
	rows, err := s.db.Query(`SELECT type,MIN(created_at),MIN(id) FROM events WHERE deployment_id=? AND type IN (?,?,?,?,?,?,?,?,?) GROUP BY type ORDER BY MIN(created_at),MIN(id)`, deploymentID, model.EventBuildStarted, model.EventBuildCompleted, model.EventBuildFailed, model.EventSandboxStarting, model.EventSandboxStarted, model.EventHealthCheckPassed, model.EventHealthCheckFailed, model.EventDeploymentSucceeded, model.EventDeploymentFailed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []model.Event{}
	for rows.Next() {
		var event model.Event
		var at string
		if err = rows.Scan(&event.Type, &at, &event.ID); err != nil {
			return nil, err
		}
		event.CreatedAt, err = decodeTime(at)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
