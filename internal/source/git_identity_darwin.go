//go:build darwin

package source

import (
	"os/exec"
	"syscall"
)

func prepareGitWorkspace(workspace, buildUser string) (string, error) { return workspace, nil }

func configureGitCommand(cmd *exec.Cmd, buildUser string) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}
