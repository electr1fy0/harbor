//go:build linux

package source

import (
	"os"
	"syscall"
)

func killGitProcessGroup(pid int) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return os.ErrProcessDone
	}
	return err
}
