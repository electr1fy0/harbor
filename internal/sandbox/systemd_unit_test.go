package sandbox

import (
	"context"
	"errors"
	"math"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"paas/internal/contracts"
)

const runningUnit = "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=42\nExecMainCode=0\nExecMainStatus=0\nResult=success\nControlGroup=/test.slice/runtime.service\n"
const missingUnit = "LoadState=not-found\nActiveState=inactive\nSubState=dead\nMainPID=0\n"
const oomUnit = "LoadState=loaded\nActiveState=failed\nSubState=failed\nMainPID=0\nExecMainCode=2\nExecMainStatus=9\nResult=oom-kill\n"

func systemdTestSpec(t *testing.T) contracts.SandboxSpec {
	t.Helper()
	return contracts.SandboxSpec{ID: "test", Dir: t.TempDir(), Command: []string{"/app/server"}, MemoryMax: "64Mi", CPUQuota: 0.005, TasksMax: 32}
}

func TestSystemdArgumentsEnforceIdentityLimitsAndLiteralValues(t *testing.T) {
	spec := systemdTestSpec(t)
	spec.Env = []string{"VALUE=${HOME} $$ literal", "PORT=40123"}
	args, err := systemdRunArgs(spec, "/usr/bin/bwrap", "1234")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--uid=1234", "--service-type=exec", "MemoryMax=67108864", "MemorySwapMax=0", "CPUQuota=0.5%", "TasksMax=32", "NoNewPrivileges=yes", "OOMPolicy=kill", "KillMode=control-group", "$${HOME} $$$$ literal"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %q in %v", want, args)
		}
	}
	args, err = systemdRunArgs(spec, "/usr/bin/bwrap", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--uid=") {
			t.Fatalf("user manager unexpectedly changes identity: %v", args)
		}
	}
}

