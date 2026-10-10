// Package sandbox provides execution environments for applications.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"paas/internal/contracts"
)

var _ contracts.Sandbox = (*BubblewrapSandbox)(nil)

// BubblewrapSandbox executes application releases inside unprivileged Bubblewrap sandboxes.
type BubblewrapSandbox struct {
	bwrapPath string
	mu        sync.RWMutex
	processes map[string]*managedProcess
}

// NewBubblewrap creates a new BubblewrapSandbox after verifying that bwrap is installed.
func NewBubblewrap() (*BubblewrapSandbox, error) {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("bubblewrap is not installed: %w", err)
	}
	return &BubblewrapSandbox{
		bwrapPath: path,
		processes: make(map[string]*managedProcess),
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
	if _, err := os.Stat(spec.Dir); err != nil {
		return nil, fmt.Errorf("release artifact directory %q: %w", spec.Dir, err)
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
	}

	// Mount essential host system root directories read-only.
	systemDirs := []string{"/bin", "/usr", "/lib", "/etc"}
	if _, err := os.Stat("/lib64"); err == nil {
		systemDirs = append(systemDirs, "/lib64")
	}
	for _, dir := range systemDirs {
		if _, err := os.Stat(dir); err == nil {
			args = append(args, "--ro-bind", dir, dir)
		}
	}

	// Mount release artifact directory read-only to /app.
	args = append(args,
		"--dir", "/app",
		"--ro-bind", spec.Dir, "/app",
		"--tmpfs", "/tmp",
		"--proc", "/proc",
		"--dev", "/dev",
	)

	// Set environment variables.
	hasPath := false
	hasHome := false
	for _, entry := range spec.Env {
		key, val, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
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

	// Append command, translating host release artifact directory paths to /app.
	for _, cmdArg := range spec.Command {
		if spec.Dir != "" && strings.Contains(cmdArg, spec.Dir) {
			cmdArg = strings.ReplaceAll(cmdArg, spec.Dir, "/app")
		}
		args = append(args, cmdArg)
	}

	return args, nil
}

// Start launches a Bubblewrap sandbox process directly.
func (b *BubblewrapSandbox) Start(ctx context.Context, spec contracts.SandboxSpec) (*contracts.SandboxHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	args, err := BuildBwrapArgs(spec)
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	if previous, ok := b.processes[spec.ID]; ok {
		select {
		case <-previous.treeDone:
			delete(b.processes, spec.ID)
		default:
			b.mu.Unlock()
			return nil, fmt.Errorf("sandbox %q is already active", spec.ID)
		}
	}
	if err := ctx.Err(); err != nil {
		b.mu.Unlock()
		return nil, err
	}

	cmd := exec.Command(b.bwrapPath, args...)
	cmd.Dir = "/tmp"

	releaseTracking, err := configureCommand(cmd, spec)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}

	if spec.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0700); err != nil {
			releaseTracking()
			b.mu.Unlock()
			return nil, fmt.Errorf("create log dir: %w", err)
		}
		logFile, openErr := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
		if openErr != nil {
			releaseTracking()
			b.mu.Unlock()
			return nil, fmt.Errorf("open sandbox log: %w", openErr)
		}
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		if err := cmd.Start(); err != nil {
			_ = logFile.Close()
			releaseTracking()
			b.mu.Unlock()
			return nil, fmt.Errorf("start sandbox: %w", err)
		}
		_ = logFile.Close()
	} else if err := cmd.Start(); err != nil {
		releaseTracking()
		b.mu.Unlock()
		return nil, fmt.Errorf("start sandbox: %w", err)
	}

	process := &managedProcess{
		cmd:      cmd,
		pid:      cmd.Process.Pid,
		running:  true,
		treeDone: make(chan struct{}),
	}
	b.processes[spec.ID] = process
	b.mu.Unlock()

	go reapProcess(process, releaseTracking)

	if err := ctx.Err(); err != nil {
		_ = b.stopProcess(context.Background(), process)
		return nil, err
	}
	return &contracts.SandboxHandle{ID: spec.ID, PID: process.pid}, nil
}

// Stop terminates a sandbox process.
func (b *BubblewrapSandbox) Stop(ctx context.Context, id string) error {
	process, err := b.lookup(id)
	if err != nil {
		return err
	}
	return b.stopProcess(ctx, process)
}

func (b *BubblewrapSandbox) stopProcess(ctx context.Context, process *managedProcess) error {
	process.stopMu.Lock()
	defer process.stopMu.Unlock()

	select {
	case <-process.treeDone:
		return nil
	default:
	}

	if processGroupAlive(process.pid) {
		if err := signalProcessGroup(process.pid, syscall.SIGTERM); err != nil {
			return fmt.Errorf("send SIGTERM to sandbox process group: %w", err)
		}
		waitForGroupExit(ctx, process.pid, 2*time.Second)
	}
	if processGroupAlive(process.pid) {
		if err := signalProcessGroup(process.pid, syscall.SIGKILL); err != nil {
			return fmt.Errorf("send SIGKILL to sandbox process group: %w", err)
		}
	}

	select {
	case <-process.treeDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
		return fmt.Errorf("sandbox process group %d did not exit after SIGKILL", process.pid)
	}
}

// Inspect returns the status of a sandbox.
func (b *BubblewrapSandbox) Inspect(ctx context.Context, id string) (*contracts.SandboxInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	process, err := b.lookup(id)
	if err != nil {
		return nil, err
	}
	process.mu.RLock()
	defer process.mu.RUnlock()
	info := &contracts.SandboxInfo{ID: id, PID: process.pid, Running: process.running}
	if process.exitCode != nil {
		code := *process.exitCode
		info.ExitCode = &code
	}
	return info, nil
}

func (b *BubblewrapSandbox) lookup(id string) (*managedProcess, error) {
	b.mu.RLock()
	process, ok := b.processes[id]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrSandboxNotFound, id)
	}
	return process, nil
}
