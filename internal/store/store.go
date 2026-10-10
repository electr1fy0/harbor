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

const schemaVersion = 2

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
			systemd_unit, sandbox_id, memory_limit, cpu_quota, tasks_limit,
			created_at, started_at, stopped_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.AppID, nullableString(d.ReleaseID), d.Port, string(d.State), d.Error, d.PID, d.LogPath,
		d.SystemdUnit, d.SandboxID, d.MemoryLimit, d.CPUQuota, d.TasksLimit,
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
	rows, err := s.db.Query(deploymentSelect + `ORDER BY d.created_at DESC, d.id DESC`)
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
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin publish: %w", err)
	}
	defer tx.Rollback()

	d.State = model.Running
	if err := updateDeployment(tx, d); err != nil {
		return err
	}
	_, err = tx.Exec(`
		INSERT INTO routes (hostname, deployment_id) VALUES (?, ?)
		ON CONFLICT(hostname) DO UPDATE SET deployment_id = excluded.deployment_id`, hostname, d.ID)
	if err != nil {
		return fmt.Errorf("store: publish route: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit publish: %w", err)
	}
	return nil
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

const deploymentSelect = `
	SELECT d.id, d.app_id, a.name, d.release_id, d.port, d.state, d.error,
		d.pid, d.log_path, d.systemd_unit, d.sandbox_id, d.memory_limit, d.cpu_quota, d.tasks_limit,
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
	var startedAt, stoppedAt sql.NullString
	if err := row.Scan(
		&d.ID, &d.AppID, &d.AppName, &releaseID, &d.Port, &state, &d.Error,
		&d.PID, &d.LogPath, &d.SystemdUnit, &d.SandboxID, &d.MemoryLimit, &d.CPUQuota, &d.TasksLimit,
		&createdAt, &startedAt, &stoppedAt,
	); err != nil {
		return nil, err
	}
	d.ReleaseID = releaseID.String
	d.State = model.State(state)
	var err error
	if d.CreatedAt, err = decodeTime(createdAt); err != nil {
		return nil, fmt.Errorf("decode creation time: %w", err)
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
			log_path = ?, systemd_unit = ?, sandbox_id = ?, memory_limit = ?, cpu_quota = ?, tasks_limit = ?,
			created_at = ?, started_at = ?, stopped_at = ?
		WHERE id = ?`,
		d.AppID, nullableString(d.ReleaseID), d.Port, string(d.State), d.Error, d.PID,
		d.LogPath, d.SystemdUnit, d.SandboxID, d.MemoryLimit, d.CPUQuota, d.TasksLimit,
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
