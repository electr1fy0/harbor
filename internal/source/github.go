// Package source resolves remote source locations into private local trees.
package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const defaultCheckoutTimeout = 2 * time.Minute

// gitRunner runs one git command. Keeping this seam private lets tests exercise
// checkout behavior without making network requests.
type gitRunner func(context.Context, string, []string, string, string, ...string) error

type githubSource struct {
	run       gitRunner
	timeout   time.Duration
	tempDir   string
	buildUser string
}

// CheckoutGitHub clones a GitHub repository over HTTPS and returns the
// selected source directory and a cleanup function. The checkout is private to
// the daemon process and should be removed after the caller has copied source.
// If GITHUB_TOKEN is set, Git uses it through a temporary askpass helper for
// private repositories. Ref may name a branch, tag, or full commit hash.
// RootDir is a repository-relative directory; an empty value selects the root.
func CheckoutGitHub(ctx context.Context, repoURL, ref, rootDir string) (string, func(), error) {
	return CheckoutGitHubAs(ctx, repoURL, ref, rootDir, "paas-build")
}

// CheckoutGitHubAs performs CheckoutGitHub with the configured unprivileged
// build identity when the daemon runs as root on Linux.
func CheckoutGitHubAs(ctx context.Context, repoURL, ref, rootDir, buildUser string) (string, func(), error) {
	return CheckoutGitHubAsWithToken(ctx, repoURL, ref, rootDir, buildUser, os.Getenv("GITHUB_TOKEN"))
}

// CheckoutGitHubWithToken checks out a repository using token only for this
// call. An empty token preserves public repository access.
func CheckoutGitHubWithToken(ctx context.Context, repoURL, ref, rootDir, token string) (string, func(), error) {
	return CheckoutGitHubAsWithToken(ctx, repoURL, ref, rootDir, "paas-build", token)
}

// CheckoutGitHubAsWithToken performs a private checkout without mutating the
// process environment. The legacy CheckoutGitHubAs wrapper retains the
// GITHUB_TOKEN environment fallback for CLI compatibility.
func CheckoutGitHubAsWithToken(ctx context.Context, repoURL, ref, rootDir, buildUser, token string) (string, func(), error) {
	return (githubSource{run: runGit, timeout: defaultCheckoutTimeout, buildUser: buildUser}).checkout(ctx, repoURL, ref, rootDir, token)
}

func (s githubSource) checkout(ctx context.Context, repoURL, ref, rootDir, token string) (string, func(), error) {
	if ctx == nil {
		return "", nil, errors.New("checkout context must not be nil")
	}
	cloneURL, err := canonicalGitHubURL(repoURL)
	if err != nil {
		return "", nil, err
	}
	if err := validateRef(ref); err != nil {
		return "", nil, err
	}
	subdir, err := cleanRootDir(rootDir)
	if err != nil {
		return "", nil, err
	}
	if s.run == nil {
		return "", nil, errors.New("git runner is not configured")
	}
	timeout := s.timeout
	if timeout <= 0 {
		timeout = defaultCheckoutTimeout
	}
	checkoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tempDir := s.tempDir
	if tempDir == "" && os.Geteuid() == 0 {
		// Keep the checkout reachable after dropping to the build identity even
		// when the daemon inherited a root-only TMPDIR.
		tempDir = "/tmp"
	}
	workspace, err := os.MkdirTemp(tempDir, "paas-source-")
	if err != nil {
		return "", nil, fmt.Errorf("create source workspace: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(workspace) }
	if err := os.Chmod(workspace, 0700); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("secure source workspace: %w", err)
	}
	gitWorkDir, err := prepareGitWorkspace(workspace, s.buildUser)
	if err != nil {
		cleanup()
		return "", nil, err
	}

	repoDir := filepath.Join(gitWorkDir, "repo")
	if strings.ContainsAny(token, "\r\n") {
		cleanup()
		return "", nil, errors.New("GitHub token contains an invalid line break")
	}
	askpassPath := ""
	if token != "" {
		askpassPath = filepath.Join(workspace, "git-askpass")
		if err := os.WriteFile(askpassPath, []byte(githubAskpassScript), 0500); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("create private Git credential helper: %w", err)
		}
		// The Linux checkout account must be able to execute the helper while
		// the token remains available only in the process environment.
		if err := os.Chmod(askpassPath, 0555); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("secure private Git credential helper: %w", err)
		}
	}
	env := gitEnvironment(token, askpassPath)
	if err := s.run(checkoutCtx, "git", env, gitWorkDir, s.buildUser,
		"-c", "credential.helper=",
		"-c", "core.hooksPath="+filepath.Join(workspace, "no-hooks"),
		"-c", "protocol.file.allow=never",
		"-c", "http.followRedirects=false",
		"clone", "--quiet", "--no-checkout", "--depth=1", "--no-tags", "--", cloneURL, repoDir,
	); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("clone GitHub repository: %w", err)
	}

	if ref != "" {
		if err := s.run(checkoutCtx, "git", env, workspace, s.buildUser,
			"-c", "credential.helper=",
			"-c", "core.hooksPath="+filepath.Join(workspace, "no-hooks"),
			"-c", "protocol.file.allow=never",
			"-c", "http.followRedirects=false",
			"-C", repoDir, "fetch", "--quiet", "--depth=1", "origin", ref,
		); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("fetch repository ref: %w", err)
		}
	}

	checkoutTarget := "HEAD"
	if ref != "" {
		checkoutTarget = "FETCH_HEAD"
	}
	if err := s.run(checkoutCtx, "git", env, workspace, s.buildUser,
		"-c", "credential.helper=",
		"-c", "core.hooksPath="+filepath.Join(workspace, "no-hooks"),
		"-c", "protocol.file.allow=never",
		"-c", "http.followRedirects=false",
		"-C", repoDir, "checkout", "--quiet", "--detach", checkoutTarget,
	); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("check out repository source: %w", err)
	}

	selected := repoDir
	if subdir != "." {
		selected = filepath.Join(repoDir, filepath.FromSlash(subdir))
	}
	if err := verifySourceTree(repoDir, selected, subdir); err != nil {
		cleanup()
		return "", nil, err
	}
	return selected, cleanup, nil
}

