package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"paas/internal/contracts"
)

func TestHostStartStopAndReuse(t *testing.T) {
	host := NewHost()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logPath := t.TempDir() + "/app.log"
	spec := testSpec("app", []string{"/bin/sh", "-c", "printf stdout; printf stderr >&2; sleep 60"})
	spec.LogPath = logPath
	handle, err := host.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if handle.ID != spec.ID || handle.PID <= 0 {
		t.Fatalf("unexpected handle: %+v", handle)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	info, err := host.Inspect(context.Background(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Running {
		t.Fatalf("process stopped when its startup context ended: %+v", info)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer stopCancel()
	if err := host.Stop(stopCtx, spec.ID); err != nil {
		t.Fatal(err)
	}
	if err := host.Stop(stopCtx, spec.ID); err != nil {
		t.Fatalf("repeated stop: %v", err)
	}
	info, err = host.Inspect(context.Background(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Running || info.ExitCode == nil {
		t.Fatalf("expected an exited process with status: %+v", info)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "stdout") || !strings.Contains(string(log), "stderr") {
		t.Fatalf("stdout/stderr were not appended to the log: %q", log)
	}

	// A deployment retry can reuse its ID after the previous group is gone.
	spec.Command = []string{"/bin/sh", "-c", "exit 0"}
	if _, err := host.Start(context.Background(), spec); err != nil {
		t.Fatalf("restart with stopped ID: %v", err)
	}
	if err := host.Stop(stopCtx, spec.ID); err != nil {
		t.Fatalf("stop restarted sandbox: %v", err)
	}
}

func TestHostCrashRecordsExitStatus(t *testing.T) {
	host := NewHost()
	spec := testSpec("crash", []string{"/bin/sh", "-c", "exit 23"})
	if _, err := host.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info, err := host.Inspect(context.Background(), spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Running {
			if info.ExitCode == nil || *info.ExitCode != 23 {
				t.Fatalf("unexpected crash status: %+v", info)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("process did not exit")
}

func TestHostBoundsCompletedProcessHistory(t *testing.T) {
	host := NewHost()
	for i := 0; i < maxCompletedProcessHistory+1; i++ {
		id := fmt.Sprintf("completed-%d", i)
		if _, err := host.Start(context.Background(), testSpec(id, []string{"/bin/sh", "-c", "exit 0"})); err != nil {
			t.Fatalf("Start(%q): %v", id, err)
		}
		process, err := host.lookup(id)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-process.treeDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("process %q did not finish", id)
		}
	}
	if _, err := host.Inspect(context.Background(), "completed-0"); !errors.Is(err, ErrSandboxNotFound) {
		t.Fatalf("old completed process remained inspectable after history bound: %v", err)
	}
	latest, err := host.Inspect(context.Background(), fmt.Sprintf("completed-%d", maxCompletedProcessHistory))
	if err != nil {
		t.Fatalf("latest completed process was evicted: %v", err)
	}
	if latest.Running || latest.ExitCode == nil || *latest.ExitCode != 0 {
		t.Fatalf("latest completed process metadata = %+v", latest)
	}
}

func TestHostStopWithCanceledContextDoesNotSignalProcess(t *testing.T) {
	host := NewHost()
	spec := testSpec("canceled-stop", []string{"/bin/sh", "-c", "sleep 60"})
	if _, err := host.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := host.Stop(ctx, spec.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop with canceled context = %v, want context.Canceled", err)
	}
	info, err := host.Inspect(context.Background(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Running {
		t.Fatalf("canceled Stop signaled the process: %+v", info)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := host.Stop(stopCtx, spec.ID); err != nil {
		t.Fatalf("cleanup Stop: %v", err)
	}
}

func TestHostCrashCleansUpAndReapsDescendants(t *testing.T) {
	host := NewHost()
	tempDir := t.TempDir()
	if err := os.Chmod(tempDir, 0o777); err != nil {
		t.Fatal(err)
	}
	pidPath := tempDir + "/child.pid"
	spec := testSpec("crash-with-child", []string{"/bin/sh", "-c", "sleep 60 & echo $! > \"$PID_FILE\"; exit 17"})
	spec.Env = append(spec.Env, "PID_FILE="+pidPath)
	handle, err := host.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := awaitPIDFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if processGone(childPID) {
			info, inspectErr := host.Inspect(context.Background(), spec.ID)
			if inspectErr != nil {
				t.Fatal(inspectErr)
			}
			if info.Running || info.ExitCode == nil || *info.ExitCode != 17 {
				t.Fatalf("unexpected state after leader crash: %+v", info)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	state, _ := exec.Command("ps", "-o", "pid=,ppid=,pgid=,stat=", "-p", strconv.Itoa(childPID)).Output()
	t.Fatalf("descendant PID %d remained alive or unreaped after leader %d exit (group alive: %v, process: %q)", childPID, handle.PID, processGroupAlive(handle.PID), strings.TrimSpace(string(state)))
}

func TestHostStartResolvesRelativePathEntriesFromWorkingDirectory(t *testing.T) {
	for _, test := range []struct {
		name   string
		path   string
		binary string
	}{
		{name: "relative path directory", path: "bin", binary: "bin"},
		{name: "empty first path component", path: ":bin", binary: "."},
		{name: "empty path", path: "", binary: "."},
	} {
		t.Run(test.name, func(t *testing.T) {
			tempDir := t.TempDir()
			if err := os.Chmod(tempDir, 0o755); err != nil {
				t.Fatal(err)
			}
			workingDirectory := filepath.Join(tempDir, "work")
			if err := os.Mkdir(workingDirectory, 0o755); err != nil {
				t.Fatal(err)
			}
			binaryDir := workingDirectory
			if test.binary != "." {
				binaryDir = filepath.Join(workingDirectory, test.binary)
				if err := os.Mkdir(binaryDir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			binaryPath := filepath.Join(binaryDir, "fixture-command")
			if err := os.WriteFile(binaryPath, []byte("#!/bin/sh\nexit 37\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			spec := testSpec("path-resolution", []string{"fixture-command"})
			spec.Dir = workingDirectory
			spec.Env = []string{"PATH=" + test.path}
			host := NewHost()
			if _, err := host.Start(context.Background(), spec); err != nil {
				t.Fatalf("Start() with PATH=%q and Dir=%q: %v", test.path, spec.Dir, err)
			}

			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				info, err := host.Inspect(context.Background(), spec.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !info.Running {
					if info.ExitCode == nil || *info.ExitCode != 37 {
						t.Fatalf("command exit status = %+v, want 37", info.ExitCode)
					}
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("fixture command did not exit")
		})
	}
}

func testSpec(id string, command []string) contracts.SandboxSpec {
	spec := contracts.SandboxSpec{
		ID:      id,
		Command: command,
		Env:     []string{"PATH=" + os.Getenv("PATH"), "HOME=/tmp"},
	}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		spec.User = "nobody"
	}
	return spec
}

func awaitPIDFile(path string) (int, error) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid, nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, fmt.Errorf("PID file %q was not written", path)
}

func processGone(pid int) bool {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	if runtime.GOOS == "darwin" && err == nil {
		state, psErr := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if psErr != nil {
			return true
		}
		return strings.HasPrefix(strings.TrimSpace(string(state)), "Z")
	}
	return false
}
