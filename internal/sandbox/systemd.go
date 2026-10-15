package sandbox

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"paas/internal/config"
	"paas/internal/contracts"
)

var _ contracts.Sandbox = (*SystemdSandbox)(nil)
var _ contracts.SandboxBatchInspector = (*SystemdSandbox)(nil)

const systemdInspectBatchSize = 32
const maxCompletedUnitHistory = 64

type cachedUnit struct {
	id   string
	info *contracts.SandboxInfo
}

// SystemdSandbox supervises Bubblewrap processes in transient cgroup v2 units.
// Terminal snapshots survive Stop so callers can classify startup failures.
type SystemdSandbox struct {
	bwrapPath    string
	mu           sync.Mutex
	units        map[string]*contracts.SandboxInfo
	completed    []cachedUnit
	run          func(context.Context, string, ...string) ([]byte, error)
	verifyLimits func(*contracts.SandboxInfo, contracts.SandboxSpec) error
}

func NewSystemd() (*SystemdSandbox, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("systemd sandbox requires Linux")
	}
	for _, tool := range []string{"systemd-run", "systemctl", "journalctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("%s is not installed: %w", tool, err)
		}
	}
	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("bubblewrap is not installed: %w", err)
	}
	controllers, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return nil, fmt.Errorf("cgroup v2 is required: %w", err)
	}
	available := " " + strings.Join(strings.Fields(string(controllers)), " ") + " "
	for _, controller := range []string{"cpu", "memory", "pids"} {
		if !strings.Contains(available, " "+controller+" ") {
			return nil, fmt.Errorf("cgroup v2 controller %s is unavailable", controller)
		}
	}
	s := &SystemdSandbox{bwrapPath: bwrapPath, units: make(map[string]*contracts.SandboxInfo), run: runSystemdCommand, verifyLimits: verifyCgroupLimits}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := s.run(ctx, "systemctl", append(managerArgs(), "show", "--property=Version")...); err != nil {
		return nil, fmt.Errorf("connect to systemd manager: %w", err)
	}
	return s, nil
}

func (*SystemdSandbox) Kind() string { return "systemd" }

func managerArgs() []string {
	if os.Geteuid() != 0 {
		return []string{"--user"}
	}
	return nil
}

// SystemdUnitName validates IDs before passing their derived names to systemctl.
func SystemdUnitName(id string) (string, error) {
	if id == "" || len(id) > 200 {
		return "", errors.New("sandbox ID must contain 1 to 200 letters, digits, underscores, or hyphens")
	}
	for _, c := range id {
		if c != '_' && c != '-' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return "", fmt.Errorf("invalid sandbox ID %q", id)
		}
	}
	if strings.HasPrefix(id, "svc_") || strings.HasPrefix(id, "service_") {
		return "paas-" + id + ".service", nil
	}
	return "paas-deploy-" + id + ".service", nil
}

func (s *SystemdSandbox) unitName(id string) string {
	name, _ := SystemdUnitName(id)
	return name
}

func systemdRunArgs(spec contracts.SandboxSpec, bwrapPath, runtimeUser string) ([]string, error) {
	unit, err := SystemdUnitName(spec.ID)
	if err != nil {
		return nil, err
	}
	bwrapArgs, err := BuildBwrapArgs(spec)
	if err != nil {
		return nil, err
	}
	memory := spec.MemoryMax
	if memory == "" {
		memory = config.DefaultMemory
	}
	memBytes, err := config.ParseMemoryBytes(memory)
	if err != nil {
		return nil, err
	}
	cpu := spec.CPUQuota
	if cpu == 0 {
		cpu = config.DefaultCPU
	}
	if cpu <= 0 || math.IsNaN(cpu) || math.IsInf(cpu, 0) || math.IsInf(cpu*100, 0) {
		return nil, errors.New("CPU quota must be finite and positive")
	}
	tasks := spec.TasksMax
	if tasks == 0 {
		tasks = config.DefaultTasks
	}
	if tasks < 1 {
		return nil, errors.New("tasks limit must be positive")
	}
	args := append(managerArgs(), "--no-ask-password", "--unit="+unit, "--service-type=exec",
		"-p", "RemainAfterExit=yes", "-p", "StandardOutput=journal", "-p", "StandardError=journal",
		"-p", "NoNewPrivileges=yes", "-p", "KillMode=control-group", "-p", "TimeoutStopSec=2s",
		"-p", "OOMPolicy=kill", "-p", "MemoryAccounting=yes", "-p", "CPUAccounting=yes", "-p", "TasksAccounting=yes",
		"-p", fmt.Sprintf("MemoryMax=%d", memBytes), "-p", "MemorySwapMax=0",
		"-p", "CPUQuota="+strconv.FormatFloat(cpu*100, 'f', -1, 64)+"%", "-p", fmt.Sprintf("TasksMax=%d", tasks))
	if runtimeUser != "" {
		args = append(args, "--uid="+runtimeUser)
	}
	args = append(args, "--", bwrapPath)
	// systemd expands $ in service arguments. Escape it without requiring the
	// --expand-environment switch introduced in systemd 254 (Ubuntu 22.04 uses 249).
	for _, arg := range bwrapArgs {
		args = append(args, strings.ReplaceAll(arg, "$", "$$"))
	}
	return args, nil
}

