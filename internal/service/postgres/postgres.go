package postgres

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"paas/internal/contracts"
	"paas/internal/runtime"
	"paas/internal/sandbox"
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if existing, inspectErr := p.sandbox.Inspect(ctx, svc.ID); inspectErr == nil && existing != nil && existing.Running {
		if existing.ID != svc.ID {
			return errors.New("refusing to adopt a postgres sandbox with a mismatched identity")
		}
		if readinessErr := waitReady(ctx, svc.Port); readinessErr == nil {
			svc.State = "READY"
			return nil
		} else if cleanupErr := p.stopUnreadySandbox(svc.ID); cleanupErr != nil {
			return errors.Join(readinessErr, cleanupErr)
		}
	} else if inspectErr != nil && !errors.Is(inspectErr, sandbox.ErrSandboxNotFound) {
		return fmt.Errorf("inspect postgres sandbox: %w", inspectErr)
	}

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

	if err := waitReady(ctx, svc.Port); err != nil {
		if cleanupErr := p.stopUnreadySandbox(svc.ID); cleanupErr != nil {
			return errors.Join(err, cleanupErr)
		}
		return err
	}

	svc.State = "READY"
	return nil
}

func (p *Provider) stopUnreadySandbox(id string) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := p.sandbox.Stop(cleanupCtx, id); err != nil && !errors.Is(err, sandbox.ErrSandboxNotFound) {
		return fmt.Errorf("stop unready postgres sandbox: %w", err)
	}
	return nil
}

func waitReady(ctx context.Context, port int) error {
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := probePostgres(ctx, port); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("postgres service failed to become ready on port %d", port)
		case <-tick.C:
		}
	}
}

// probePostgres performs a PostgreSQL v3 startup exchange and succeeds only
// after the server reports AuthenticationOk and ReadyForQuery. A listening
// TCP socket is not enough to prove that a restored managed database is ready.
func probePostgres(ctx context.Context, port int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stopCancel()
	deadline := time.Now().Add(time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	const user, database = "postgres", "postgres"
	packet := make([]byte, 4)
	packet = binary.BigEndian.AppendUint32(packet, 196608) // protocol 3.0
	packet = append(packet, "user"...)
	packet = append(packet, 0)
	packet = append(packet, user...)
	packet = append(packet, 0)
	packet = append(packet, "database"...)
	packet = append(packet, 0)
	packet = append(packet, database...)
	packet = append(packet, 0, 0)
	binary.BigEndian.PutUint32(packet[:4], uint32(len(packet)))
	if _, err := conn.Write(packet); err != nil {
		return err
	}

	authenticated := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var header [5]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return err
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length < 4 || length > 1<<20 {
			return errors.New("invalid postgres readiness response")
		}
		body := make([]byte, int(length)-4)
		if _, err := io.ReadFull(conn, body); err != nil {
			return err
		}
		switch header[0] {
		case 'R': // Authentication request
			if len(body) != 4 {
				return errors.New("postgres requested unsupported authentication")
			}
			if binary.BigEndian.Uint32(body) == 0 {
				authenticated = true
			}
		case 'E':
			return errors.New("postgres rejected readiness probe")
		case 'Z': // ReadyForQuery
			if authenticated && len(body) == 1 {
				return nil
			}
			return errors.New("postgres did not complete readiness authentication")
		}
	}
}

// Stop stops the running PostgreSQL service.
func (p *Provider) Stop(ctx context.Context, id string) error {
	err := p.sandbox.Stop(ctx, id)
	if errors.Is(err, sandbox.ErrSandboxNotFound) {
		return nil
	}
	return err
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