const githubAskpassScript = "#!/bin/sh\ncase \"$1\" in\n  *Username*) printf '%s\\n' 'x-access-token' ;;\n  *) printf '%s\\n' \"$GITHUB_TOKEN\" ;;\nesac\n"

func gitEnvironment(token, askpassPath string) []string {
	pathValue := os.Getenv("PATH")
	if pathValue == "" {
		pathValue = "/usr/bin:/bin:/usr/local/bin"
	}
	env := []string{
		"PATH=" + pathValue,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LFS_SKIP_SMUDGE=1",
	}
	if token != "" {
		env = append(env, "GITHUB_TOKEN="+token, "GIT_ASKPASS="+askpassPath)
	}
	return env
}

func runGit(ctx context.Context, executable string, env []string, directory, buildUser string, args ...string) error {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = directory
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	if err := configureGitCommand(cmd, buildUser); err != nil {
		return err
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return killGitProcessGroup(cmd.Process.Pid)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		message := strings.TrimSpace(string(output))
		for _, value := range env {
			if strings.HasPrefix(value, "GITHUB_TOKEN=") {
				message = strings.ReplaceAll(message, strings.TrimPrefix(value, "GITHUB_TOKEN="), "[redacted]")
			}
		}
		if message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
		return err
	}
	return nil
}

func canonicalGitHubURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid GitHub repository URL: %w", err)
	}
	if u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || u.Fragment != "" || u.RawPath != "" {
		return "", errors.New("RepoURL must be an HTTPS github.com owner/repository URL")
	}
	if strings.Contains(u.EscapedPath(), "%") {
		return "", errors.New("RepoURL must not contain escaped path segments")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.HasSuffix(u.Path, "/") {
		return "", errors.New("RepoURL must contain exactly one owner and one repository")
	}
	for _, part := range parts {
		if part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") {
			return "", errors.New("RepoURL contains an invalid owner or repository")
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
				return "", errors.New("RepoURL contains an invalid owner or repository")
			}
		}
	}
	if strings.TrimSuffix(parts[1], ".git") == "" || strings.TrimSuffix(parts[1], ".git") == "." || strings.TrimSuffix(parts[1], ".git") == ".." {
		return "", errors.New("RepoURL contains an invalid repository name")
	}
	return "https://github.com/" + parts[0] + "/" + parts[1], nil
}

// ValidateGitHubCheckoutRequest validates repository coordinates without
// contacting GitHub, so daemon APIs can reject malformed input before queueing.
func ValidateGitHubCheckoutRequest(repoURL, ref, rootDir string) error {
	if _, err := canonicalGitHubURL(repoURL); err != nil {
		return err
	}
	if err := validateRef(ref); err != nil {
		return err
	}
	_, err := cleanRootDir(rootDir)
	return err
}

func validateRef(ref string) error {
	if ref == "" {
		return nil
	}
	if len(ref) == 40 || len(ref) == 64 {
		allHex := true
		for _, r := range ref {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
				allHex = false
				break
			}
		}
		if allHex {
			return nil
		}
	}
	if ref == "@" || strings.HasPrefix(ref, "-") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") {
		return errors.New("Ref must be a branch, tag, or full commit hash")
	}
	if strings.ContainsAny(ref, "~^:?*[\\") || strings.ContainsRune(ref, '\\') {
		return errors.New("Ref must be a branch, tag, or full commit hash")
	}
	for _, r := range ref {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("Ref must be a branch, tag, or full commit hash")
		}
	}
	for _, component := range strings.Split(ref, "/") {
		if component == "" || component == "." || component == ".." || strings.HasPrefix(component, ".") || strings.HasSuffix(strings.ToLower(component), ".lock") {
			return errors.New("Ref must be a branch, tag, or full commit hash")
		}
	}
	return nil
}

func cleanRootDir(rootDir string) (string, error) {
	if rootDir == "" {
		return ".", nil
	}
	if strings.Contains(rootDir, "\\") || !fs.ValidPath(rootDir) {
		return "", errors.New("RootDir must be a repository-relative directory without path traversal")
	}
	for _, component := range strings.Split(rootDir, "/") {
		if component == ".git" {
			return "", errors.New("RootDir must not select Git metadata")
		}
	}
	return path.Clean(rootDir), nil
}

func verifySourceTree(repoDir, selected, subdir string) error {
	for current := selected; ; {
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect repository root directory %q: %w", subdir, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("repository root directory %q must be a real directory", subdir)
		}
		if current == repoDir {
			break
		}
		parent := filepath.Dir(current)
		if parent == current || !strings.HasPrefix(current, repoDir+string(os.PathSeparator)) {
			return errors.New("RootDir escapes the repository checkout")
		}
		current = parent
	}

	err := filepath.WalkDir(selected, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == repoDir && subdir == "." {
			return nil
		}
		if filePath == filepath.Join(repoDir, ".git") && subdir == "." {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return errors.New("repository contains an invalid .git entry")
		}
		if entry.Name() == ".git" {
			return errors.New("repository source contains nested Git metadata")
		}
		info, err := os.Lstat(filePath)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("repository source contains a symlink: %s", filepath.Base(filePath))
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("repository source contains an unsupported file: %s", filepath.Base(filePath))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("inspect repository source: %w", err)
	}
	return nil
}