func runtimeIdentity(spec contracts.SandboxSpec) (string, error) {
	if os.Geteuid() != 0 {
		return "", nil
	}
	name := spec.User
	if name == "" {
		name = "paas-runtime"
	}
	target, err := user.Lookup(name)
	if err != nil {
		return "", fmt.Errorf("look up runtime user %q: %w", name, err)
	}
	uid, err := strconv.ParseUint(target.Uid, 10, 32)
	if err != nil || uid == 0 {
		return "", fmt.Errorf("runtime user %q must have a valid non-root UID", name)
	}
	return target.Uid, nil
}

func (s *SystemdSandbox) Start(ctx context.Context, spec contracts.SandboxSpec) (*contracts.SandboxHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity, err := runtimeIdentity(spec)
	if err != nil {
		return nil, err
	}
	args, err := systemdRunArgs(spec, s.bwrapPath, identity)
	if err != nil {
		return nil, err
	}
	unit, _ := SystemdUnitName(spec.ID) // validated by systemdRunArgs
	s.mu.Lock()
	defer s.mu.Unlock()
	// Never reset or stop a live unit just because a duplicate Start arrived.
	previous, err := s.inspectUnit(ctx, spec.ID)
	if err != nil && !errors.Is(err, ErrSandboxNotFound) {
		return nil, err
	}
	if err == nil {
		if previous.Running || previous.Transitioning {
			return nil, fmt.Errorf("sandbox %q is already active", spec.ID)
		}
		if err := s.stopUnit(ctx, spec.ID); err != nil {
			return nil, err
		}
	}
	delete(s.units, spec.ID)
	startCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := s.run(startCtx, "systemd-run", args...); err != nil {
		// A canceled client may already have submitted the service to the manager.
		// Capture its result before cleanup, retaining OOM evidence for Deploy.
		return nil, s.cleanupStart(spec.ID, err)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := s.inspectUnit(startCtx, spec.ID)
		if err != nil {
			return nil, s.cleanupStart(spec.ID, err)
		}
		if info.Running {
			if err := s.verifyLimits(info, spec); err != nil {
				// A short-lived process can exit after show reports it running.
				// systemd then removes its cgroup, so missing limit files are
				// evidence of that exit rather than an enforcement failure.
				current, inspectErr := s.inspectUnit(startCtx, spec.ID)
				if inspectErr == nil && !current.Running && !current.Transitioning {
					s.rememberCompletedUnit(spec.ID, current)
					err = systemdStartupExit(unit, current)
				} else {
					err = errors.Join(err, inspectErr)
				}
				return nil, s.cleanupStart(spec.ID, err)
			}
			return &contracts.SandboxHandle{ID: spec.ID, PID: info.PID}, nil
		}
		if info.ExitCode != nil || info.Result != "" && info.Result != "success" {
			s.rememberCompletedUnit(spec.ID, info)
			return nil, s.cleanupStart(spec.ID, systemdStartupExit(unit, info))
		}
		select {
		case <-startCtx.Done():
			return nil, s.cleanupStart(spec.ID, fmt.Errorf("wait for systemd unit %s: %w", unit, startCtx.Err()))
		case <-ticker.C:
		}
	}
}

func systemdStartupExit(unit string, info *contracts.SandboxInfo) error {
	if info.ExitCode != nil {
		return fmt.Errorf("systemd unit %s exited during startup (exit code %d; result=%s); check runtime logs", unit, *info.ExitCode, info.Result)
	}
	return fmt.Errorf("systemd unit %s exited during startup: result=%s; check runtime logs", unit, info.Result)
}

