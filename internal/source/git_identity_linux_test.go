//go:build linux

package source

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestConfigureGitCommandDropsRootAndSupplementaryGroups(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to inspect privilege dropping")
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("nobody account is unavailable: %v", err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil || uid == 0 {
		t.Fatalf("unexpected nobody UID %q", account.Uid)
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		t.Fatalf("unexpected nobody GID %q", account.Gid)
	}
	cmd := exec.Command("git", "--version")
	if err := configureGitCommand(cmd, "nobody"); err != nil {
		t.Fatalf("configure command: %v", err)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("Git command has no unprivileged credentials")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("Git command is not isolated in a process group")
	}
	credential := cmd.SysProcAttr.Credential
	if credential.Uid != uint32(uid) || credential.Gid != uint32(gid) {
		t.Fatalf("Git UID/GID = %d/%d, want %d/%d", credential.Uid, credential.Gid, uid, gid)
	}
	if credential.NoSetGroups || len(credential.Groups) != 0 {
		t.Fatalf("Git supplementary groups are not cleared: %+v", credential)
	}
}

func TestRootCheckoutAskpassIsExecutableAndRootOwned(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to verify the Git helper owner")
	}
	if _, err := user.Lookup("nobody"); err != nil {
		t.Skipf("nobody account is unavailable: %v", err)
	}
	t.Setenv("GITHUB_TOKEN", "ghp_root_test_secret")
	root := t.TempDir()
	git := &fakeGit{}
	selected, cleanup, err := (githubSource{run: git.run, timeout: time.Second, tempDir: root, buildUser: "nobody"}).checkout(
		context.Background(), "https://github.com/example/private", "", "app", "ghp_root_test_secret",
	)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	defer cleanup()
	if !strings.HasSuffix(selected, filepath.Join("repo", "app")) {
		t.Fatalf("selected path = %q", selected)
	}
	var askpassPath string
	for _, value := range git.envs[0] {
		if strings.HasPrefix(value, "GIT_ASKPASS=") {
			askpassPath = strings.TrimPrefix(value, "GIT_ASKPASS=")
		}
	}
	info, err := os.Stat(askpassPath)
	if err != nil {
		t.Fatalf("stat askpass helper: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm() != 0555 {
		t.Fatalf("askpass owner/mode = %#v / %04o, want root / 0555", info.Sys(), info.Mode().Perm())
	}
	workspacePath := filepath.Dir(askpassPath)
	workspaceInfo, err := os.Stat(workspacePath)
	if err != nil {
		t.Fatalf("stat source workspace: %v", err)
	}
	workspaceStat, ok := workspaceInfo.Sys().(*syscall.Stat_t)
	if !ok || workspaceStat.Uid != 0 || workspaceStat.Gid != uint32(mustLookupGID(t, "nobody")) || workspaceInfo.Mode().Perm() != 0710 {
		t.Fatalf("workspace owner/group/mode = %#v / %04o, want root:build-group / 0710", workspaceInfo.Sys(), workspaceInfo.Mode().Perm())
	}
	gitDirInfo, err := os.Stat(filepath.Join(workspacePath, "checkout"))
	if err != nil {
		t.Fatalf("stat writable checkout directory: %v", err)
	}
	gitDirStat, ok := gitDirInfo.Sys().(*syscall.Stat_t)
	if !ok || gitDirStat.Uid != uint32(mustLookupUID(t, "nobody")) || gitDirInfo.Mode().Perm() != 0700 {
		t.Fatalf("checkout owner/mode = %#v / %04o, want build user / 0700", gitDirInfo.Sys(), gitDirInfo.Mode().Perm())
	}
}

func mustLookupUID(t *testing.T, name string) int {
	t.Helper()
	account, err := user.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		t.Fatal(err)
	}
	return uid
}

func mustLookupGID(t *testing.T, name string) int {
	t.Helper()
	account, err := user.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	return gid
}
