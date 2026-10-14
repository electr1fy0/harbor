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

func TestSystemdInspectManyBatchesUnitsAndKeepsResultsAligned(t *testing.T) {
	var calls int
	var requested [][]string
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if name != "systemctl" || !slices.Contains(args, "show") {
			return nil, errors.New("unexpected systemd command")
		}
		calls++
		showAt := slices.Index(args, "show")
		var units []string
		for _, arg := range args[showAt+1:] {
			if arg == "-p" {
				break
			}
			units = append(units, arg)
		}
		requested = append(requested, units)
		blocks := make([]string, 0, len(units))
		for _, unit := range units {
			block := runningUnit
			if strings.Contains(unit, "alpha") {
				block = strings.Replace(block, "ActiveState=active\n", "ActiveState=active\nId="+unit+"\n", 1)
			} else {
				block += "Id=" + unit + "\n"
			}
			blocks = append(blocks, block)
		}
		return []byte(strings.Join(blocks, "\n")), nil
	}
	sb := &SystemdSandbox{units: make(map[string]*contracts.SandboxInfo), run: runner}
	results := sb.InspectMany(context.Background(), []string{"alpha", "beta"})
	if calls != 1 || len(requested) != 1 || !reflect.DeepEqual(requested[0], []string{"paas-deploy-alpha.service", "paas-deploy-beta.service"}) {
		t.Fatalf("show calls=%d requested=%v; want one call for both units", calls, requested)
	}
	if len(results) != 2 {
		t.Fatalf("results length=%d, want 2", len(results))
	}
	for i, result := range results {
		if result.Err != nil || result.Info == nil || !result.Info.Running || result.Info.ID != []string{"alpha", "beta"}[i] {
			t.Fatalf("result %d = %+v, want running matching unit", i, result)
		}
	}
}

func TestSystemdInspectManyBoundsBatchesAndRejectsPartialOutput(t *testing.T) {
	ids := make([]string, systemdInspectBatchSize+1)
	for i := range ids {
		ids[i] = "app" + strings.Repeat("a", i+1)
	}
	calls := 0
	runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls++
		showAt := slices.Index(args, "show")
		var units []string
		for _, arg := range args[showAt+1:] {
			if arg == "-p" {
				break
			}
			units = append(units, arg)
		}
		blocks := make([]string, 0, len(units))
		for _, unit := range units {
			blocks = append(blocks, "Id="+unit+"\n"+runningUnit)
		}
		return []byte(strings.Join(blocks, "\n")), nil
	}
	sb := &SystemdSandbox{units: make(map[string]*contracts.SandboxInfo), run: runner}
	results := sb.InspectMany(context.Background(), ids)
	if calls != 2 || len(results) != len(ids) {
		t.Fatalf("calls=%d results=%d; want 2 batches and %d results", calls, len(results), len(ids))
	}
	for i, result := range results {
		if result.Err != nil || result.Info == nil || result.Info.ID != ids[i] {
			t.Fatalf("result %d = %+v, want matching runtime", i, result)
		}
	}

	alphaUnit, _ := SystemdUnitName("alpha")
	sb.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("Id=" + alphaUnit + "\n" + runningUnit), nil
	}
	partial := sb.InspectMany(context.Background(), []string{"alpha", "beta"})
	if partial[0].Err != nil || partial[0].Info == nil || partial[1].Err == nil || partial[1].Info != nil {
		t.Fatalf("partial batch was not classified per unit: %+v", partial)
	}
}

func TestSystemdInspectManyClassifiesMissingOOMAndClonesCachedExit(t *testing.T) {
	unitFor := func(id string) string {
		unit, err := SystemdUnitName(id)
		if err != nil {
			t.Fatal(err)
		}
		return unit
	}
	blocks := []string{
		"Id=" + unitFor("healthy") + "\n" + runningUnit,
		"Id=" + unitFor("gone") + "\n" + missingUnit,
		"Id=" + unitFor("oom") + "\n" + oomUnit,
	}
	sb := &SystemdSandbox{
		units: make(map[string]*contracts.SandboxInfo),
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte(strings.Join(blocks, "\n")), nil
		},
	}
	results := sb.InspectMany(context.Background(), []string{"healthy", "gone", "oom"})
	if len(results) != 3 || results[0].Err != nil || results[0].Info == nil || !results[0].Info.Running {
		t.Fatalf("healthy result = %+v", results)
	}
	if !errors.Is(results[1].Err, ErrSandboxNotFound) || results[1].Info != nil {
		t.Fatalf("missing result = %+v, want not found", results[1])
	}
	if results[2].Err != nil || results[2].Info == nil || !results[2].Info.OOMKilled || results[2].Info.ExitCode == nil || *results[2].Info.ExitCode != 137 {
		t.Fatalf("OOM result = %+v", results[2])
	}

	exitCode := 137
	sb.units["cached"] = &contracts.SandboxInfo{ID: "cached", ExitCode: &exitCode}
	cached := sb.InspectMany(context.Background(), []string{"cached"})
	if cached[0].Err != nil || cached[0].Info == nil || cached[0].Info.ExitCode == nil {
		t.Fatalf("cached result = %+v", cached[0])
	}
	*cached[0].Info.ExitCode = 1
	again := sb.InspectMany(context.Background(), []string{"cached"})
	if again[0].Info == nil || again[0].Info.ExitCode == nil || *again[0].Info.ExitCode != 137 {
		t.Fatalf("cached exit snapshot was mutable: %+v", again[0])
	}
}

func TestSystemdInspectManyRejectsIncompleteLifecycleProperties(t *testing.T) {
	unit, _ := SystemdUnitName("truncated")
	sb := &SystemdSandbox{
		units: make(map[string]*contracts.SandboxInfo),
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("Id=" + unit + "\nLoadState=loaded\nActiveState=failed\nSubState=failed\nMainPID=0\nResult=success\n"), nil
		},
	}
	result := sb.InspectMany(context.Background(), []string{"truncated"})[0]
	if result.Err == nil || result.Info != nil || !strings.Contains(result.Err.Error(), "ExecMainCode") {
		t.Fatalf("truncated lifecycle record = %+v; want an inspection error", result)
	}
}

func TestSystemdInspectManyCommandFailureAndCanceledContextAreSafe(t *testing.T) {
	commandErr := errors.New("systemd manager unavailable")
	calls := 0
	sb := &SystemdSandbox{units: make(map[string]*contracts.SandboxInfo), run: func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return []byte("Id=paas-deploy-alpha.service\n" + runningUnit), commandErr
	}}
	failed := sb.InspectMany(context.Background(), []string{"alpha", "beta"})
	if calls != 1 || len(failed) != 2 {
		t.Fatalf("calls=%d results=%d; want one failed batch", calls, len(failed))
	}
	for _, result := range failed {
		if !errors.Is(result.Err, commandErr) || result.Info != nil {
			t.Fatalf("command failure produced unsafe result: %+v", result)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := sb.InspectMany(ctx, []string{"alpha", "beta"})
	if calls != 1 || len(canceled) != 2 {
		t.Fatalf("canceled batch called systemctl: calls=%d results=%d", calls, len(canceled))
	}
	for _, result := range canceled {
		if !errors.Is(result.Err, context.Canceled) || result.Info != nil {
			t.Fatalf("canceled batch produced unsafe result: %+v", result)
		}
	}
}

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
