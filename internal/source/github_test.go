package source

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fakeGit struct {
	calls      [][]string
	envs       [][]string
	buildUsers []string
	clone      func(string) error
	block      bool
}

func (g *fakeGit) run(ctx context.Context, executable string, env []string, dir, buildUser string, args ...string) error {
	if executable != "git" {
		return errors.New("unexpected executable")
	}
	g.calls = append(g.calls, append([]string(nil), args...))
	g.envs = append(g.envs, append([]string(nil), env...))
	g.buildUsers = append(g.buildUsers, buildUser)
	command := ""
	for _, arg := range args {
		if arg == "clone" || arg == "fetch" || arg == "checkout" {
			command = arg
			break
		}
	}
	if command == "clone" {
		repoDir := args[len(args)-1]
		if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0700); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(repoDir, "app"), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(repoDir, "app", "paas.yaml"), []byte("name: fixture\n"), 0600); err != nil {
			return err
		}
		if g.clone != nil {
			if err := g.clone(repoDir); err != nil {
				return err
			}
		}
		if g.block {
			<-ctx.Done()
			return ctx.Err()
		}
	}
	return nil
}

func TestCheckoutGitHubKeepsTokenOutOfURLAndAskpassFile(t *testing.T) {
	const token = "ghp_test_secret_123"
	t.Setenv("GITHUB_TOKEN", token)
	git := &fakeGit{}
	selected, cleanup, err := (githubSource{run: git.run, tempDir: t.TempDir(), buildUser: "custom-build"}).checkout(
		context.Background(), "https://github.com/example/private", "main", "app", token,
	)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	defer cleanup()
	for i, call := range git.calls {
		if strings.Contains(strings.Join(call, " "), token) {
			t.Fatalf("token appeared in git arguments: %#v", call)
		}
		if git.buildUsers[i] != "custom-build" {
			t.Fatalf("git build identity = %q, want configured user", git.buildUsers[i])
		}
	}
	var askpassPath string
	for _, value := range git.envs[0] {
		if strings.HasPrefix(value, "GIT_ASKPASS=") {
			askpassPath = strings.TrimPrefix(value, "GIT_ASKPASS=")
		}
	}
	if askpassPath == "" {
		t.Fatal("GITHUB_TOKEN did not configure Git askpass")
	}
	if got := git.envs[0]; !containsEnv(got, "GITHUB_TOKEN="+token) {
		t.Fatal("token is not available to the isolated Git credential helper")
	}
	contents, err := os.ReadFile(askpassPath)
	if err != nil {
		t.Fatalf("read askpass helper: %v", err)
	}
	if strings.Contains(string(contents), token) {
		t.Fatal("token was written into the askpass helper")
	}
	info, err := os.Stat(askpassPath)
	if err != nil {
		t.Fatalf("stat askpass helper: %v", err)
	}
	if info.Mode().Perm()&0111 == 0 {
		t.Fatalf("askpass helper is not executable: %v", info.Mode())
	}
	if !strings.HasSuffix(selected, filepath.Join("repo", "app")) {
		t.Fatalf("selected directory = %q", selected)
	}
}

func TestCheckoutGitHubSelectsRefAndRootAndCleansWorkspace(t *testing.T) {
	work := t.TempDir()
	git := &fakeGit{}
	selected, cleanup, err := (githubSource{run: git.run, timeout: time.Second, tempDir: work}).checkout(
		context.Background(), "https://github.com/example/app.git", "refs/heads/release/v2", "app", "",
	)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if filepath.Base(selected) != "app" || !strings.HasSuffix(selected, filepath.Join("repo", "app")) {
		t.Fatalf("selected source directory = %q", selected)
	}
	if _, err := os.Stat(filepath.Join(selected, "paas.yaml")); err != nil {
		t.Fatalf("selected source file missing: %v", err)
	}
	if len(git.calls) != 3 || !strings.Contains(strings.Join(git.calls[0], " "), "https://github.com/example/app.git") {
		t.Fatalf("git commands = %#v", git.calls)
	}
	if got := strings.Join(git.calls[1], " "); !strings.Contains(got, "fetch --quiet --depth=1 origin refs/heads/release/v2") {
		t.Fatalf("ref was not fetched explicitly: %q", got)
	}
	if got := strings.Join(git.calls[2], " "); !strings.Contains(got, "--detach FETCH_HEAD") {
		t.Fatalf("fetched ref was not checked out: %q", got)
	}
	for _, env := range git.envs {
		for _, forbidden := range []string{"GIT_CONFIG_COUNT=", "GIT_ASKPASS=", "GIT_TRACE=", "GIT_DIR="} {
			for _, value := range env {
				if strings.HasPrefix(value, forbidden) {
					t.Fatalf("inherited Git environment was retained: %q", value)
				}
			}
		}
		if !containsEnv(env, "GIT_CONFIG_NOSYSTEM=1") || !containsEnv(env, "GIT_CONFIG_SYSTEM=/dev/null") || !containsEnv(env, "GIT_CONFIG_GLOBAL=/dev/null") || !containsEnv(env, "GIT_TERMINAL_PROMPT=0") {
			t.Fatalf("Git config or prompting was not isolated: %#v", env)
		}
	}
	cleanup()
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cleanup left source workspace: %v", entries)
	}
}

