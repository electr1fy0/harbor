package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"paas/internal/contracts"
	"paas/internal/model"

	_ "github.com/mattn/go-sqlite3"
)

var ErrNotFound = errors.New("store: not found")

const schemaVersion = 5

const timestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Store is the durable SQLite-backed application and deployment store.
type Store struct {
	db *sql.DB
}

// Open opens the SQLite database at path and applies any pending migrations.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: database path is empty")
	}

	dsn, err := makeDSN(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: connect to database: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate database: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// CreateApp creates an application, returning the existing record when name is
// already present.
func (s *Store) CreateApp(name string) (*model.Application, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("store: application name is empty")
	}
	id, err := randomID("app_")
	if err != nil {
		return nil, fmt.Errorf("store: generate application id: %w", err)
	}
	createdAt := time.Now().UTC()
	_, err = s.db.Exec(`
		INSERT INTO apps (id, name, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT(name) DO NOTHING`, id, name, encodeTime(createdAt))
	if err != nil {
		return nil, fmt.Errorf("store: create application: %w", err)
	}

	app := &model.Application{}
	var storedTime string
	if err := s.db.QueryRow(`SELECT id, name, created_at FROM apps WHERE name = ?`, name).
		Scan(&app.ID, &app.Name, &storedTime); err != nil {
		return nil, fmt.Errorf("store: read application after create: %w", err)
	}
	app.CreatedAt, err = decodeTime(storedTime)
	if err != nil {
		return nil, fmt.Errorf("store: decode application creation time: %w", err)
	}
	return app, nil
}

// CreateDeployment inserts a deployment. Its application and release must
// already exist.
func (s *Store) CreateDeployment(d model.Deployment) error {
	_, err := s.db.Exec(`
		INSERT INTO deployments (
			id, app_id, release_id, port, state, error, pid, log_path,
			systemd_unit, sandbox_type, sandbox_id, memory_limit, cpu_quota, tasks_limit, health_path, promoted_at,
			created_at, started_at, stopped_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.AppID, nullableString(d.ReleaseID), d.Port, string(d.State), d.Error, d.PID, d.LogPath,
		d.SystemdUnit, d.SandboxType, d.SandboxID, d.MemoryLimit, d.CPUQuota, d.TasksLimit, nullableHealthPath(d), nullableTime(d.PromotedAt),
		encodeTime(d.CreatedAt), nullableTime(d.StartedAt), nullableTime(d.StoppedAt))
	if err != nil {
		return fmt.Errorf("store: create deployment: %w", err)
	}
	return nil
}

// UpdateDeployment replaces a deployment's persisted fields.
func (s *Store) UpdateDeployment(d model.Deployment) error {
	return updateDeployment(s.db, d)
}

// SaveRelease stores an immutable release and its command.
func (s *Store) SaveRelease(appID string, r *contracts.Release) error {
	if r == nil {
		return errors.New("store: release is nil")
	}
	command, err := json.Marshal(r.Command)
	if err != nil {
		return fmt.Errorf("store: encode release command: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT INTO releases (
			id, app_id, source_hash, runtime, artifact_dir, command, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, appID, r.SourceHash, r.Runtime, r.ArtifactDir, string(command), encodeTime(r.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: save release: %w", err)
	}
	return nil
}

// Release returns the release with id.
func (s *Store) Release(id string) (*contracts.Release, error) {
	r, err := scanRelease(s.db.QueryRow(`
		SELECT id, source_hash, runtime, artifact_dir, command, created_at
		FROM releases WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: release %q", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read release: %w", err)
	}
	return r, nil
}

// Apps returns applications ordered by creation time and id.
func (s *Store) Apps() ([]model.Application, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at FROM apps ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list applications: %w", err)
	}
	defer rows.Close()

	apps := make([]model.Application, 0)
	for rows.Next() {
		var app model.Application
		var createdAt string
		if err := rows.Scan(&app.ID, &app.Name, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan application: %w", err)
		}
		app.CreatedAt, err = decodeTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("store: decode application creation time: %w", err)
		}
		apps = append(apps, app)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list applications: %w", err)
	}
	return apps, nil
}