func TestSystemdArgumentsRejectInvalidResourcesAndIDs(t *testing.T) {
	cases := map[string]func(*contracts.SandboxSpec){
		"bad ID":          func(s *contracts.SandboxSpec) { s.ID = "../../other" },
		"too long ID":     func(s *contracts.SandboxSpec) { s.ID = strings.Repeat("a", 201) },
		"invalid memory":  func(s *contracts.SandboxSpec) { s.MemoryMax = "banana" },
		"overflow memory": func(s *contracts.SandboxSpec) { s.MemoryMax = "8Ei" },
		"negative CPU":    func(s *contracts.SandboxSpec) { s.CPUQuota = -1 },
		"NaN CPU":         func(s *contracts.SandboxSpec) { s.CPUQuota = math.NaN() },
		"infinite CPU":    func(s *contracts.SandboxSpec) { s.CPUQuota = math.Inf(1) },
		"overflow CPU":    func(s *contracts.SandboxSpec) { s.CPUQuota = math.MaxFloat64 },
		"negative tasks":  func(s *contracts.SandboxSpec) { s.TasksMax = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := systemdTestSpec(t)
			mutate(&spec)
			if _, err := systemdRunArgs(spec, "/usr/bin/bwrap", ""); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
}

func TestSystemdInspectRecognizesExitedUnitsAndSignals(t *testing.T) {
	tests := []struct {
		name, output string
		running, oom bool
		exit         *int
		wantErr      bool
	}{
		{name: "running", output: runningUnit, running: true},
		{name: "clean exit retained as active", output: "LoadState=loaded\nActiveState=active\nSubState=exited\nMainPID=0\nExecMainCode=1\nExecMainStatus=0\nResult=success\n", exit: intPtr(0)},
		{name: "OOM kill", output: oomUnit, oom: true, exit: intPtr(137)},
		{name: "activating is not an exit", output: "LoadState=loaded\nActiveState=activating\nSubState=start\nMainPID=0\nExecMainCode=0\nExecMainStatus=0\nResult=success\n"},
		{name: "missing", output: missingUnit, wantErr: true},
		{name: "incomplete", output: "", wantErr: true},
		{name: "invalid pid", output: strings.Replace(runningUnit, "MainPID=42", "MainPID=bad", 1), wantErr: true},
		{name: "unavailable metrics", output: runningUnit + "MemoryCurrent=18446744073709551615\nTasksCurrent=[not set]\n", running: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := parseSystemdInfo("test", "paas-deploy-test.service", tt.output)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if info.Running != tt.running || info.OOMKilled != tt.oom || !reflect.DeepEqual(info.ExitCode, tt.exit) {
				t.Fatalf("unexpected info: %+v", info)
			}
		})
	}
}
func intPtr(value int) *int { return &value }

type systemdRunner struct {
	state             string
	startErr, stopErr error
	starts, stops     int
	cancelStart       bool
}

func (r *systemdRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "systemd-run" {
		r.starts++
		r.state = runningUnit
		if r.startErr != nil {
			r.state = oomUnit
			return nil, r.startErr
		}
		if r.cancelStart {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, nil
	}
	if slices.Contains(args, "show") {
		if r.state == "" {
			return []byte(missingUnit), errors.New("unit does not exist")
		}
		return []byte(r.state), nil
	}
	if slices.Contains(args, "stop") {
		r.stops++
		if r.stopErr != nil {
			return nil, r.stopErr
		}
		r.state = ""
	}
	return nil, nil
}
func testSystemd(r *systemdRunner) *SystemdSandbox {
	return &SystemdSandbox{bwrapPath: "/usr/bin/bwrap", units: make(map[string]*contracts.SandboxInfo), run: r.run, verifyLimits: func(*contracts.SandboxInfo, contracts.SandboxSpec) error { return nil }}
}

func TestSystemdStartStopRetryAndDuplicateOwnership(t *testing.T) {
	spec := systemdTestSpec(t)
	runner := &systemdRunner{}
	sb := testSystemd(runner)
	ctx := context.Background()
	handle, err := sb.Start(ctx, spec)
	if err != nil || handle.PID != 42 {
		t.Fatalf("Start=%+v %v", handle, err)
	}
	if _, err := sb.Start(ctx, spec); err == nil {
		t.Fatal("duplicate Start succeeded")
	}
	if runner.starts != 1 || runner.stops != 0 {
		t.Fatal("duplicate Start modified the live unit")
	}
	if err := sb.Stop(ctx, spec.ID); err != nil {
		t.Fatal(err)
	}
	info, err := sb.Inspect(ctx, spec.ID)
	if err != nil || info.Running {
		t.Fatalf("after Stop=%+v %v", info, err)
	}
	if _, err := sb.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if runner.starts != 2 {
		t.Fatalf("starts=%d", runner.starts)
	}
}

func TestSystemdStartupFailurePreservesOOMAfterCleanup(t *testing.T) {
	spec := systemdTestSpec(t)
	runner := &systemdRunner{startErr: errors.New("startup failed")}
	sb := testSystemd(runner)
	if _, err := sb.Start(context.Background(), spec); !errors.Is(err, runner.startErr) {
		t.Fatalf("Start error=%v", err)
	}
	info, err := sb.Inspect(context.Background(), spec.ID)
	if err != nil || !info.OOMKilled || info.Running || runner.stops != 1 {
		t.Fatalf("info=%+v err=%v stops=%d", info, err, runner.stops)
	}
	*info.ExitCode = 0
	again, err := sb.Inspect(context.Background(), spec.ID)
	if err != nil || *again.ExitCode != 137 {
		t.Fatal("caller modified cached exit status")
	}
}

func TestSystemdStopPropagatesFailureAndRetainsOwnership(t *testing.T) {
	runner := &systemdRunner{state: runningUnit, stopErr: errors.New("permission denied")}
	sb := testSystemd(runner)
	if err := sb.Stop(context.Background(), "test"); !errors.Is(err, runner.stopErr) {
		t.Fatalf("Stop error=%v", err)
	}
	info, err := sb.Inspect(context.Background(), "test")
	if err != nil || !info.Running {
		t.Fatalf("failed Stop lost running unit: %+v %v", info, err)
	}
}

func TestSystemdCanceledStartCleansUpUsingFreshContext(t *testing.T) {
	spec := systemdTestSpec(t)
	runner := &systemdRunner{cancelStart: true}
	sb := testSystemd(runner)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := sb.Start(ctx, spec); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start error=%v", err)
	}
	if runner.stops != 1 {
		t.Fatal("canceled start left its unit running")
	}
}

func TestSystemdRejectsUnenforcedLimitsAndCleansUp(t *testing.T) {
	spec := systemdTestSpec(t)
	runner := &systemdRunner{}
	sb := testSystemd(runner)
	sb.verifyLimits = func(*contracts.SandboxInfo, contracts.SandboxSpec) error {
		return errors.New("cpu controller not delegated")
	}
	if _, err := sb.Start(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "not delegated") {
		t.Fatalf("Start error=%v", err)
	}
	if runner.stops != 1 {
		t.Fatal("unenforced runtime left alive")
	}
}

func TestSystemdCommandHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err := runSystemdCommand(ctx, "/bin/sleep", "30")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(began) > time.Second {
		t.Fatalf("command ignored deadline: %v", err)
	}
}

func TestTailBufferBoundsOutputAcrossWrites(t *testing.T) {
	buf := &tailBuffer{limit: 5}
	for _, chunk := range []string{"ab", "cdef", "ghijkl", "m"} {
		if n, err := buf.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write=%d %v", n, err)
		}
		if len(buf.data) > 5 {
			t.Fatal("output exceeded bound")
		}
	}
	if string(buf.data) != "ijklm" {
		t.Fatalf("tail=%q", buf.data)
	}
}

func TestSystemdDuplicateStartDoesNotInterruptActivatingUnit(t *testing.T) {
	runner := &systemdRunner{state: "LoadState=loaded\nActiveState=activating\nSubState=start\nMainPID=0\nExecMainCode=0\nResult=success\n"}
	sb := testSystemd(runner)
	if _, err := sb.Start(context.Background(), systemdTestSpec(t)); err == nil {
		t.Fatal("duplicate start replaced an activating service")
	}
	if runner.starts != 0 || runner.stops != 0 {
		t.Fatal("activating service was modified")
	}
}

func TestDefaultSandboxDoesNotSilentlyLoseLinuxIsolation(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	sb, err := NewDefault()
	if runtime.GOOS == "linux" {
		if err == nil || sb != nil {
			t.Fatalf("Linux silently fell back: %T %v", sb, err)
		}
	} else if err != nil || sb.Kind() != "host" {
		t.Fatalf("development fallback: %T %v", sb, err)
	}
}
