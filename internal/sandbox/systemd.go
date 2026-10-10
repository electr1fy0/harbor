// Package sandbox provides execution environments for applications.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"paas/internal/contracts"
)

var _ contracts.Sandbox = (*SystemdSandbox)(nil)

// SystemdSandbox executes applications inside Bubblewrap sandboxes supervised
// by systemd transient service units and cgroups v2.
type SystemdSandbox struct {
	bwrapPath string
	mu        sync.RWMutex
	units     map[string]string // deploymentID -> unitName
}

// NewSystemd creates a new SystemdSandbox after checking systemd and bubblewrap tools.
func NewSystemd() (*SystemdSandbox, error) {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return nil, fmt.Errorf("systemd-run is not installed: %w", err)
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil, fmt.Errorf("systemctl is not installed: %w", err)
	}
	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("bubblewrap is not installed: %w", err)
	}

	return &SystemdSandbox{
		bwrapPath: bwrapPath,
		units:     make(map[string]string),
	}, nil
}

func (s *SystemdSandbox) systemdArgs() []string {
	if os.Geteuid() != 0 {
		return []string{"--user"}
	}
	return nil
}

func (s *SystemdSandbox) unitName(id string) string {
	return fmt.Sprintf("paas-deploy-%s.service", id)
}

// Start launches a transient systemd service unit wrapping Bubblewrap.
func (s *SystemdSandbox) Start(ctx context.Context, spec contracts.SandboxSpec) (*contracts.SandboxHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.ID == "" {
		return nil, errors.New("sandbox ID is required")
	}

	bwrapArgs, err := BuildBwrapArgs(spec)
	if err != nil {
		return nil, err
	}

	unit := s.unitName(spec.ID)

	// Clean up any stale unit state before starting
	_ = s.execSystemctl("reset-failed", unit)

	args := append([]string{}, s.systemdArgs()...)
	args = append(args,
		"--unit="+unit,
		"-p", "RemainAfterExit=yes",
		"-p", "StandardOutput=journal",
		"-p", "StandardError=journal",
	)

	// Resource limit: MemoryMax & MemorySwapMax
	memBytes := parseMemoryBytes(spec.MemoryMax)
	if memBytes > 0 {
		args = append(args,
			"-p", fmt.Sprintf("MemoryMax=%d", memBytes),
			"-p", "MemorySwapMax=0",
		)
	}

	// Resource limit: CPUQuota
	if spec.CPUQuota > 0 {
		percent := int(spec.CPUQuota * 100)
		if percent > 0 {
			args = append(args, "-p", fmt.Sprintf("CPUQuota=%d%%", percent))
		}
	}

	// Resource limit: TasksMax
	if spec.TasksMax > 0 {
		args = append(args, "-p", fmt.Sprintf("TasksMax=%d", spec.TasksMax))
	}

	// Pass Bubblewrap executable and arguments
	args = append(args, s.bwrapPath)
	args = append(args, bwrapArgs...)

	runCmd := exec.Command("systemd-run", args...)
	if out, err := runCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("systemd-run failed: %w: %s", err, string(out))
	}

	s.mu.Lock()
	s.units[spec.ID] = unit
	s.mu.Unlock()

	// Wait for the unit to become active or fail
	timeout := time.After(3 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = s.Stop(context.Background(), spec.ID)
			return nil, ctx.Err()
		case <-timeout:
			info, _ := s.Inspect(context.Background(), spec.ID)
			_ = s.Stop(context.Background(), spec.ID)
			return nil, fmt.Errorf("timeout waiting for systemd unit %s to start (info: %+v)", unit, info)
		case <-ticker.C:
			info, inspectErr := s.Inspect(context.Background(), spec.ID)
			if inspectErr != nil {
				continue
			}
			if info.Running && info.PID > 0 {
				return &contracts.SandboxHandle{ID: spec.ID, PID: info.PID}, nil
			}
			if !info.Running && (info.ExitCode != nil || info.Result != "" && info.Result != "success") {
				// Failed early during startup
				return nil, fmt.Errorf("systemd unit %s failed during startup: result=%s exit=%v", unit, info.Result, info.ExitCode)
			}
		}
	}
}