// LatestDeployment returns the newest deployment for the named application.
func (s *Store) LatestDeployment(name string) (*model.Deployment, error) {
	d, err := scanDeployment(s.db.QueryRow(deploymentSelect+`
		WHERE a.name = ? ORDER BY d.created_at DESC, d.id DESC LIMIT 1`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: deployment for application %q", ErrNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read latest deployment: %w", err)
	}
	return d, nil
}

// ActiveDeployment returns the deployment currently mapped to hostname in routes.
func (s *Store) ActiveDeployment(name string) (*model.Deployment, error) {
	hostname := name + ".localhost"
	d, err := scanDeployment(s.db.QueryRow(deploymentSelect+`
		JOIN routes AS r ON r.deployment_id = d.id
		WHERE r.hostname = ?`, hostname))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: active deployment for application %q", ErrNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read active deployment: %w", err)
	}
	return d, nil
}

// Deployment returns the deployment with id.
func (s *Store) Deployment(id string) (*model.Deployment, error) {
	d, err := scanDeployment(s.db.QueryRow(deploymentSelect+`WHERE d.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: deployment %q", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read deployment: %w", err)
	}
	return d, nil
}

// Deployments returns deployments newest first.
func (s *Store) Deployments() ([]model.Deployment, error) {
	return s.queryDeployments(deploymentSelect + `ORDER BY d.created_at DESC, d.id DESC`)
}

// UnfinishedDeployments returns candidates, running deployments, and failed
// runtime cleanups. An empty name selects all applications. Completed history
// stays out of admission checks and the frequent supervision loop.
func (s *Store) UnfinishedDeployments(name string) ([]model.Deployment, error) {
	query := deploymentSelect + `WHERE (d.state IN (?, ?, ?, ?, ?, ?)
		OR (d.sandbox_id != '' AND d.stopped_at IS NULL))`
	args := []any{model.Queued, model.Building, model.Built, model.Starting, model.HealthChecking, model.Running}
	if name != "" {
		query += ` AND a.name = ?`
		args = append(args, name)
	}
	return s.queryDeployments(query+` ORDER BY d.created_at DESC, d.id DESC`, args...)
}

func (s *Store) queryDeployments(query string, args ...any) ([]model.Deployment, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list deployments: %w", err)
	}
	defer rows.Close()

	deployments := make([]model.Deployment, 0)
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan deployment: %w", err)
		}
		deployments = append(deployments, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list deployments: %w", err)
	}
	return deployments, nil
}

// Routes returns the current hostname-to-deployment mapping.
func (s *Store) Routes() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT hostname, deployment_id FROM routes ORDER BY hostname`)
	if err != nil {
		return nil, fmt.Errorf("store: list routes: %w", err)
	}
	defer rows.Close()

	routes := make(map[string]string)
	for rows.Next() {
		var hostname, deploymentID string
		if err := rows.Scan(&hostname, &deploymentID); err != nil {
			return nil, fmt.Errorf("store: scan route: %w", err)
		}
		routes[hostname] = deploymentID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list routes: %w", err)
	}
	return routes, nil
}

// Publish marks d as running and switches hostname to it in one transaction.
func (s *Store) Publish(d model.Deployment, hostname string) error {
	_, err := s.Promote(d, hostname)
	return err
}

// Finish stores d and removes hostname only when it still points to d.
func (s *Store) Finish(d model.Deployment, hostname string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin finish: %w", err)
	}
	defer tx.Rollback()

	if err := updateDeployment(tx, d); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM routes WHERE hostname = ? AND deployment_id = ?`, hostname, d.ID); err != nil {
		return fmt.Errorf("store: remove finished route: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit finish: %w", err)
	}
	return nil
}

// Promote atomically switches the route from any previous deployment to candidate d,
// marking candidate d as RUNNING. It returns the ID of the replaced deployment if one existed.
func (s *Store) Promote(d model.Deployment, hostname string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("store: begin promote: %w", err)
	}
	defer tx.Rollback()

	var previousDeploymentID sql.NullString
	if err := tx.QueryRow(`SELECT deployment_id FROM routes WHERE hostname = ?`, hostname).Scan(&previousDeploymentID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: read previous route: %w", err)
	}

	d.State = model.Running
	if d.PromotedAt == nil {
		now := time.Now().UTC()
		d.PromotedAt = &now
	}
	if err := updateDeployment(tx, d); err != nil {
		return "", err
	}
	_, err = tx.Exec(`
		INSERT INTO routes (hostname, deployment_id) VALUES (?, ?)
		ON CONFLICT(hostname) DO UPDATE SET deployment_id = excluded.deployment_id`, hostname, d.ID)
	if err != nil {
		return "", fmt.Errorf("store: promote route: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: commit promote: %w", err)
	}
	if previousDeploymentID.Valid && previousDeploymentID.String != d.ID {
		return previousDeploymentID.String, nil
	}
	return "", nil
}

// RecordEvent appends a lifecycle event for a deployment.
func (s *Store) RecordEvent(deploymentID, appID string, eventType model.EventType, payload string) error {
	_, err := s.db.Exec(`
		INSERT INTO events (deployment_id, app_id, type, payload, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		deploymentID, appID, string(eventType), payload, encodeTime(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("store: record event: %w", err)
	}
	return nil
}

// Events returns the lifecycle events for a deployment in chronological order.
func (s *Store) Events(deploymentID string) ([]model.Event, error) {
	rows, err := s.db.Query(`
		SELECT id, deployment_id, app_id, type, payload, created_at
		FROM events WHERE deployment_id = ? ORDER BY id ASC`, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer rows.Close()

	events := make([]model.Event, 0)
	for rows.Next() {
		var e model.Event
		var typeStr, createdAt string
		if err := rows.Scan(&e.ID, &e.DeploymentID, &e.AppID, &typeStr, &e.Payload, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		e.Type = model.EventType(typeStr)
		var decodeErr error
		if e.CreatedAt, decodeErr = decodeTime(createdAt); decodeErr != nil {
			return nil, fmt.Errorf("store: decode event creation time: %w", decodeErr)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	return events, nil
}

// ReleasesForApp returns all immutable releases for the application newest first,
// indicating which release is currently mapped to the active route.
func (s *Store) ReleasesForApp(appName string) ([]model.ReleaseView, error) {
	var appID string
	err := s.db.QueryRow(`SELECT id FROM apps WHERE name = ?`, appName).Scan(&appID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: application %q", ErrNotFound, appName)
	}
	if err != nil {
		return nil, fmt.Errorf("store: check application: %w", err)
	}

	rows, err := s.db.Query(`
		SELECT r.id, r.app_id, a.name, r.source_hash, r.runtime, r.artifact_dir, r.created_at,
		       CASE WHEN routes.deployment_id IS NOT NULL AND d.release_id = r.id THEN 1 ELSE 0 END AS is_active
		FROM releases AS r
		JOIN apps AS a ON a.id = r.app_id
		LEFT JOIN routes ON routes.hostname = a.name || '.localhost'
		LEFT JOIN deployments AS d ON d.id = routes.deployment_id
		WHERE a.name = ?
		ORDER BY r.created_at DESC, r.id DESC`, appName)
	if err != nil {
		return nil, fmt.Errorf("store: list releases: %w", err)
	}
	defer rows.Close()

	releases := make([]model.ReleaseView, 0)
	for rows.Next() {
		var r model.ReleaseView
		var createdAt string
		var isActive int
		if err := rows.Scan(&r.ID, &r.AppID, &r.AppName, &r.SourceHash, &r.Runtime, &r.ArtifactDir, &createdAt, &isActive); err != nil {
			return nil, fmt.Errorf("store: scan release view: %w", err)
		}
		var decodeErr error
		if r.CreatedAt, decodeErr = decodeTime(createdAt); decodeErr != nil {
			return nil, fmt.Errorf("store: decode release creation time: %w", decodeErr)
		}
		r.IsActive = isActive == 1
		releases = append(releases, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list releases: %w", err)
	}
	return releases, nil
}

// PreviousRelease returns the most recent successfully deployed release prior to the current active release.
func (s *Store) PreviousRelease(appName string) (*contracts.Release, error) {
	active, err := s.ActiveDeployment(appName)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	currentID := ""
	if active != nil {
		currentID = active.ReleaseID
	}
	r, err := scanRelease(s.db.QueryRow(`
		SELECT r.id, r.source_hash, r.runtime, r.artifact_dir, r.command, r.created_at
		FROM deployments AS d JOIN releases AS r ON r.id = d.release_id
		JOIN apps AS a ON a.id = d.app_id
		WHERE a.name = ? AND d.promoted_at IS NOT NULL AND r.id != ?
		ORDER BY d.promoted_at DESC, d.created_at DESC, d.id DESC LIMIT 1`, appName, currentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no previous release available for application %q", ErrNotFound, appName)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read previous release: %w", err)
	}
	return r, nil
}

// ReleaseForApp refuses release IDs owned by other applications.
func (s *Store) ReleaseForApp(appName, releaseID string) (*contracts.Release, error) {
	r, err := scanRelease(s.db.QueryRow(`
		SELECT r.id, r.source_hash, r.runtime, r.artifact_dir, r.command, r.created_at
		FROM releases AS r JOIN apps AS a ON a.id = r.app_id
		WHERE a.name = ? AND r.id = ?`, appName, releaseID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: release %q for application %q", ErrNotFound, releaseID, appName)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read application release: %w", err)
	}
	return r, nil
}

// DeploymentForRelease prefers the last successful launch configuration, falling
// back to the newest candidate when this release has never been promoted.
func (s *Store) DeploymentForRelease(releaseID string) (*model.Deployment, error) {
	d, err := scanDeployment(s.db.QueryRow(deploymentSelect+`WHERE d.release_id = ? ORDER BY d.promoted_at DESC, d.created_at DESC, d.id DESC LIMIT 1`, releaseID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: deployment for release %q", ErrNotFound, releaseID)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read deployment for release: %w", err)
	}
	return d, nil
}

const deploymentSelect = `
	SELECT d.id, d.app_id, a.name, d.release_id, d.port, d.state, d.error,
		d.pid, d.log_path, d.systemd_unit, d.sandbox_type, d.sandbox_id, d.memory_limit, d.cpu_quota, d.tasks_limit, d.health_path, d.promoted_at,
		d.created_at, d.started_at, d.stopped_at
	FROM deployments AS d
	JOIN apps AS a ON a.id = d.app_id
	`

type scanner interface {
	Scan(dest ...any) error
}

func scanDeployment(row scanner) (*model.Deployment, error) {
	var d model.Deployment
	var state, createdAt string
	var releaseID sql.NullString
	var startedAt, stoppedAt, promotedAt, healthPath sql.NullString
	if err := row.Scan(
		&d.ID, &d.AppID, &d.AppName, &releaseID, &d.Port, &state, &d.Error,
		&d.PID, &d.LogPath, &d.SystemdUnit, &d.SandboxType, &d.SandboxID, &d.MemoryLimit, &d.CPUQuota, &d.TasksLimit, &healthPath, &promotedAt,
		&createdAt, &startedAt, &stoppedAt,
	); err != nil {
		return nil, err
	}
	d.HealthPath, d.ReadinessKnown = healthPath.String, healthPath.Valid
	d.ReleaseID = releaseID.String
	d.State = model.State(state)
	var err error
	if d.CreatedAt, err = decodeTime(createdAt); err != nil {
		return nil, fmt.Errorf("decode creation time: %w", err)
	}
	if promotedAt.Valid {
		t, err := decodeTime(promotedAt.String)
		if err != nil {
			return nil, fmt.Errorf("decode promotion time: %w", err)
		}
		d.PromotedAt = &t
	}
	if startedAt.Valid {
		t, err := decodeTime(startedAt.String)
		if err != nil {
			return nil, fmt.Errorf("decode start time: %w", err)
		}
		d.StartedAt = &t
	}
	if stoppedAt.Valid {
		t, err := decodeTime(stoppedAt.String)
		if err != nil {
			return nil, fmt.Errorf("decode stop time: %w", err)
		}
		d.StoppedAt = &t
	}
	return &d, nil
}

func scanRelease(row scanner) (*contracts.Release, error) {
	var r contracts.Release
	var command, createdAt string
	if err := row.Scan(&r.ID, &r.SourceHash, &r.Runtime, &r.ArtifactDir, &command, &createdAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(command), &r.Command); err != nil {
		return nil, fmt.Errorf("decode command: %w", err)
	}
	var err error
	if r.CreatedAt, err = decodeTime(createdAt); err != nil {
		return nil, fmt.Errorf("decode creation time: %w", err)
	}
	return &r, nil
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func updateDeployment(db execer, d model.Deployment) error {
	result, err := db.Exec(`
		UPDATE deployments SET
			app_id = ?, release_id = ?, port = ?, state = ?, error = ?, pid = ?,
			log_path = ?, systemd_unit = ?, sandbox_type = ?, sandbox_id = ?, memory_limit = ?, cpu_quota = ?, tasks_limit = ?, health_path = ?, promoted_at = ?,
			created_at = ?, started_at = ?, stopped_at = ?
		WHERE id = ?`,
		d.AppID, nullableString(d.ReleaseID), d.Port, string(d.State), d.Error, d.PID,
		d.LogPath, d.SystemdUnit, d.SandboxType, d.SandboxID, d.MemoryLimit, d.CPUQuota, d.TasksLimit, nullableHealthPath(d), nullableTime(d.PromotedAt),
		encodeTime(d.CreatedAt), nullableTime(d.StartedAt), nullableTime(d.StoppedAt), d.ID)
	if err != nil {
		return fmt.Errorf("store: update deployment: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: check updated deployment: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: deployment %q", ErrNotFound, d.ID)
	}
	return nil
}

func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, schemaVersion)
	}
	if version < 1 {
		statements := []string{
			`CREATE TABLE apps (
				id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
				name TEXT NOT NULL UNIQUE CHECK (length(trim(name)) > 0),
				created_at TEXT NOT NULL
			)`,
			`CREATE TABLE releases (
				id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
				app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
				source_hash TEXT NOT NULL,
				runtime TEXT NOT NULL,
				artifact_dir TEXT NOT NULL,
				command TEXT NOT NULL CHECK (json_valid(command)),
				created_at TEXT NOT NULL
			)`,
			`CREATE TABLE deployments (
				id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
				app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
				release_id TEXT REFERENCES releases(id) ON DELETE RESTRICT,
				port INTEGER NOT NULL CHECK (port >= 0 AND port <= 65535),
				state TEXT NOT NULL,
				error TEXT NOT NULL,
				pid INTEGER NOT NULL CHECK (pid >= 0),
				log_path TEXT NOT NULL,
				created_at TEXT NOT NULL,
				started_at TEXT,
				stopped_at TEXT
			)`,
			`CREATE INDEX deployments_app_created ON deployments(app_id, created_at DESC, id DESC)`,
			`CREATE TABLE routes (
				hostname TEXT COLLATE NOCASE PRIMARY KEY NOT NULL CHECK (length(trim(hostname)) > 0),
				deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE
			)`,
		}
		for _, statement := range statements {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`PRAGMA user_version = 1`); err != nil {
			return err
		}
		version = 1
	}
	if version < 2 {
		statements := []string{
			`ALTER TABLE deployments ADD COLUMN systemd_unit TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE deployments ADD COLUMN sandbox_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE deployments ADD COLUMN memory_limit TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE deployments ADD COLUMN cpu_quota REAL NOT NULL DEFAULT 0`,
			`ALTER TABLE deployments ADD COLUMN tasks_limit INTEGER NOT NULL DEFAULT 0`,
		}
		for _, statement := range statements {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`PRAGMA user_version = 2`); err != nil {
			return err
		}
		version = 2
	}
	if version < 4 {
		// Two branches shipped distinct version-3 migrations. Reconcile both
		// schemas by inspecting the column, rather than trusting the version.
		var backendColumns int
		if err := tx.QueryRow(`SELECT count(*) FROM pragma_table_info('deployments') WHERE name = 'sandbox_type'`).Scan(&backendColumns); err != nil {
			return err
		}
		if backendColumns == 0 {
			if _, err := tx.Exec(`ALTER TABLE deployments ADD COLUMN sandbox_type TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
		var eventTables int
		if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'events'`).Scan(&eventTables); err != nil {
			return err
		}
		statements := []string{
			`CREATE TABLE IF NOT EXISTS events (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
				app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
				type TEXT NOT NULL CHECK (length(type) > 0),
				payload TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS events_deployment_idx ON events(deployment_id, id ASC)`,
			`CREATE INDEX IF NOT EXISTS events_app_idx ON events(app_id, id DESC)`,
			`CREATE INDEX IF NOT EXISTS releases_app_created_idx ON releases(app_id, created_at DESC, id DESC)`,
			`ALTER TABLE deployments ADD COLUMN health_path TEXT`,
			`ALTER TABLE deployments ADD COLUMN promoted_at TEXT`,
			`PRAGMA user_version = 4`,
		}
		for _, statement := range statements {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
		if eventTables > 0 {
			// Cycle 3 logged publication; terminal state alone cannot distinguish
			// a successful release from a canceled candidate later stopped.
			if _, err := tx.Exec(`UPDATE deployments SET promoted_at =
				(SELECT MAX(created_at) FROM events WHERE deployment_id = deployments.id AND type = 'DeploymentSucceeded')
				WHERE release_id IS NOT NULL`); err != nil {
				return err
			}
		} else {
			// Older schemas could publish only one runtime per app. Preserve the
			// history they recorded, without fabricating readiness configuration.
			if _, err := tx.Exec(`UPDATE deployments SET promoted_at = COALESCE(started_at, created_at)
				WHERE release_id IS NOT NULL AND state IN ('RUNNING', 'STOPPED') AND error = ''`); err != nil {
				return err
			}
		}
		version = 4
	}
	if version < 5 {
		statements := []string{
			`CREATE TABLE IF NOT EXISTS services (
				id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
				name TEXT NOT NULL UNIQUE CHECK (length(trim(name)) > 0),
				type TEXT NOT NULL,
				version TEXT NOT NULL,
				port INTEGER NOT NULL CHECK (port >= 0 AND port <= 65535),
				state TEXT NOT NULL,
				volume_dir TEXT NOT NULL,
				created_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS bindings (
				id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
				app_id TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
				service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
				database_name TEXT NOT NULL,
				username TEXT NOT NULL,
				password TEXT NOT NULL,
				env_key TEXT NOT NULL DEFAULT 'DATABASE_URL',
				created_at TEXT NOT NULL,
				UNIQUE(app_id, service_id)
			)`,
			`PRAGMA user_version = 5`,
		}
		for _, statement := range statements {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
		version = 5
	}
	return tx.Commit()
}

func makeDSN(path string) (string, error) {
	var u *url.URL
	if path == ":memory:" {
		id, err := randomID("memory_")
		if err != nil {
			return "", fmt.Errorf("store: generate in-memory database id: %w", err)
		}
		u = &url.URL{Scheme: "file", Opaque: id}
		query := url.Values{}
		query.Set("mode", "memory")
		query.Set("cache", "private")
		u.RawQuery = query.Encode()
	} else if strings.HasPrefix(path, "file:") {
		var err error
		u, err = url.Parse(path)
		if err != nil {
			return "", fmt.Errorf("store: parse database URI: %w", err)
		}
	} else {
		u = &url.URL{Scheme: "file", Path: path}
	}
	query := u.Query()
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	query.Set("_journal_mode", "WAL")
	query.Set("_synchronous", "NORMAL")
	query.Set("_txlock", "immediate")
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func randomID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func encodeTime(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}

func decodeTime(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return encodeTime(*t)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// A NULL path means a legacy release's readiness configuration is unknown.
// An explicitly empty path is a recorded TCP readiness check.
func nullableHealthPath(d model.Deployment) any {
	if !d.ReadinessKnown {
		return nil
	}
	return d.HealthPath
}

// CreateService creates a managed service record.
func (s *Store) CreateService(svc model.Service) error {
	if strings.TrimSpace(svc.ID) == "" {
		return errors.New("store: service id is empty")
	}
	if strings.TrimSpace(svc.Name) == "" {
		return errors.New("store: service name is empty")
	}
	_, err := s.db.Exec(`
		INSERT INTO services (id, name, type, version, port, state, volume_dir, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		svc.ID, svc.Name, svc.Type, svc.Version, svc.Port, string(svc.State), svc.VolumeDir, encodeTime(svc.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: create service: %w", err)
	}
	return nil
}

// GetService retrieves a service by name or id.
func (s *Store) GetService(nameOrID string) (*model.Service, error) {
	row := s.db.QueryRow(`
		SELECT id, name, type, version, port, state, volume_dir, created_at
		FROM services WHERE id = ? OR name = ?`, nameOrID, nameOrID)
	var svc model.Service
	var stateStr, timeStr string
	if err := row.Scan(&svc.ID, &svc.Name, &svc.Type, &svc.Version, &svc.Port, &stateStr, &svc.VolumeDir, &timeStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get service: %w", err)
	}
	svc.State = model.ServiceState(stateStr)
	createdAt, err := decodeTime(timeStr)
	if err != nil {
		return nil, err
	}
	svc.CreatedAt = createdAt
	return &svc, nil
}

// ListServices lists all services.
func (s *Store) ListServices() ([]model.Service, error) {
	rows, err := s.db.Query(`
		SELECT id, name, type, version, port, state, volume_dir, created_at
		FROM services ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list services: %w", err)
	}
	defer rows.Close()
	var list []model.Service
	for rows.Next() {
		var svc model.Service
		var stateStr, timeStr string
		if err := rows.Scan(&svc.ID, &svc.Name, &svc.Type, &svc.Version, &svc.Port, &stateStr, &svc.VolumeDir, &timeStr); err != nil {
			return nil, fmt.Errorf("store: scan service: %w", err)
		}
		svc.State = model.ServiceState(stateStr)
		createdAt, err := decodeTime(timeStr)
		if err != nil {
			return nil, err
		}
		svc.CreatedAt = createdAt
		list = append(list, svc)
	}
	return list, rows.Err()
}

// UpdateServiceState updates a service's state.
func (s *Store) UpdateServiceState(id string, state model.ServiceState) error {
	res, err := s.db.Exec(`UPDATE services SET state = ? WHERE id = ?`, string(state), id)
	if err != nil {
		return fmt.Errorf("store: update service state: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateBinding records an application-to-service link.
func (s *Store) CreateBinding(b model.Binding) error {
	_, err := s.db.Exec(`
		INSERT INTO bindings (id, app_id, service_id, database_name, username, password, env_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(app_id, service_id) DO UPDATE SET
			database_name = excluded.database_name,
			username = excluded.username,
			password = excluded.password,
			env_key = excluded.env_key`,
		b.ID, b.AppID, b.ServiceID, b.DatabaseName, b.Username, b.Password, b.EnvKey, encodeTime(b.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: create binding: %w", err)
	}
	return nil
}

// GetBindingsForApp returns all service bindings for an application.
func (s *Store) GetBindingsForApp(appID string) ([]model.Binding, error) {
	rows, err := s.db.Query(`
		SELECT id, app_id, service_id, database_name, username, password, env_key, created_at
		FROM bindings WHERE app_id = ? ORDER BY created_at ASC`, appID)
	if err != nil {
		return nil, fmt.Errorf("store: get bindings: %w", err)
	}
	defer rows.Close()
	var list []model.Binding
	for rows.Next() {
		var b model.Binding
		var timeStr string
		if err := rows.Scan(&b.ID, &b.AppID, &b.ServiceID, &b.DatabaseName, &b.Username, &b.Password, &b.EnvKey, &timeStr); err != nil {
			return nil, fmt.Errorf("store: scan binding: %w", err)
		}
		createdAt, err := decodeTime(timeStr)
		if err != nil {
			return nil, err
		}
		b.CreatedAt = createdAt
		list = append(list, b)
	}
	return list, rows.Err()
}
