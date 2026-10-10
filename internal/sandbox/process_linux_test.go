//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"paas/internal/contracts"
)

func TestLinuxProcessAttributes(t *testing.T) {
	cmd := exec.Command("/bin/true")
	userName := "nobody"
	release, err := configureCommand(cmd, contracts.SandboxSpec{User: userName})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("process group was not enabled")
	}
	if cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("Pdeathsig is %v, want SIGKILL", cmd.SysProcAttr.Pdeathsig)
	}
	if os.Geteuid() == 0 {
		target, err := user.Lookup(userName)
		if err != nil {
			t.Fatal(err)
		}
		uid, err := strconv.ParseUint(target.Uid, 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		if cmd.SysProcAttr.Credential == nil || cmd.SysProcAttr.Credential.Uid != uint32(uid) || uid == 0 {
			t.Fatalf("unexpected runtime credentials: %+v", cmd.SysProcAttr.Credential)
		}
	} else if cmd.SysProcAttr.Credential != nil {
		t.Fatalf("unprivileged launch changed credentials: %+v", cmd.SysProcAttr.Credential)
	}
}

func TestLinuxRejectsUIDZeroRuntimeUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to configure a dropped-privilege launch")
	}
	_, err := configureCommand(exec.Command("/bin/true"), contracts.SandboxSpec{User: "root"})
	if err == nil || !strings.Contains(err.Error(), "UID 0") {
		t.Fatalf("expected UID 0 rejection, got %v", err)
	}
}

func TestLinuxRootLaunchRunsAsNamedUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to verify privilege dropping")
	}
	target, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal(err)
	}
	logPath := t.TempDir() + "/uid.log"
	host := NewHost()
	spec := contracts.SandboxSpec{
		ID:      "credential-check",
		Command: []string{"/bin/sh", "-c", "id -u; sleep 0.1"},
		Env:     []string{"PATH=/usr/bin:/bin", "HOME=/tmp"},
		User:    "nobody",
		LogPath: logPath,
	}
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
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := host.Stop(stopCtx, spec.ID); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(output))
	if got != target.Uid {
		t.Fatalf("process ran as UID %q, want %q", got, target.Uid)
	}
}
