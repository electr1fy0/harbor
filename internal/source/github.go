// Package source resolves remote source locations into private local trees.
package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

// GitMetadata describes the exact commit selected by a checkout. Message is
// the full commit message; URL is the canonical GitHub commit page.
type GitMetadata struct {
	CommitSHA   string `json:"commitSHA"`
	Message     string `json:"message"`
	Author      string `json:"author"`
	AuthorEmail string `json:"authorEmail"`
	URL         string `json:"url"`
}

// CheckoutGitHubAsWithTokenAndMetadata performs a private checkout and
// returns metadata read from the detached checkout itself. Callers that need
// webhook reproducibility should pass a full commit SHA as ref.
func CheckoutGitHubAsWithTokenAndMetadata(ctx context.Context, repoURL, ref, rootDir, buildUser, token string) (string, func(), GitMetadata, error) {
	return (githubSource{run: runGit, timeout: defaultCheckoutTimeout, buildUser: buildUser}).checkoutWithMetadata(ctx, repoURL, ref, rootDir, token)
}

// ReadGitMetadata reads the exact checked-out commit from a selected source
// directory, including when that directory is nested under a repository root.
func ReadGitMetadata(ctx context.Context, sourceDir, repoURL, buildUser string) (GitMetadata, error) {
	if ctx == nil {
		return GitMetadata{}, errors.New("metadata context must not be nil")
	}
	if strings.TrimSpace(sourceDir) == "" {
		return GitMetadata{}, errors.New("source directory is required")
	}
	cloneURL, err := canonicalGitHubURL(repoURL)
	if err != nil {
		return GitMetadata{}, err
	}
	metadataCtx, cancel := context.WithTimeout(ctx, defaultCheckoutTimeout)
	defer cancel()
	return gitMetadata(metadataCtx, gitEnvironment("", ""), sourceDir, buildUser, sourceDir, cloneURL)
}

func (s githubSource) checkout(ctx context.Context, repoURL, ref, rootDir, token string) (string, func(), error) {
	selected, cleanup, _, err := s.checkoutInternal(ctx, repoURL, ref, rootDir, token, false)
	return selected, cleanup, err
}

func (s githubSource) checkoutWithMetadata(ctx context.Context, repoURL, ref, rootDir, token string) (string, func(), GitMetadata, error) {
	return s.checkoutInternal(ctx, repoURL, ref, rootDir, token, true)
}

func (s githubSource) checkoutInternal(ctx context.Context, repoURL, ref, rootDir, token string, includeMetadata bool) (string, func(), GitMetadata, error) {
	if ctx == nil {
		return "", nil, GitMetadata{}, errors.New("checkout context must not be nil")
	}
	cloneURL, err := canonicalGitHubURL(repoURL)
	if err != nil {
		return "", nil, GitMetadata{}, err
	}
	if err := validateRef(ref); err != nil {
		return "", nil, GitMetadata{}, err
	}
	subdir, err := cleanRootDir(rootDir)
	if err != nil {
		return "", nil, GitMetadata{}, err
	}
	if s.run == nil {
		return "", nil, GitMetadata{}, errors.New("git runner is not configured")
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
		return "", nil, GitMetadata{}, fmt.Errorf("create source workspace: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(workspace) }
	if err := os.Chmod(workspace, 0700); err != nil {
		cleanup()
		return "", nil, GitMetadata{}, fmt.Errorf("secure source workspace: %w", err)
	}
	gitWorkDir, err := prepareGitWorkspace(workspace, s.buildUser)
	if err != nil {
		cleanup()
		return "", nil, GitMetadata{}, err
	}

	repoDir := filepath.Join(gitWorkDir, "repo")
	if strings.ContainsAny(token, "\r\n") {
		cleanup()
		return "", nil, GitMetadata{}, errors.New("GitHub token contains an invalid line break")
	}
	askpassPath := ""
	if token != "" {
		askpassPath = filepath.Join(workspace, "git-askpass")
		if err := os.WriteFile(askpassPath, []byte(githubAskpassScript), 0500); err != nil {
			cleanup()
			return "", nil, GitMetadata{}, fmt.Errorf("create private Git credential helper: %w", err)
		}
		// The Linux checkout account must be able to execute the helper while
		// the token remains available only in the process environment.
		if err := os.Chmod(askpassPath, 0555); err != nil {
			cleanup()
			return "", nil, GitMetadata{}, fmt.Errorf("secure private Git credential helper: %w", err)
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
		return "", nil, GitMetadata{}, fmt.Errorf("clone GitHub repository: %w", err)
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
			return "", nil, GitMetadata{}, fmt.Errorf("fetch repository ref: %w", err)
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
		return "", nil, GitMetadata{}, fmt.Errorf("check out repository source: %w", err)
	}

	selected := repoDir
	if subdir != "." {
		selected = filepath.Join(repoDir, filepath.FromSlash(subdir))
	}
	if err := verifySourceTree(repoDir, selected, subdir); err != nil {
		cleanup()
		return "", nil, GitMetadata{}, err
	}
	var metadata GitMetadata
	if includeMetadata {
		metadata, err = gitMetadata(checkoutCtx, env, workspace, s.buildUser, repoDir, cloneURL)
		if err != nil {
			cleanup()
			return "", nil, GitMetadata{}, err
		}
	}
	return selected, cleanup, metadata, nil
}

func gitMetadata(ctx context.Context, env []string, directory, buildUser, repoDir, cloneURL string) (GitMetadata, error) {
	command := exec.CommandContext(ctx, "git", "-C", repoDir, "show", "-s", "--format=%H%x00%an%x00%ae%x00%B", "HEAD")
	command.Dir = directory
	command.Env = env
	if err := configureGitCommand(command, buildUser); err != nil {
		return GitMetadata{}, fmt.Errorf("read selected commit metadata: %w", err)
	}
	var metadataOutput boundedMetadataOutput
	command.Stdout = &metadataOutput
	command.Stderr = io.Discard
	err := command.Run()
	output := metadataOutput.Bytes()
	if err != nil {
		if ctx.Err() != nil {
			return GitMetadata{}, ctx.Err()
		}
		return GitMetadata{}, errors.New("read selected commit metadata")
	}
	if metadataOutput.tooLarge {
		return GitMetadata{}, errors.New("selected commit metadata is too large")
	}
	parts := strings.SplitN(string(output), "\x00", 4)
	if len(parts) != 4 {
		return GitMetadata{}, errors.New("selected commit metadata is invalid")
	}
	sha := strings.TrimSpace(parts[0])
	if len(sha) != 40 && len(sha) != 64 {
		return GitMetadata{}, errors.New("selected commit SHA is invalid")
	}
	for _, r := range sha {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return GitMetadata{}, errors.New("selected commit SHA is invalid")
		}
	}
	repoURL := strings.TrimSuffix(cloneURL, ".git")
	return GitMetadata{CommitSHA: sha, Author: strings.TrimSpace(parts[1]), AuthorEmail: strings.TrimSpace(parts[2]), Message: strings.TrimRight(parts[3], "\r\n"), URL: repoURL + "/commit/" + sha}, nil
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

// Drain oversized metadata without retaining an unbounded commit message.
type boundedMetadataOutput struct {
	bytes.Buffer
	tooLarge bool
}

func (w *boundedMetadataOutput) Write(data []byte) (int, error) {
	size := len(data)
	remaining := (16 << 10) - w.Len()
	if size > remaining {
		w.tooLarge = true
		data = data[:remaining]
	}
	_, err := w.Buffer.Write(data)
	return size, err
}
