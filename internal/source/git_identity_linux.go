//go:build linux

package source

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

func prepareGitWorkspace(workspace, buildUser string) (string, error) {
	if os.Geteuid() != 0 {
		return workspace, nil
	}
	uid, gid, err := buildUserIDs(buildUser)
	if err != nil {
		return "", err
	}
	if err := os.Chown(workspace, 0, gid); err != nil {
		return "", fmt.Errorf("assign Git checkout workspace group: %w", err)
	}
	if err := os.Chmod(workspace, 0710); err != nil {
		return "", fmt.Errorf("secure Git checkout workspace: %w", err)
	}
	gitWorkDir := filepath.Join(workspace, "checkout")
	if err := os.Mkdir(gitWorkDir, 0700); err != nil {
		return "", fmt.Errorf("create Git checkout directory: %w", err)
	}
	if err := os.Chown(gitWorkDir, uid, gid); err != nil {
		return "", fmt.Errorf("assign Git checkout directory to %s: %w", buildUser, err)
	}
	return gitWorkDir, nil
}

func configureGitCommand(cmd *exec.Cmd, buildUser string) error {
	attributes := &syscall.SysProcAttr{Setpgid: true}
	if os.Geteuid() != 0 {
		cmd.SysProcAttr = attributes
		return nil
	}
	uid, gid, err := buildUserIDs(buildUser)
	if err != nil {
		return err
	}
	attributes.Credential = &syscall.Credential{
		Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}, NoSetGroups: false,
	}
	cmd.SysProcAttr = attributes
	return nil
}

func buildUserIDs(name string) (int, int, error) {
	if name == "" {
		return 0, 0, fmt.Errorf("a non-root build user is required for Git checkouts")
	}
	account, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("lookup build user %q: %w", name, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid <= 0 {
		return 0, 0, fmt.Errorf("build user %q must have a non-root UID", name)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil || gid < 0 {
		return 0, 0, fmt.Errorf("build user %q has an invalid primary GID", name)
	}
	return uid, gid, nil
}
