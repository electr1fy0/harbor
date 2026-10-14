package service

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"paas/internal/model"
	"paas/internal/store"
)

var databaseNamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,127}$`)

// BindingManager provisions a private, durable SQLite file for each
// application/service binding.
type BindingManager struct {
	store              *store.Store
	initializeDatabase func(context.Context, string) error
	mu                 sync.Mutex
}

// NewBindingManager creates a BindingManager.
func NewBindingManager(st *store.Store) *BindingManager {
	return &BindingManager{store: st, initializeDatabase: initializeSQLiteDatabase}
}

// Link creates a binding and its SQLite file, or returns the existing binding
// unchanged when the same app is linked to the same service again.
func (m *BindingManager) Link(ctx context.Context, app *model.Application, svc *model.Service) (*model.Binding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if app == nil || app.ID == "" {
		return nil, errors.New("application identity is required to create a service binding")
	}
	if svc == nil || svc.ID == "" {
		return nil, errors.New("service identity is required to create a service binding")
	}
	if svc.Type != "sqlite" {
		if svc.Type == "postgres" {
			return nil, legacyPostgresError(svc.Name)
		}
		return nil, fmt.Errorf("service type %q does not support database provisioning", svc.Type)
	}
	if svc.State != model.ServiceReady {
		return nil, fmt.Errorf("SQLite service %q is %s; only READY services can be linked", svc.Name, svc.State)
	}

	bindings, err := m.store.GetBindingsForApp(app.ID)
	if err != nil {
		return nil, fmt.Errorf("list existing application service bindings: %w", err)
	}
	for i := range bindings {
		binding := &bindings[i]
		if binding.ServiceID == svc.ID {
			if binding.EnvKey != "DATABASE_URL" {
				return nil, fmt.Errorf("existing binding for SQLite service %q does not use DATABASE_URL", svc.Name)
			}
			path, err := DatabasePath(binding, svc)
			if err != nil {
				return nil, fmt.Errorf("validate existing SQLite binding: %w", err)
			}
			if err := validateSQLiteDatabase(ctx, path); err != nil {
				return nil, fmt.Errorf("validate existing SQLite database for application %q: %w", app.Name, err)
			}
			return binding, nil
		}
		if binding.EnvKey == "DATABASE_URL" {
			other, lookupErr := m.store.GetService(binding.ServiceID)
			if lookupErr == nil && other.Type == "postgres" {
				return nil, legacyPostgresError(other.Name)
			}
			return nil, fmt.Errorf("application %q already has a DATABASE_URL binding to service %q; explicitly migrate that application and its data before switching services", app.Name, serviceName(other, binding.ServiceID))
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	bindingIDBytes := make([]byte, 8)
	if _, err := rand.Read(bindingIDBytes); err != nil {
		return nil, fmt.Errorf("generate binding id: %w", err)
	}
	binding := model.Binding{
		ID:           "bind_" + hex.EncodeToString(bindingIDBytes),
		AppID:        app.ID,
		ServiceID:    svc.ID,
		DatabaseName: "db_" + app.ID,
		EnvKey:       "DATABASE_URL",
		CreatedAt:    time.Now().UTC(),
	}
	path, err := bindingDatabasePath(&binding, svc)
	if err != nil {
		return nil, fmt.Errorf("prepare SQLite database path: %w", err)
	}
	createdDirectory, err := prepareSQLiteDatabase(ctx, path, m.initializeDatabase)
	if err != nil {
		cleanupErr := m.cleanupUnboundDatabaseDirectory(path, createdDirectory, app.ID, svc.ID)
		return nil, errors.Join(fmt.Errorf("create SQLite database for application %q: %w", app.Name, err), cleanupErr)
	}
	if err := ctx.Err(); err != nil {
		cleanupErr := m.cleanupUnboundDatabaseDirectory(path, createdDirectory, app.ID, svc.ID)
		return nil, errors.Join(err, cleanupErr)
	}
	if _, err := DatabaseURL(&binding, svc); err != nil {
		cleanupErr := m.cleanupUnboundDatabaseDirectory(path, createdDirectory, app.ID, svc.ID)
		return nil, errors.Join(fmt.Errorf("build SQLite database URL: %w", err), cleanupErr)
	}
	if err := m.store.CreateBinding(binding); err != nil {
		cleanupErr := m.cleanupUnboundDatabaseDirectory(path, createdDirectory, app.ID, svc.ID)
		return nil, errors.Join(fmt.Errorf("store SQLite binding: %w", err), cleanupErr)
	}
	return &binding, nil
}

func (m *BindingManager) cleanupUnboundDatabaseDirectory(path string, created bool, appID, serviceID string) error {
	if !created {
		return nil
	}
	bindings, err := m.store.GetBindingsForApp(appID)
	if err != nil {
		return fmt.Errorf("check binding before cleaning up new SQLite directory: %w", err)
	}
	for _, binding := range bindings {
		if binding.ServiceID == serviceID {
			return nil
		}
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		return fmt.Errorf("remove unbound SQLite database directory: %w", err)
	}
	return nil
}

func serviceName(svc *model.Service, fallback string) string {
	if svc != nil && svc.Name != "" {
		return svc.Name
	}
	return fallback
}

// DatabasePath returns the durable database file for a SQLite binding. It
// requires that the file already exists, so deployment cannot recreate a lost
// database while preparing a runtime.
func DatabasePath(b *model.Binding, svc *model.Service) (string, error) {
	path, err := bindingDatabasePath(b, svc)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("SQLite database %q is unavailable: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("SQLite database %q is not a regular file", path)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("SQLite database %q is empty", path)
	}
	return path, nil
}

// DatabaseURL returns an escaped SQLite file URI configured for an existing
// database, foreign keys, and bounded lock waits.
func DatabaseURL(b *model.Binding, svc *model.Service) (string, error) {
	path, err := DatabasePath(b, svc)
	if err != nil {
		return "", err
	}
	return databaseURI(path), nil
}

func databaseURI(path string) string {
	query := url.Values{}
	query.Set("mode", "rw")
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	u := url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}
	return u.String()
}

// bindingDatabasePath validates ownership and the path's lexical and
// filesystem boundaries. It permits the final database to be absent only for
// initial provisioning.
func bindingDatabasePath(b *model.Binding, svc *model.Service) (string, error) {
	if b == nil || svc == nil {
		return "", errors.New("binding and service are required")
	}
	if svc.Type != "sqlite" {
		return "", fmt.Errorf("service %q has type %q; SQLite file paths require a sqlite service", svc.Name, svc.Type)
	}
	if svc.ID == "" || b.ServiceID != svc.ID {
		return "", errors.New("SQLite binding does not belong to the requested service")
	}
	if b.AppID == "" || b.DatabaseName != "db_"+b.AppID {
		return "", errors.New("SQLite binding database directory does not match its application owner")
	}
	if !databaseNamePattern.MatchString(b.DatabaseName) {
		return "", fmt.Errorf("unsafe SQLite binding directory name %q", b.DatabaseName)
	}
	volumeDir := filepath.Clean(svc.VolumeDir)
	if !filepath.IsAbs(volumeDir) || volumeDir == string(filepath.Separator) {
		return "", errors.New("SQLite service volume must be a non-root absolute path")
	}
	if filepath.Clean(svc.VolumeDir) != svc.VolumeDir {
		return "", errors.New("SQLite service volume path must be clean")
	}
	path := filepath.Join(volumeDir, b.DatabaseName, "database.sqlite")
	rel, err := filepath.Rel(volumeDir, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", errors.New("SQLite database path escapes the service volume")
	}
	if err := rejectSQLiteSymlinks(volumeDir, path); err != nil {
		return "", err
	}
	return path, nil
}

func rejectSQLiteSymlinks(volumeDir, databasePath string) error {
	volumeInfo, err := os.Lstat(volumeDir)
	if err != nil {
		return fmt.Errorf("inspect SQLite service volume %q: %w", volumeDir, err)
	}
	if volumeInfo.Mode()&os.ModeSymlink != 0 || !volumeInfo.IsDir() {
		return fmt.Errorf("SQLite service volume %q is not a real directory", volumeDir)
	}
	databaseDir := filepath.Dir(databasePath)
	for _, item := range []struct {
		path string
		dir  bool
	}{{databaseDir, true}, {databasePath, false}, {databasePath + "-wal", false}, {databasePath + "-shm", false}, {databasePath + "-journal", false}} {
		info, err := os.Lstat(item.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect SQLite path %q: %w", item.path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("SQLite path %q must not be a symlink", item.path)
		}
		if item.dir && !info.IsDir() {
			return fmt.Errorf("SQLite binding path %q is not a directory", item.path)
		}
		if !item.dir && !info.Mode().IsRegular() {
			return fmt.Errorf("SQLite database sidecar %q is not a regular file", item.path)
		}
	}
	return nil
}

func createSQLiteDatabase(ctx context.Context, path string, initialize func(context.Context, string) error) (bool, error) {
	databaseDir := filepath.Dir(path)
	if err := os.Mkdir(databaseDir, 0700); err != nil {
		return false, fmt.Errorf("create private database directory %q: %w", databaseDir, err)
	}
	if err := os.Chmod(databaseDir, 0700); err != nil {
		return true, fmt.Errorf("secure database directory %q: %w", databaseDir, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return true, fmt.Errorf("create database file %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return true, fmt.Errorf("close new database file: %w", err)
	}
	if initialize == nil {
		initialize = initializeSQLiteDatabase
	}
	if err := initialize(ctx, path); err != nil {
		return true, err
	}
	return true, nil
}

// prepareSQLiteDatabase reuses a complete, valid orphan database left by an
// unlink operation. Unlink removes the control-plane binding while preserving
// application data; a later link therefore reconnects to that same file.
func prepareSQLiteDatabase(ctx context.Context, path string, initialize func(context.Context, string) error) (bool, error) {
	directory := filepath.Dir(path)
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return createSQLiteDatabase(ctx, path, initialize)
	}
	if err != nil {
		return false, fmt.Errorf("inspect existing SQLite database directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("existing SQLite database directory is unsafe")
	}
	if info.Mode().Perm() != 0700 {
		if err := os.Chmod(directory, 0700); err != nil {
			return false, errors.New("existing SQLite database directory permissions are unsafe")
		}
	}
	if err := validateSQLiteDatabase(ctx, path); err != nil {
		return false, fmt.Errorf("existing SQLite database cannot be safely re-linked: %w", err)
	}
	return false, nil
}

func initializeSQLiteDatabase(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite3", databaseURI(path))
	if err != nil {
		return fmt.Errorf("open new SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to new SQLite database: %w", err)
	}
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("enable SQLite WAL mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("SQLite refused WAL mode and selected %q", journalMode)
	}
	return checkSQLiteIntegrity(ctx, db)
}

func validateSQLiteDatabase(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("database file is unavailable: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("database file is not a regular file")
	}
	if info.Size() == 0 {
		return errors.New("database file is empty")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read database header: %w", err)
	}
	var header [16]byte
	_, readErr := io.ReadFull(file, header[:])
	closeErr := file.Close()
	if readErr != nil {
		return fmt.Errorf("read database header: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close database header: %w", closeErr)
	}
	if string(header[:]) != "SQLite format 3\x00" {
		return errors.New("database file does not have a valid SQLite header")
	}
	db, err := sql.Open("sqlite3", databaseURI(path))
	if err != nil {
		return fmt.Errorf("open database for integrity check: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("open database for integrity check: %w", err)
	}
	return checkSQLiteIntegrity(ctx, db)
}

func checkSQLiteIntegrity(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return fmt.Errorf("run SQLite quick_check: %w", err)
	}
	defer rows.Close()
	var checks int
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("read SQLite quick_check: %w", err)
		}
		checks++
		if result != "ok" {
			return fmt.Errorf("SQLite quick_check failed: %s", result)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read SQLite quick_check: %w", err)
	}
	if checks == 0 {
		return errors.New("SQLite quick_check returned no result")
	}
	return nil
}

func legacyPostgresError(name string) error {
	if name == "" {
		name = "unknown"
	}
	return fmt.Errorf("legacy PostgreSQL service %q is unavailable: explicitly migrate the application's data into a SQLite database, update the application's database driver and configuration, then create a SQLite service and binding; PostgreSQL volume data is not a SQLite database", name)
}