// Caller holds mu. Cleanup uses a fresh bounded context even if Start was canceled.
func (s *SystemdSandbox) cleanupStart(id string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	return errors.Join(cause, s.stopUnit(ctx, id))
}

func (s *SystemdSandbox) Stop(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopUnit(ctx, id)
}

func (s *SystemdSandbox) stopUnit(ctx context.Context, id string) error {
	unit, err := SystemdUnitName(id)
	if err != nil {
		return err
	}
	before, err := s.inspectUnit(ctx, id)
	if errors.Is(err, ErrSandboxNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := s.run(ctx, "systemctl", append(managerArgs(), "--no-ask-password", "stop", unit)...); err != nil {
		// Another cleanup may unload the unit between show and stop.
		if _, inspectErr := s.inspectUnit(ctx, id); !errors.Is(inspectErr, ErrSandboxNotFound) {
			return errors.Join(err, inspectErr)
		}
		before.Running = false
		before.Transitioning = false
		s.rememberCompletedUnit(id, before)
		return nil
	}
	after, err := s.inspectUnit(ctx, id)
	if err != nil && !errors.Is(err, ErrSandboxNotFound) {
		return err
	}
	if err == nil && after.Running {
		return fmt.Errorf("systemd unit %s is still running after stop", unit)
	}
	before.Running = false
	before.Transitioning = false
	s.rememberCompletedUnit(id, before)
	// Successful transient units may already have been unloaded; reset only failed units.
	if after != nil && after.Result != "" && after.Result != "success" {
		if _, err := s.run(ctx, "systemctl", append(managerArgs(), "reset-failed", unit)...); err != nil {
			return err
		}
	}
	return nil
}

// rememberCompletedUnit keeps a bounded FIFO of terminal snapshots so Inspect
// can classify recent exits without retaining every deployment forever.
// Caller holds s.mu.
func (s *SystemdSandbox) rememberCompletedUnit(id string, info *contracts.SandboxInfo) {
	s.units[id] = info
	s.completed = append(s.completed, cachedUnit{id: id, info: info})
	if len(s.completed) > maxCompletedUnitHistory {
		oldest := s.completed[0]
		s.completed = s.completed[1:]
		if s.units[oldest.id] == oldest.info {
			delete(s.units, oldest.id)
		}
	}
}

// StopSystemdDeployment reclaims a previous daemon's unit, even when the new
// daemon uses a different backend or Bubblewrap has since been uninstalled.
func StopSystemdDeployment(ctx context.Context, id string) error {
	s := &SystemdSandbox{units: make(map[string]*contracts.SandboxInfo), run: runSystemdCommand}
	return s.Stop(ctx, id)
}

func (s *SystemdSandbox) Inspect(ctx context.Context, id string) (*contracts.SandboxInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if info := s.units[id]; info != nil {
		return cloneSandboxInfo(info), nil
	}
	return s.inspectUnit(ctx, id)
}

// InspectMany reads a bounded number of unit snapshots per systemctl process.
// Result positions match ids; malformed, omitted, and failed records carry an
// error so the supervisor never interprets partial output as a runtime exit.
func (s *SystemdSandbox) InspectMany(ctx context.Context, ids []string) []contracts.SandboxInspection {
	results := make([]contracts.SandboxInspection, len(ids))
	if len(ids) == 0 {
		return results
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		for i := range results {
			results[i].Err = err
		}
		return results
	}

	type pendingUnit struct {
		indices []int
		id      string
		unit    string
	}
	pending := make([]pendingUnit, 0, len(ids))
	pendingByID := make(map[string]int, len(ids))
	for i, id := range ids {
		unit, err := SystemdUnitName(id)
		if err != nil {
			results[i].Err = err
			continue
		}
		if cached := s.units[id]; cached != nil {
			results[i].Info = cloneSandboxInfo(cached)
			continue
		}
		if pendingIndex, ok := pendingByID[id]; ok {
			pending[pendingIndex].indices = append(pending[pendingIndex].indices, i)
			continue
		}
		pendingByID[id] = len(pending)
		pending = append(pending, pendingUnit{indices: []int{i}, id: id, unit: unit})
	}

	for start := 0; start < len(pending); start += systemdInspectBatchSize {
		end := start + systemdInspectBatchSize
		if end > len(pending) {
			end = len(pending)
		}
		batch := pending[start:end]
		if err := ctx.Err(); err != nil {
			for _, item := range batch {
				for _, index := range item.indices {
					results[index].Err = err
				}
			}
			continue
		}
		args := append(managerArgs(), "show")
		for _, item := range batch {
			args = append(args, item.unit)
		}
		for _, property := range []string{"Id", "LoadState", "ActiveState", "SubState", "MainPID", "ExecMainCode", "ExecMainStatus", "Result", "MemoryCurrent", "TasksCurrent", "ControlGroup"} {
			args = append(args, "-p", property)
		}
		out, err := s.run(ctx, "systemctl", args...)
		if err != nil {
			for _, item := range batch {
				for _, index := range item.indices {
					results[index].Err = fmt.Errorf("inspect systemd units: %w", err)
				}
			}
			continue
		}
		propertiesByUnit := parseSystemdShowBlocks(string(out))
		for _, item := range batch {
			block, ok := propertiesByUnit[item.unit]
			if !ok {
				for _, index := range item.indices {
					results[index].Err = fmt.Errorf("systemctl omitted properties for unit %s", item.unit)
				}
				continue
			}
			info, parseErr := parseSystemdBatchInfo(item.id, item.unit, block)
			for _, index := range item.indices {
				results[index] = contracts.SandboxInspection{Info: cloneSandboxInfo(info), Err: parseErr}
			}
		}
	}
	return results
}

func parseSystemdBatchInfo(id, unit, block string) (*contracts.SandboxInfo, error) {
	props := parseSystemdProperties(block)
	if props["LoadState"] != "not-found" {
		for _, property := range []string{"LoadState", "ActiveState", "SubState", "MainPID", "ExecMainCode", "ExecMainStatus", "Result"} {
			if _, ok := props[property]; !ok {
				return nil, fmt.Errorf("systemctl returned incomplete properties for unit %s: missing %s", unit, property)
			}
		}
	}
	return parseSystemdInfo(id, unit, block)
}

func cloneSandboxInfo(info *contracts.SandboxInfo) *contracts.SandboxInfo {
	if info == nil {
		return nil
	}
	snapshot := *info
	if info.ExitCode != nil {
		code := *info.ExitCode
		snapshot.ExitCode = &code
	}
	return &snapshot
}

// systemctl show separates multiple unit records with blank lines. The Id
// property can appear anywhere in a record, so it is only used to identify a
// completed block after its separator has been seen.
func parseSystemdShowBlocks(output string) map[string]string {
	blocks := make(map[string]string)
	var current strings.Builder
	flush := func() {
		block := strings.TrimSpace(current.String())
		current.Reset()
		if block == "" {
			return
		}
		unit := parseSystemdProperties(block)["Id"]
		if unit != "" {
			if _, duplicate := blocks[unit]; duplicate {
				blocks[unit] = "" // mark duplicate records as malformed
			} else {
				blocks[unit] = block
			}
		}
	}
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			flush()
			continue
		}
		current.WriteString(line)
		current.WriteByte('\n')
	}
	flush()
	return blocks
}

