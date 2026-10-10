// Package sandbox runs applications as host processes.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"paas/internal/contracts"
)

var ErrSandboxNotFound = errors.New("sandbox not found")

var _ contracts.Sandbox = (*HostSandbox)(nil)

// HostSandbox manages application process groups on the local host.
type HostSandbox struct {
	mu        sync.RWMutex
	processes map[string]*managedProcess
}

type managedProcess struct {
	cmd      *exec.Cmd
	pid      int
	mu       sync.RWMutex
	running  bool
	exitCode *int
	treeDone chan struct{}
	stopMu   sync.Mutex
}

// NewHost returns a host process sandbox.
func NewHost() *HostSandbox {
	return &HostSandbox{processes: make(map[string]*managedProcess)}
}

// Start launches a process group. The context is used to reject canceled
// startup requests; the running process is managed independently afterward.
func (h *HostSandbox) Start(ctx context.Context, spec contracts.SandboxSpec) (*contracts.SandboxHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.ID == "" {
		return nil, errors.New("sandbox ID is required")
	}
	if len(spec.Command) == 0 || spec.Command[0] == "" {
		return nil, errors.New("sandbox command is required")
	}
	env, err := makeEnvironment(spec.Env)
	if err != nil {
		return nil, err
	}
	command, err := commandForEnvironment(spec.Command, env)
	if err != nil {
		return nil, err
	}

	h.mu.Lock()
	if previous, ok := h.processes[spec.ID]; ok {
		select {
		case <-previous.treeDone:
			delete(h.processes, spec.ID)
		default:
			h.mu.Unlock()
			return nil, fmt.Errorf("sandbox %q is already active", spec.ID)
		}
	}
	if err := ctx.Err(); err != nil {
		h.mu.Unlock()
		return nil, err
	}

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = env

	releaseTracking, err := configureCommand(cmd, spec)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	if spec.LogPath != "" {
		logFile, openErr := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if openErr != nil {
			releaseTracking()
			h.mu.Unlock()
			return nil, fmt.Errorf("open sandbox log: %w", openErr)
		}
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		if err := cmd.Start(); err != nil {
			_ = logFile.Close()
			releaseTracking()
			h.mu.Unlock()
			return nil, fmt.Errorf("start sandbox: %w", err)
		}
		_ = logFile.Close()
	} else if err := cmd.Start(); err != nil {
		releaseTracking()
		h.mu.Unlock()
		return nil, fmt.Errorf("start sandbox: %w", err)
	}

	process := &managedProcess{
		cmd:      cmd,
		pid:      cmd.Process.Pid,
		running:  true,
		treeDone: make(chan struct{}),
	}
	h.processes[spec.ID] = process
	h.mu.Unlock()

	go reapProcess(process, releaseTracking)

	if err := ctx.Err(); err != nil {
		_ = h.stopProcess(context.Background(), process)
		select {
		case <-process.treeDone:
			h.mu.Lock()
			if h.processes[spec.ID] == process {
				delete(h.processes, spec.ID)
			}
			h.mu.Unlock()
		default:
		}
		return nil, err
	}
	return &contracts.SandboxHandle{ID: spec.ID, PID: process.pid}, nil
}

// Stop terminates a known process group and waits for its members to be reaped.
func (h *HostSandbox) Stop(ctx context.Context, id string) error {
	process, err := h.lookup(id)
	if err != nil {
		return err
	}
	return h.stopProcess(ctx, process)
}

func (h *HostSandbox) stopProcess(ctx context.Context, process *managedProcess) error {
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

func waitForGroupExit(ctx context.Context, pgid int, duration time.Duration) {
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for processGroupAlive(pgid) {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

// Inspect returns the latest known state for a sandbox.
func (h *HostSandbox) Inspect(ctx context.Context, id string) (*contracts.SandboxInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	process, err := h.lookup(id)
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

func (h *HostSandbox) lookup(id string) (*managedProcess, error) {
	h.mu.RLock()
	process, ok := h.processes[id]
	h.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrSandboxNotFound, id)
	}
	return process, nil
}

func reapProcess(process *managedProcess, releaseTracking func()) {
	_ = process.cmd.Wait()
	code := exitStatus(process.cmd.ProcessState)
	process.mu.Lock()
	process.running = false
	process.exitCode = &code
	process.mu.Unlock()
	finishProcessTree(process.pid)
	releaseTracking()
	close(process.treeDone)
}

func makeEnvironment(explicit []string) ([]string, error) {
	values := make(map[string]string, len(explicit))
	for _, entry := range explicit {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.Contains(key, "=") {
			return nil, fmt.Errorf("invalid environment entry %q", entry)
		}
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env, nil
}

func commandForEnvironment(command, env []string) ([]string, error) {
	if strings.ContainsRune(command[0], os.PathSeparator) {
		return command, nil
	}
	pathValue := ""
	for _, entry := range env {
		if key, value, ok := strings.Cut(entry, "="); ok && key == "PATH" {
			pathValue = value
			break
		}
	}
	if pathValue == "" {
		return nil, fmt.Errorf("sandbox PATH is required to resolve command %q", command[0])
	}
	for _, directory := range filepath.SplitList(pathValue) {
		candidate := directory + string(os.PathSeparator) + command[0]
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			resolved := append([]string(nil), command...)
			resolved[0] = candidate
			return resolved, nil
		}
	}
	return nil, fmt.Errorf("resolve sandbox command %q using PATH: %w", command[0], exec.ErrNotFound)
}
