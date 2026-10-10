package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"paas/internal/contracts"
)

func TestBubblewrapBuildArgs(t *testing.T) {
	tmpDir := t.TempDir()
	spec := contracts.SandboxSpec{
		ID:      "test-1",
		Command: []string{"/app/server", "--arg1"},
		Dir:     tmpDir,
		Env:     []string{"PORT=41000", "CUSTOM_VAR=val"},
	}

	args, err := BuildBwrapArgs(spec)
	if err != nil {
		t.Fatalf("BuildBwrapArgs failed: %v", err)
	}

	requiredFlags := []string{
		"--die-with-parent",
		"--new-session",
		"--clearenv",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--share-net",
		"--dir",
		"/app",
		"--ro-bind",
		tmpDir,
		"/app",
		"--tmpfs",
		"/tmp",
		"--proc",
		"/proc",
		"--dev",
		"/dev",
		"--setenv",
		"PORT",
		"41000",
		"--setenv",
		"CUSTOM_VAR",
		"val",
	}

	for _, flag := range requiredFlags {
		if !slices.Contains(args, flag) {
			t.Errorf("args missing required flag %q; got: %v", flag, args)
		}
	}

	// Verify command is at the end
	if len(args) < 2 || args[len(args)-2] != "/app/server" || args[len(args)-1] != "--arg1" {
		t.Errorf("expected command at the end of args, got: %v", args[len(args)-2:])
	}
}

func TestBubblewrapMountIsolationAndTmpfs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Linux-only Bubblewrap test")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}

	tmpDir := t.TempDir()
	markerFile := filepath.Join(tmpDir, "marker.txt")
	if err := os.WriteFile(markerFile, []byte("hello read-only"), 0644); err != nil {
		t.Fatal(err)
	}

	sbx, err := NewBubblewrap()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Verify that writing to /app fails (read-only) and writing to /tmp succeeds
	logPath := filepath.Join(t.TempDir(), "test.log")
	spec := contracts.SandboxSpec{
		ID:  "isolation-test",
		Dir: tmpDir,
		Command: []string{
			"/bin/sh", "-c",
			`if touch /app/new_file 2>/dev/null; then exit 10; fi && touch /tmp/new_file && exit 0`,
		},
		Env:     []string{"PATH=/bin:/usr/bin"},
		LogPath: logPath,
	}

	handle, err := sbx.Start(ctx, spec)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Wait for process to finish
	var info *contracts.SandboxInfo
	for i := 0; i < 50; i++ {
		time.Sleep(50 * time.Millisecond)
		info, err = sbx.Inspect(ctx, handle.ID)
		if err == nil && !info.Running {
			break
		}
	}

	if info == nil || info.Running {
		t.Fatal("sandbox did not finish within timeout")
	}
	if info.ExitCode == nil || *info.ExitCode != 0 {
		logContent, _ := os.ReadFile(logPath)
		t.Fatalf("expected exit code 0, got %v; logs: %s", info.ExitCode, string(logContent))
	}
}

func TestBubblewrapPIDNamespace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Linux-only Bubblewrap test")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}

	tmpDir := t.TempDir()
	sbx, err := NewBubblewrap()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Inside the sandbox PID namespace, host PID (os.Getpid()) must NOT exist in /proc
	hostPid := strconv.Itoa(os.Getpid())
	spec := contracts.SandboxSpec{
		ID:  "pid-test",
		Dir: tmpDir,
		Command: []string{
			"/bin/sh", "-c",
			`if [ -d "/proc/` + hostPid + `" ]; then exit 20; fi && exit 0`,
		},
		Env: []string{"PATH=/bin:/usr/bin"},
	}

	handle, err := sbx.Start(ctx, spec)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	var info *contracts.SandboxInfo
	for i := 0; i < 50; i++ {
		time.Sleep(50 * time.Millisecond)
		info, err = sbx.Inspect(ctx, handle.ID)
		if err == nil && !info.Running {
			break
		}
	}

	if info == nil || info.Running {
		t.Fatal("sandbox did not finish within timeout")
	}
	if info.ExitCode == nil || *info.ExitCode != 0 {
		t.Fatalf("expected exit code 0 (host PID %s hidden), got %v", hostPid, info.ExitCode)
	}
}
