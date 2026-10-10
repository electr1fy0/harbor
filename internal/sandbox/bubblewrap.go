// Package sandbox provides execution environments for applications.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"paas/internal/contracts"
)

var _ contracts.Sandbox = (*BubblewrapSandbox)(nil)

// BubblewrapSandbox executes application releases inside unprivileged Bubblewrap sandboxes.
type BubblewrapSandbox struct {
	bwrapPath string
	host      *HostSandbox
}

// NewBubblewrap creates a new BubblewrapSandbox after verifying that bwrap is installed.
func NewBubblewrap() (*BubblewrapSandbox, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("bubblewrap requires Linux")
	}
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("bubblewrap is not installed: %w", err)
	}
	return &BubblewrapSandbox{
		bwrapPath: path,
		host:      NewHost(),
	}, nil
}

// BuildBwrapArgs builds the command line arguments for bwrap given a SandboxSpec.
func BuildBwrapArgs(spec contracts.SandboxSpec) ([]string, error) {
	if spec.ID == "" {
		return nil, errors.New("sandbox ID is required")
	}
	if len(spec.Command) == 0 || spec.Command[0] == "" {
		return nil, errors.New("sandbox command is required")
	}
	if spec.Dir == "" {
		return nil, errors.New("sandbox release artifact directory is required")
	}
	artifactDir, err := filepath.Abs(spec.Dir)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact directory: %w", err)
	}
	stat, err := os.Stat(artifactDir)
	if err != nil {
		return nil, fmt.Errorf("release artifact directory %q: %w", artifactDir, err)
	}
	if !stat.IsDir() {
		return nil, fmt.Errorf("release artifact path %q is not a directory", artifactDir)
	}
	env, err := makeEnvironment(spec.Env)
	if err != nil {
		return nil, err
	}

	args := []string{
		"--die-with-parent",
		"--new-session",
		"--clearenv",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--share-net",
		"--cap-drop", "ALL",
	}

	// Mount essential host system root directories read-only.

	for _, dir := range []string{"/bin", "/usr", "/lib", "/lib64"} {
		if _, err := os.Stat(dir); err == nil {
			args = append(args, "--ro-bind", dir, dir)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect runtime directory %q: %w", dir, err)
		}
	}

	// Mount release artifact directory read-only to /app.
	args = append(args,
		"--dir", "/app",
		"--ro-bind", artifactDir, "/app",
		"--chdir", "/app",
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev", "/dev",
	)

	// Expose only runtime resolver/certificate data, not host credentials in /etc.
	for _, path := range []string{"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/ssl/certs", "/etc/ca-certificates"} {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect runtime path %q: %w", path, err)
		}
	}
	// Set environment variables.
	hasPath := false
	hasHome := false
	for _, entry := range env {
		key, val, _ := strings.Cut(entry, "=") // validated by makeEnvironment
		if key == "PATH" {
			hasPath = true
		}
		if key == "HOME" {
			hasHome = true
		}
		args = append(args, "--setenv", key, val)
	}
	if !hasPath {
		args = append(args, "--setenv", "PATH", "/usr/local/bin:/usr/bin:/bin")
	}
	if !hasHome {
		args = append(args, "--setenv", "HOME", "/tmp")
	}

	// Translate only the executable path; arguments may contain literal host paths.
	// A path in a sibling directory must never be rewritten as a release path.
	command := append([]string(nil), spec.Command...)
	if filepath.IsAbs(command[0]) {
		rel, err := filepath.Rel(artifactDir, command[0])
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			command[0] = filepath.Join("/app", rel)
		}
	}
	args = append(args, "--")
	args = append(args, command...)

	return args, nil
}

func (*BubblewrapSandbox) Kind() string { return "bubblewrap" }

// Start uses the same process ownership and termination rules as host processes.
func (b *BubblewrapSandbox) Start(ctx context.Context, spec contracts.SandboxSpec) (*contracts.SandboxHandle, error) {
	args, err := BuildBwrapArgs(spec)
	if err != nil {
		return nil, err
	}
	spec.Command = append([]string{b.bwrapPath}, args...)
	spec.Dir = "/tmp"
	// Bubblewrap receives only the explicit environment configured below.
	spec.Env = nil
	return b.host.Start(ctx, spec)
}

func (b *BubblewrapSandbox) Stop(ctx context.Context, id string) error { return b.host.Stop(ctx, id) }
func (b *BubblewrapSandbox) Inspect(ctx context.Context, id string) (*contracts.SandboxInfo, error) {
	return b.host.Inspect(ctx, id)
}
