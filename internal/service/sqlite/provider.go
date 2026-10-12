// Package sqlite implements file-backed managed SQLite services.
package sqlite

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"paas/internal/contracts"
)

const markerName = ".harbor-sqlite-service"

var _ contracts.ServiceProvider = (*Provider)(nil)

// Provider manages durable service-volume directories. SQLite databases are
// provisioned individually by the binding manager, so no service process runs.
type Provider struct {
	dataDir string
}

// NewProvider creates a file-backed SQLite provider.
func NewProvider(dataDir string) *Provider {
	return &Provider{dataDir: dataDir}
}

// Create creates a private persistent service volume and an identity marker.
func (p *Provider) Create(ctx context.Context, req contracts.ServiceRequest) (*contracts.Service, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Type != "sqlite" {
		return nil, fmt.Errorf("unsupported service type %q: only sqlite is supported", req.Type)
	}
	if strings.TrimSpace(req.Name) == "" {
		return nil, errors.New("service name is required")
	}
	if p.dataDir == "" || !filepath.IsAbs(p.dataDir) {
		return nil, errors.New("service data directory must be an absolute path")
	}

	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, fmt.Errorf("generate service id: %w", err)
	}
	id := "svc_" + hex.EncodeToString(idBytes)
	volumesDir := filepath.Join(p.dataDir, "volumes")
	if err := os.MkdirAll(volumesDir, 0711); err != nil {
		return nil, fmt.Errorf("create service volumes directory: %w", err)
	}
	volumesInfo, err := os.Lstat(volumesDir)
	if err != nil {
		return nil, fmt.Errorf("inspect service volumes directory: %w", err)
	}
	if volumesInfo.Mode()&os.ModeSymlink != 0 || !volumesInfo.IsDir() {
		return nil, errors.New("service volumes path is not a real directory")
	}
	if err := os.Chmod(volumesDir, 0711); err != nil {
		return nil, fmt.Errorf("secure service volumes directory: %w", err)
	}
	volumeDir := filepath.Join(volumesDir, id)
	if err := os.Mkdir(volumeDir, 0711); err != nil {
		return nil, fmt.Errorf("create service volume: %w", err)
	}
	if err := os.Chmod(volumeDir, 0711); err != nil {
		return nil, errors.Join(fmt.Errorf("secure service volume: %w", err), os.RemoveAll(volumeDir))
	}
	if err := writeMarker(ctx, filepath.Join(volumeDir, markerName), id); err != nil {
		return nil, errors.Join(fmt.Errorf("initialize service volume: %w", err), os.RemoveAll(volumeDir))
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, os.RemoveAll(volumeDir))
	}

	return &contracts.Service{
		ID:        id,
		Name:      req.Name,
		Type:      "sqlite",
		Version:   "3",
		Port:      0,
		State:     "CREATED",
		VolumeDir: volumeDir,
	}, nil
}

// DiscardUnpersisted removes a newly-created service volume only when it is
// still the exact empty-of-bindings volume owned by this provider. It is used
// when the service record could not be persisted.
func (p *Provider) DiscardUnpersisted(svc *contracts.Service) error {
	if svc == nil || svc.Type != "sqlite" || svc.ID == "" {
		return errors.New("cannot discard an unidentified SQLite service volume")
	}
	dataDir, err := filepath.Abs(p.dataDir)
	if err != nil {
		return err
	}
	volumeDir := filepath.Clean(svc.VolumeDir)
	if !filepath.IsAbs(volumeDir) || filepath.Dir(volumeDir) != filepath.Join(dataDir, "volumes") || filepath.Base(volumeDir) != svc.ID {
		return errors.New("refusing to discard a service volume outside the provider's volume directory")
	}
	info, err := os.Lstat(volumeDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect unpersisted service volume: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("refusing to discard a service volume that is not a real directory")
	}
	markerPath := filepath.Join(volumeDir, markerName)
	markerInfo, err := os.Lstat(markerPath)
	if err != nil {
		return fmt.Errorf("inspect unpersisted service marker: %w", err)
	}
	if markerInfo.Mode()&os.ModeSymlink != 0 || !markerInfo.Mode().IsRegular() {
		return errors.New("refusing to discard service volume with an invalid marker")
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		return fmt.Errorf("read unpersisted service marker: %w", err)
	}
	if strings.TrimSpace(string(marker)) != svc.ID {
		return errors.New("refusing to discard service volume with a mismatched marker")
	}
	entries, err := os.ReadDir(volumeDir)
	if err != nil {
		return fmt.Errorf("inspect unpersisted service contents: %w", err)
	}
	if len(entries) != 1 || entries[0].Name() != markerName {
		return errors.New("refusing to discard a service volume that contains data")
	}
	return os.RemoveAll(volumeDir)
}

func writeMarker(ctx context.Context, path, id string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(file, id); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return ctx.Err()
}

// Start validates the persisted volume marker. It does not create or repair
// missing storage during restore.
func (p *Provider) Start(ctx context.Context, svc *contracts.Service) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if svc == nil || svc.Type != "sqlite" {
		return errors.New("cannot start a non-SQLite managed service")
	}
	info, err := os.Lstat(svc.VolumeDir)
	if err != nil {
		return fmt.Errorf("inspect SQLite service volume %q: %w", svc.VolumeDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("SQLite service volume %q is not a real directory", svc.VolumeDir)
	}
	markerPath := filepath.Join(svc.VolumeDir, markerName)
	markerInfo, err := os.Lstat(markerPath)
	if err != nil {
		return fmt.Errorf("SQLite service volume marker is unavailable at %q: %w", markerPath, err)
	}
	if markerInfo.Mode()&os.ModeSymlink != 0 || !markerInfo.Mode().IsRegular() {
		return fmt.Errorf("SQLite service volume marker at %q is not a regular file", markerPath)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		return fmt.Errorf("read SQLite service volume marker: %w", err)
	}
	if strings.TrimSpace(string(marker)) != svc.ID {
		return fmt.Errorf("SQLite service volume marker identity mismatch: expected %q", svc.ID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	svc.State = "READY"
	return nil
}

// Stop is a no-op because managed SQLite services have no process.
func (*Provider) Stop(ctx context.Context, _ string) error {
	return ctx.Err()
}

// ConnectionInfo returns the driver contract. The database URL is per binding.
func (*Provider) ConnectionInfo(ctx context.Context, svc *contracts.Service) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if svc == nil || svc.Type != "sqlite" {
		return nil, errors.New("connection info is only available for SQLite services")
	}
	return map[string]string{"driver": "sqlite"}, nil
}
