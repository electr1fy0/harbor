//go:build darwin

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"paas/internal/contracts"
)

func configureCommand(cmd *exec.Cmd, _ contracts.SandboxSpec) (func(), error) {
	if os.Geteuid() == 0 {
		return nil, errors.New("host sandbox cannot drop root credentials on Darwin")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return func() {}, nil
}

func signalProcessGroup(pgid int, signal syscall.Signal) error {
	err := syscall.Kill(-pgid, signal)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

func processGroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || err == syscall.EPERM
}

func finishProcessTree(pgid int) {
	if processGroupAlive(pgid) {
		_ = signalProcessGroup(pgid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processGroupAlive(pgid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processGroupAlive(pgid) {
		_ = signalProcessGroup(pgid, syscall.SIGKILL)
	}
	// Darwin has no child-subreaper facility. Give SIGKILL time to take effect;
	// any remaining group entries are unreapable zombies owned by launchd.
	time.Sleep(200 * time.Millisecond)
}

func exitStatus(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok {
		if status.Exited() {
			return status.ExitStatus()
		}
		if status.Signaled() {
			return 128 + int(status.Signal())
		}
	}
	return state.ExitCode()
}
