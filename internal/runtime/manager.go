package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// DefaultPostgresImage is the pinned OCI image for PostgreSQL service.
	DefaultPostgresImage = "docker://docker.io/library/postgres:16-alpine"
	DefaultPostgresTag   = "postgres-16"
)

// Manager manages prepared runtime and service root filesystems.
type Manager struct {
	dataDir string
}

// NewManager creates a runtime Manager anchored at dataDir.
func NewManager(dataDir string) *Manager {
	return &Manager{dataDir: dataDir}
}

// ServiceCacheDir returns the directory where managed service root filesystems are cached.
func (m *Manager) ServiceCacheDir() string {
	return filepath.Join(m.dataDir, "services", "cache")
}

// EnsurePostgresRootFS ensures the pinned PostgreSQL root filesystem is unpacked and returns its rootfs path.
func (m *Manager) EnsurePostgresRootFS(ctx context.Context) (string, error) {
	targetDir := filepath.Join(m.ServiceCacheDir(), DefaultPostgresTag)
	rootfs := filepath.Join(targetDir, "rootfs")
	if info, err := os.Stat(rootfs); err == nil && info.IsDir() {
		return rootfs, nil
	}

	if home, err := os.UserHomeDir(); err == nil {
		globalRootfs := filepath.Join(home, ".cache", "paas", "services", DefaultPostgresTag, "rootfs")
		if info, err := os.Stat(globalRootfs); err == nil && info.IsDir() {
			return globalRootfs, nil
		}
	}

	if err := FetchAndUnpack(ctx, DefaultPostgresImage, targetDir); err != nil {
		return "", fmt.Errorf("prepare postgres rootfs: %w", err)
	}
	return rootfs, nil
}
