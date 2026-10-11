//go:build !linux && !darwin

package source

import "os/exec"

func prepareGitWorkspace(workspace, buildUser string) (string, error) { return workspace, nil }

func configureGitCommand(cmd *exec.Cmd, buildUser string) error { return nil }
