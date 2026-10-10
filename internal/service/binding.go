package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"paas/internal/model"
	"paas/internal/runtime"
	"paas/internal/store"
)

// BindingManager manages database and user provisioning when an application links to a service.
type BindingManager struct {
	store   *store.Store
	runtime *runtime.Manager
}

// NewBindingManager creates a new BindingManager.
func NewBindingManager(st *store.Store, rm *runtime.Manager) *BindingManager {
	return &BindingManager{store: st, runtime: rm}
}

// Link provisions an isolated user and database in PostgreSQL and records the binding.
func (m *BindingManager) Link(ctx context.Context, app *model.Application, svc *model.Service) (*model.Binding, error) {
	if svc.Type != "postgres" {
		return nil, fmt.Errorf("service type %q does not support database provisioning", svc.Type)
	}

	userSuffix := make([]byte, 4)
	if _, err := rand.Read(userSuffix); err != nil {
		return nil, fmt.Errorf("generate credentials: %w", err)
	}
	dbUser := fmt.Sprintf("u_%s", hex.EncodeToString(userSuffix))

	passBytes := make([]byte, 8)
	if _, err := rand.Read(passBytes); err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}
	dbPass := hex.EncodeToString(passBytes)

	dbName := fmt.Sprintf("db_%s", hex.EncodeToString(userSuffix))

	rootfs, err := m.runtime.EnsurePostgresRootFS(ctx)
	if err != nil {
		return nil, fmt.Errorf("ensure postgres rootfs: %w", err)
	}

	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("bwrap not found: %w", err)
	}

	createUserSQL := fmt.Sprintf("CREATE USER %s WITH PASSWORD '%s';", dbUser, dbPass)
	createDBSQL := fmt.Sprintf("CREATE DATABASE %s OWNER %s;", dbName, dbUser)
	args := []string{
		"--unshare-user",
		"--uid", "70",
		"--gid", "70",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--share-net",
		"--ro-bind", rootfs, "/",
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev", "/dev",
		"--setenv", "PATH", "/usr/local/bin:/usr/bin:/bin",
		"--",
		"/usr/local/bin/psql",
		"-h", "127.0.0.1",
		"-p", strconv.Itoa(svc.Port),
		"-U", "postgres",
		"-d", "postgres",
		"-c", createUserSQL,
		"-c", createDBSQL,
	}

	cmd := exec.CommandContext(ctx, bwrapPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("provision postgres user/db: %w: %s", err, string(output))
	}

	bindingIDBytes := make([]byte, 6)
	if _, err := rand.Read(bindingIDBytes); err != nil {
		return nil, fmt.Errorf("generate binding id: %w", err)
	}
	bindingID := "bind_" + hex.EncodeToString(bindingIDBytes)

	binding := model.Binding{
		ID:           bindingID,
		AppID:        app.ID,
		ServiceID:    svc.ID,
		DatabaseName: dbName,
		Username:     dbUser,
		Password:     dbPass,
		EnvKey:       "DATABASE_URL",
		CreatedAt:    time.Now().UTC(),
	}

	if err := m.store.CreateBinding(binding); err != nil {
		return nil, fmt.Errorf("store binding: %w", err)
	}

	return &binding, nil
}

// FormatDatabaseURL constructs the PostgreSQL connection string.
func FormatDatabaseURL(b *model.Binding, port int) string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable",
		b.Username, b.Password, port, b.DatabaseName)
}