func (s *SystemdSandbox) inspectUnit(ctx context.Context, id string) (*contracts.SandboxInfo, error) {
	unit, err := SystemdUnitName(id)
	if err != nil {
		return nil, err
	}
	args := append(managerArgs(), "show", unit)
	for _, property := range []string{"LoadState", "ActiveState", "SubState", "MainPID", "ExecMainCode", "ExecMainStatus", "Result", "MemoryCurrent", "TasksCurrent", "ControlGroup"} {
		args = append(args, "-p", property)
	}
	out, err := s.run(ctx, "systemctl", args...)
	if parseSystemdProperties(string(out))["LoadState"] == "not-found" {
		return nil, fmt.Errorf("%w: %q", ErrSandboxNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return parseSystemdInfo(id, unit, string(out))
}

func parseSystemdInfo(id, unit, output string) (*contracts.SandboxInfo, error) {
	props := parseSystemdProperties(output)
	if props["LoadState"] == "not-found" {
		return nil, fmt.Errorf("%w: %q", ErrSandboxNotFound, id)
	}
	if props["LoadState"] == "" || props["ActiveState"] == "" || props["SubState"] == "" {
		return nil, errors.New("systemctl returned incomplete unit properties")
	}
	pid, err := strconv.Atoi(props["MainPID"])
	if err != nil || pid < 0 {
		return nil, fmt.Errorf("invalid systemd MainPID %q", props["MainPID"])
	}
	info := &contracts.SandboxInfo{ID: id, PID: pid, SystemdUnit: unit, Result: props["Result"], OOMKilled: props["Result"] == "oom-kill", ControlGroup: props["ControlGroup"]}
	// RemainAfterExit leaves cleanly exited units active with SubState=exited.
	info.Transitioning = props["ActiveState"] == "activating" || props["ActiveState"] == "deactivating"
	info.Running = props["ActiveState"] == "active" && props["SubState"] == "running" && pid > 0
	if !info.Running && props["ExecMainCode"] != "0" && props["ExecMainCode"] != "" {
		status, err := strconv.Atoi(props["ExecMainStatus"])
		if err != nil {
			return nil, fmt.Errorf("invalid systemd exit status %q", props["ExecMainStatus"])
		}
		switch props["ExecMainCode"] {
		case "1": // CLD_EXITED
		case "2", "3":
			status += 128 // CLD_KILLED / CLD_DUMPED
		default:
			return nil, fmt.Errorf("invalid systemd exit code %q", props["ExecMainCode"])
		}
		info.ExitCode = &status
	}
	if value := props["MemoryCurrent"]; value != "" && value != "[not set]" && value != "18446744073709551615" {
		info.MemoryCurrent, err = strconv.ParseInt(value, 10, 64)
		if err != nil || info.MemoryCurrent < 0 {
			return nil, fmt.Errorf("invalid systemd MemoryCurrent %q", value)
		}
	}

	if value := props["TasksCurrent"]; value != "" && value != "[not set]" && value != "18446744073709551615" {
		info.TasksCurrent, err = strconv.Atoi(value)
		if err != nil || info.TasksCurrent < 0 {
			return nil, fmt.Errorf("invalid systemd TasksCurrent %q", value)
		}
	}
	return info, nil
}

func parseSystemdProperties(output string) map[string]string {
	props := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			props[key] = value
		}
	}
	return props
}