// Stop stops the transient systemd service unit.
func (s *SystemdSandbox) Stop(ctx context.Context, id string) error {
	unit := s.unitName(id)
	_ = s.execSystemctl("stop", unit)
	_ = s.execSystemctl("reset-failed", unit)

	s.mu.Lock()
	delete(s.units, id)
	s.mu.Unlock()
	return nil
}

// Inspect queries the current properties of the transient systemd service unit.
func (s *SystemdSandbox) Inspect(ctx context.Context, id string) (*contracts.SandboxInfo, error) {
	unit := s.unitName(id)

	args := append([]string{}, s.systemdArgs()...)
	args = append(args, "show", unit,
		"-p", "ActiveState",
		"-p", "SubState",
		"-p", "MainPID",
		"-p", "ExecMainStatus",
		"-p", "Result",
		"-p", "OOMKilled",
		"-p", "MemoryCurrent",
		"-p", "TasksCurrent",
	)

	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl show %s: %w", unit, err)
	}

	props := parseSystemdProperties(string(out))
	activeState := props["ActiveState"]
	result := props["Result"]
	oomKilled := props["OOMKilled"] == "yes" || result == "oom-kill"

	running := activeState == "active"
	mainPID, _ := strconv.Atoi(props["MainPID"])

	info := &contracts.SandboxInfo{
		ID:          id,
		PID:         mainPID,
		Running:     running,
		OOMKilled:   oomKilled,
		Result:      result,
		SystemdUnit: unit,
	}

	if statusStr, ok := props["ExecMainStatus"]; ok && statusStr != "" {
		if code, err := strconv.Atoi(statusStr); err == nil && (!running || code != 0) {
			info.ExitCode = &code
		}
	}

	if memStr, ok := props["MemoryCurrent"]; ok && memStr != "[not set]" && memStr != "" {
		if mem, err := strconv.ParseInt(memStr, 10, 64); err == nil {
			info.MemoryCurrent = mem
		}
	}

	if taskStr, ok := props["TasksCurrent"]; ok && taskStr != "[not set]" && taskStr != "" {
		if tasks, err := strconv.Atoi(taskStr); err == nil {
			info.TasksCurrent = tasks
		}
	}

	return info, nil
}

func (s *SystemdSandbox) execSystemctl(action, unit string) error {
	args := append([]string{}, s.systemdArgs()...)
	args = append(args, action, unit)
	cmd := exec.Command("systemctl", args...)
	return cmd.Run()
}

func parseSystemdProperties(output string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if ok {
			result[key] = val
		}
	}
	return result
}

func parseMemoryBytes(val string) int64 {
	val = strings.TrimSpace(val)
	if val == "" {
		return 128 * 1024 * 1024
	}

	units := []struct {
		suffix string
		mult   int64
	}{
		{"Ki", 1024},
		{"K", 1024},
		{"Mi", 1024 * 1024},
		{"M", 1024 * 1024},
		{"Gi", 1024 * 1024 * 1024},
		{"G", 1024 * 1024 * 1024},
		{"Ti", 1024 * 1024 * 1024 * 1024},
		{"T", 1024 * 1024 * 1024 * 1024},
		{"B", 1},
	}

	for _, u := range units {
		if strings.HasSuffix(val, u.suffix) {
			numStr := strings.TrimSuffix(val, u.suffix)
			if num, err := strconv.ParseInt(numStr, 10, 64); err == nil {
				return num * u.mult
			}
		}
	}

	if num, err := strconv.ParseInt(val, 10, 64); err == nil {
		return num
	}

	return 128 * 1024 * 1024
}
