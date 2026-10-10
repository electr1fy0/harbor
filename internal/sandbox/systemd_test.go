package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"paas/internal/contracts"
)

func TestSystemdSandboxLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Linux-only systemd sandbox test")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run not installed")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}

	sbx, err := NewSystemd()
	if err != nil {
		t.Fatal(err)
	}

	tmpDir := sandboxArtifactDir(t)
	spec := contracts.SandboxSpec{
		ID:        "sys-test-1",
		Dir:       tmpDir,
		Command:   []string{"/bin/sleep", "30"},
		Env:       []string{"PATH=/bin:/usr/bin"},
		MemoryMax: "64Mi",
		CPUQuota:  0.5,
		TasksMax:  64,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Cleanup(func() {
		if err := sbx.Stop(context.Background(), spec.ID); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	handle, err := sbx.Start(ctx, spec)
	if err != nil {
		t.Fatalf("SystemdSandbox.Start failed: %v", err)
	}
	if handle.PID <= 0 {
		t.Errorf("expected PID > 0, got %d", handle.PID)
	}

	info, err := sbx.Inspect(ctx, spec.ID)
	if err != nil {
		t.Fatalf("SystemdSandbox.Inspect failed: %v", err)
	}
	if !info.Running {
		t.Error("expected unit to be running")
	}
	if info.SystemdUnit != "paas-deploy-sys-test-1.service" {
		t.Errorf("unexpected unit name: %s", info.SystemdUnit)
	}

	if err := sbx.Stop(ctx, spec.ID); err != nil {
		t.Fatalf("SystemdSandbox.Stop failed: %v", err)
	}

	infoAfter, err := sbx.Inspect(ctx, spec.ID)
	if err != nil {
		t.Fatalf("SystemdSandbox.Inspect after stop failed: %v", err)
	}
	if infoAfter.Running {
		t.Error("expected unit to be stopped")
	}
	// A clean exit with RemainAfterExit must also be reported as stopped.
	spec.Command = []string{"/bin/sh", "-c", "exit 0"}
	_, startErr := sbx.Start(ctx, spec)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		info, err = sbx.Inspect(ctx, spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Running && info.ExitCode != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if info.Running || info.ExitCode == nil || *info.ExitCode != 0 {
		t.Fatalf("clean exit incorrectly reported: %+v startErr=%v", info, startErr)
	}
}

func TestSystemdSandboxMemoryOOM(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skipping Linux-only systemd OOM test")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run not installed")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap not installed")
	}

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	memHogSrc := filepath.Join(root, "examples", "memory-hog")
	if _, err := os.Stat(memHogSrc); err != nil {
		t.Skip("examples/memory-hog not found")
	}

	tmpDir := sandboxArtifactDir(t)
	binPath := filepath.Join(tmpDir, "server")
	buildCmd := exec.Command("go", "build", "-o", binPath, filepath.Join(memHogSrc, "main.go"))
	buildCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build memory-hog failed: %v: %s", err, string(out))
	}

	sbx, err := NewSystemd()
	if err != nil {
		t.Fatal(err)
	}

	spec := contracts.SandboxSpec{
		ID:        "sys-oom-test",
		Dir:       tmpDir,
		Command:   []string{"/app/server"},
		Env:       []string{"PORT=41990", "MEMORY_HOG_MB=256", "PATH=/bin:/usr/bin"},
		MemoryMax: "64Mi",
		CPUQuota:  0.5,
		TasksMax:  64,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	t.Cleanup(func() {
		if err := sbx.Stop(context.Background(), spec.ID); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	handle, err := sbx.Start(ctx, spec)
	t.Logf("Start err=%v handle=%+v", err, handle)

	// Poll until unit finishes
	var info *contracts.SandboxInfo
	for i := 0; i < 40; i++ {
		time.Sleep(100 * time.Millisecond)
		info, err = sbx.Inspect(ctx, spec.ID)
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		t.Logf("iter %d: running=%v result=%q oom=%v", i, info.Running, info.Result, info.OOMKilled)
		if err == nil && !info.Running {
			break
		}
	}

	if info == nil || info.Running {
		t.Fatal("expected memory-hog unit to be stopped/killed by OOM killer")
	}
	if !info.OOMKilled && info.Result != "oom-kill" {
		t.Fatalf("expected OOMKilled=true or Result=oom-kill, got: OOMKilled=%v Result=%q", info.OOMKilled, info.Result)
	}
}
