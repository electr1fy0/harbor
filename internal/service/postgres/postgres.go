package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"paas/internal/contracts"
	"paas/internal/runtime"
)

var _ contracts.ServiceProvider = (*Provider)(nil)

// Provider implements contracts.ServiceProvider for PostgreSQL.
type Provider struct {
	dataDir string
	runtime *runtime.Manager
	sandbox contracts.Sandbox
	mu      sync.Mutex
}

// NewProvider creates a new PostgreSQL Provider.
func NewProvider(dataDir string, mgr *runtime.Manager, sb contracts.Sandbox) *Provider {
	return &Provider{
		dataDir: dataDir,
		runtime: mgr,
		sandbox: sb,
	}
}

// Create provisions the volume and initializes a fresh database cluster if needed.
func (p *Provider) Create(ctx context.Context, req contracts.ServiceRequest) (*contracts.Service, error) {
	if req.Type != "postgres" {
		return nil, fmt.Errorf("unsupported service type %q: only postgres is supported", req.Type)
	}
	if req.Name == "" {
		return nil, fmt.Errorf("service name is required")
	}

	idBytes := make([]byte, 6)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, fmt.Errorf("generate service id: %w", err)
	}
	id := "svc_" + hex.EncodeToString(idBytes)

	port, err := findFreePort()
	if err != nil {
		return nil, fmt.Errorf("allocate loopback port: %w", err)
	}

	volumeDir := filepath.Join(p.dataDir, "volumes", id, "data")
	if err := os.MkdirAll(volumeDir, 0700); err != nil {
		return nil, fmt.Errorf("create volume directory: %w", err)
	}

	rootfs, err := p.runtime.EnsurePostgresRootFS(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare rootfs: %w", err)
	}

	// If database cluster is not initialized, run initdb inside bubblewrap.
	pgVersionFile := filepath.Join(volumeDir, "PG_VERSION")
	if _, err := os.Stat(pgVersionFile); os.IsNotExist(err) {
		if err := p.runInitDB(ctx, rootfs, volumeDir); err != nil {
			return nil, fmt.Errorf("initdb failed: %w", err)
		}
	}

	svc := &contracts.Service{
		ID:        id,
		Name:      req.Name,
		Type:      "postgres",
		Version:   "16",
		Port:      port,
		State:     "CREATED",
		VolumeDir: volumeDir,
	}
	return svc, nil
}

// Start launches the PostgreSQL service process inside the sandbox and checks readiness.
func (p *Provider) Start(ctx context.Context, svc *contracts.Service) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	rootfs, err := p.runtime.EnsurePostgresRootFS(ctx)
	if err != nil {
		return fmt.Errorf("prepare rootfs: %w", err)
	}

	spec := contracts.SandboxSpec{
		ID:        svc.ID,
		RootFS:    rootfs,
		UID:       70,
		GID:       70,
		MemoryMax: "256Mi",
		CPUQuota:  1.0,
		TasksMax:  128,
		Binds: []contracts.BindMount{
			{HostPath: svc.VolumeDir, MountPath: "/data", ReadOnly: false},
		},
		Command: []string{
			"/usr/local/bin/postgres",
			"-D", "/data",
			"-p", strconv.Itoa(svc.Port),
			"-h", "127.0.0.1",
			"-k", "/tmp",
		},
	}

	if _, err := p.sandbox.Start(ctx, spec); err != nil {
		return fmt.Errorf("start postgres sandbox: %w", err)
	}

	// Poll readiness
	deadline := time.Now().Add(15 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", svc.Port), 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !ready {
		_ = p.sandbox.Stop(ctx, svc.ID)
		return fmt.Errorf("postgres service failed to become ready on port %d", svc.Port)
	}

	svc.State = "READY"
	return nil
}

// Stop stops the running PostgreSQL service.
func (p *Provider) Stop(ctx context.Context, id string) error {
	return p.sandbox.Stop(ctx, id)
}

// ConnectionInfo returns loopback connection parameters for the service.
func (p *Provider) ConnectionInfo(ctx context.Context, svc *contracts.Service) (map[string]string, error) {
	return map[string]string{
		"host":     "127.0.0.1",
		"port":     strconv.Itoa(svc.Port),
		"user":     "postgres",
		"database": "postgres",
	}, nil
}

func (p *Provider) runInitDB(ctx context.Context, rootfs, volumeDir string) error {
	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		return fmt.Errorf("bwrap not found: %w", err)
	}

	args := []string{
		"--unshare-user",
		"--uid", "70",
		"--gid", "70",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--ro-bind", rootfs, "/",
		"--bind", volumeDir, "/data",
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev", "/dev",
		"--setenv", "PATH", "/usr/local/bin:/usr/bin:/bin",
		"--",
		"/usr/local/bin/initdb",
		"-D", "/data",
		"--auth=trust",
		"-U", "postgres",
		"-E", "UTF8",
	}

	cmd := exec.CommandContext(ctx, bwrapPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(output))
	}
	return nil
}

func findFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