func verifyCgroupLimits(info *contracts.SandboxInfo, spec contracts.SandboxSpec) error {
	group := info.ControlGroup
	if group == "" || !filepath.IsAbs(group) || filepath.Clean(group) != group || group == "/" {
		return fmt.Errorf("invalid runtime cgroup %q", group)
	}
	root := filepath.Join("/sys/fs/cgroup", group)
	memory := spec.MemoryMax
	if memory == "" {
		memory = config.DefaultMemory
	}
	memoryBytes, err := config.ParseMemoryBytes(memory)
	if err != nil {
		return err
	}
	tasks := spec.TasksMax
	if tasks == 0 {
		tasks = config.DefaultTasks
	}
	for _, limit := range []struct {
		file    string
		maximum int64
	}{{"memory.max", memoryBytes}, {"memory.swap.max", 0}, {"pids.max", int64(tasks)}} {
		data, err := os.ReadFile(filepath.Join(root, limit.file))
		if err != nil {
			return fmt.Errorf("verify %s enforcement: %w", limit.file, err)
		}
		value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil || value < 0 || value > limit.maximum {
			return fmt.Errorf("%s limit is not enforced: %q", limit.file, strings.TrimSpace(string(data)))
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "cpu.max"))
	if err != nil {
		return fmt.Errorf("verify CPU enforcement: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || fields[0] == "max" {
		return fmt.Errorf("CPU quota is not enforced: %q", string(data))
	}
	quota, qerr := strconv.ParseFloat(fields[0], 64)
	period, perr := strconv.ParseFloat(fields[1], 64)
	cpu := spec.CPUQuota
	if cpu == 0 {
		cpu = config.DefaultCPU
	}
	// The kernel rounds quotas to microseconds; systemd enforces a 1ms minimum.
	if qerr != nil || perr != nil || quota <= 0 || period <= 0 || quota/period > cpu+1000/period {
		return fmt.Errorf("unexpected CPU quota: %q", string(data))
	}
	return nil
}

func runSystemdCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	output := &tailBuffer{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	out := output.data
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), errors.Join(err, ctx.Err()), strings.TrimSpace(string(out)))
	}
	return out, nil
}
