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
	tmpDir := sandboxArtifactDir(t)
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

	tmpDir := sandboxArtifactDir(t)
	markerFile := filepath.Join(tmpDir, "marker.txt")
	if err := os.WriteFile(markerFile, []byte("hello read-only"), 0644); err != nil {
		t.Fatal(err)
	}
	volumeDir := sandboxArtifactDir(t)
	if err := os.Chmod(volumeDir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volumeDir, "marker"), []byte("persistent"), 0644); err != nil {
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
			`if touch /app/new_file 2>/dev/null; then exit 10; fi && touch /tmp/new_file && test "$(cat /tmp/managed/marker)" = persistent && printf saved > /tmp/managed/result`,
		},
		Binds:   []contracts.BindMount{{HostPath: volumeDir, MountPath: "/tmp/managed"}},
		Env:     []string{"PATH=/bin:/usr/bin"},
		LogPath: logPath,
	}

	t.Cleanup(func() {
		if err := sbx.Stop(context.Background(), spec.ID); err != nil {
			t.Errorf("stop sandbox: %v", err)
		}
	})
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
	if result, err := os.ReadFile(filepath.Join(volumeDir, "result")); err != nil || string(result) != "saved" {
		t.Fatalf("persistent volume write = %q, %v; want saved", result, err)
	}
}

func TestBubblewrapPIDNamespace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Linux-only Bubblewrap test")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}

	tmpDir := sandboxArtifactDir(t)
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

	t.Cleanup(func() {
		if err := sbx.Stop(context.Background(), spec.ID); err != nil {
			t.Errorf("stop sandbox: %v", err)
		}
	})
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

func TestBubblewrapPathsAndEnvironmentAreNotSilentlyRewritten(t *testing.T) {
	dir := t.TempDir()
	literal := "message contains " + dir + "/server"
	spec := contracts.SandboxSpec{ID: "path-test", Dir: dir, Command: []string{filepath.Join(dir, "server"), literal, dir + "-sibling/server"}}
	args, err := BuildBwrapArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--", "/app/server", literal, dir + "-sibling/server"}
	if !slices.Equal(args[len(args)-len(want):], want) {
		t.Fatalf("unexpected command suffix: %v", args)
	}
	if index := slices.Index(args, "--chdir"); index < 0 || args[index+1] != "/app" {
		t.Fatal("missing artifact working directory")
	}
	for i, arg := range args {
		if arg == "--ro-bind" && args[i+1] == "/etc" {
			t.Fatal("host /etc exposed wholesale")
		}
	}
	spec.Command[0] = dir + "-sibling/server"
	args, err = BuildBwrapArgs(spec)
	if err != nil || args[len(args)-3] != spec.Command[0] {
		t.Fatalf("sibling executable rewritten: %v %v", args, err)
	}
	spec.Env = []string{"broken"}
	if _, err := BuildBwrapArgs(spec); err == nil {
		t.Fatal("malformed environment silently accepted")
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	spec.Env = nil
	spec.Dir = file
	if _, err := BuildBwrapArgs(spec); err == nil {
		t.Fatal("file accepted as artifact directory")
	}
}

func sandboxArtifactDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "harbor-sandbox-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove artifacts: %v", err)
		}
	})
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	return dir
}
