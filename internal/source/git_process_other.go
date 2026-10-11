//go:build !linux && !darwin

package source

import "os"

func killGitProcessGroup(pid int) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}