func TestCheckoutGitHubDefaultRefDoesNotFetchAndDetachesHEAD(t *testing.T) {
	git := &fakeGit{}
	selected, cleanup, err := (githubSource{run: git.run, tempDir: t.TempDir()}).checkout(
		context.Background(), "https://github.com/example/app", "", "", "",
	)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	defer cleanup()
	if len(git.calls) != 2 {
		t.Fatalf("git command count = %d, want clone and checkout", len(git.calls))
	}
	if got := strings.Join(git.calls[1], " "); !strings.Contains(got, "--detach HEAD") {
		t.Fatalf("default branch head was not checked out: %q", got)
	}
	if filepath.Base(selected) != "repo" {
		t.Fatalf("default source path = %q", selected)
	}
}

func TestCheckoutGitHubRejectsInvalidInputBeforeRunningGit(t *testing.T) {
	invalid := []struct {
		name, repoURL, ref, rootDir string
	}{
		{"ssh URL", "git@github.com:example/app.git", "", ""},
		{"non-GitHub host", "https://github.example.com/example/app", "", ""},
		{"credentials", "https://user:pass@github.com/example/app", "", ""},
		{"port", "https://github.com:443/example/app", "", ""},
		{"query", "https://github.com/example/app?ref=main", "", ""},
		{"fragment", "https://github.com/example/app#source", "", ""},
		{"extra path", "https://github.com/example/app/tree/main", "", ""},
		{"escaped path", "https://github.com/example%2fother/app", "", ""},
		{"traversal ref", "https://github.com/example/app", "../../etc", ""},
		{"option ref", "https://github.com/example/app", "--upload-pack=bad", ""},
		{"traversal root", "https://github.com/example/app", "", "app/../other"},
		{"absolute root", "https://github.com/example/app", "", "/tmp/app"},
		{"backslash root", "https://github.com/example/app", "", `app\child`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			git := &fakeGit{}
			_, cleanup, err := (githubSource{run: git.run, tempDir: t.TempDir()}).checkout(
				context.Background(), test.repoURL, test.ref, test.rootDir, "",
			)
			if err == nil {
				if cleanup != nil {
					cleanup()
				}
				t.Fatal("invalid GitHub checkout request succeeded")
			}
			if len(git.calls) != 0 {
				t.Fatalf("invalid request ran git: %#v", git.calls)
			}
		})
	}
}

func TestCheckoutGitHubRejectsSymlinksAndCleansFailedCheckout(t *testing.T) {
	work := t.TempDir()
	git := &fakeGit{clone: func(repoDir string) error {
		return os.Symlink("../../outside", filepath.Join(repoDir, "app", "escape"))
	}}
	_, cleanup, err := (githubSource{run: git.run, tempDir: work}).checkout(
		context.Background(), "https://github.com/example/app", "", "app", "",
	)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink checkout error = %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	entries, readErr := os.ReadDir(work)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed checkout left workspaces: %v", entries)
	}
}

func TestCheckoutGitHubHonorsContextAndCleansCanceledCheckout(t *testing.T) {
	work := t.TempDir()
	git := &fakeGit{block: true}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, cleanup, err := (githubSource{run: git.run, timeout: time.Second, tempDir: work}).checkout(
		ctx, "https://github.com/example/app", "", "", "",
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("checkout error = %v, want deadline exceeded", err)
	}
	if cleanup != nil {
		cleanup()
	}
	entries, readErr := os.ReadDir(work)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("canceled checkout left workspaces: %v", entries)
	}
}

func TestRunGitRedactsTokenFromCommandOutput(t *testing.T) {
	root := t.TempDir()
	buildUser := ""
	if os.Geteuid() == 0 {
		if _, err := user.Lookup("nobody"); err != nil {
			t.Skipf("nobody account is unavailable: %v", err)
		}
		buildUser = "nobody"
		if err := os.Chmod(root, 0755); err != nil {
			t.Fatal(err)
		}
	}
	fake := filepath.Join(root, "git-fake")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$GITHUB_TOKEN\" >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	const token = "ghp_error_secret"
	err := runGit(context.Background(), fake, []string{"GITHUB_TOKEN=" + token}, root, buildUser, "clone")
	if err == nil {
		t.Fatal("fake git unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("Git error was not redacted: %v", err)
	}
}

func TestRunGitCancellationKillsForkedChildWithinBound(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process-group cancellation is only supported on Linux and macOS")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is not available")
	}
	root := t.TempDir()
	buildUser := ""
	if os.Geteuid() == 0 && runtime.GOOS == "linux" {
		if _, err := user.Lookup("nobody"); err != nil {
			t.Skipf("nobody account is unavailable: %v", err)
		}
		buildUser = "nobody"
		if err := os.Chmod(root, 0755); err != nil {
			t.Fatal(err)
		}
	}
	fake := filepath.Join(root, "git-forker")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := runGit(ctx, fake, []string{"PATH=" + os.Getenv("PATH")}, root, buildUser, "clone")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runGit error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("canceled Git process tree took %s to finish", elapsed)
	}
}

func containsEnv(env []string, expected string) bool {
	for _, value := range env {
		if value == expected {
			return true
		}
	}
	return false
}
