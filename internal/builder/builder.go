// Package builder implements the Go source builder and immutable release store.
package builder

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"paas/internal/config"
	"paas/internal/contracts"
)

const (
	defaultBuildUser = "paas-build"
	defaultTimeout   = 5 * time.Minute
)

// Options configures the Go builder and its release store.
type Options struct {
	DataDir   string
	BuildUser string
	Timeout   time.Duration
}

// GoBuilder builds Go applications and publishes immutable releases.
type GoBuilder struct {
	dataDir   string
	buildUser string
	timeout   time.Duration
	cacheMu   sync.RWMutex
}

// New creates a Go builder. An empty BuildUser selects paas-build, and a
// non-positive Timeout selects five minutes.
func New(opts Options) *GoBuilder {
	buildUser := opts.BuildUser
	if buildUser == "" {
		buildUser = defaultBuildUser
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &GoBuilder{dataDir: opts.DataDir, buildUser: buildUser, timeout: timeout}
}

// CacheDir returns the builder's persistent cache directory. The caller must
// not remove it while a build is running; Build serializes cache clearing with
// all builds performed by this builder instance.
func (b *GoBuilder) CacheDir() string { return filepath.Join(b.dataDir, "build-cache") }

// PruneCache safely removes the persistent build cache after all builds using
// this builder have completed. It can be used by app deletion and maintenance.
func (b *GoBuilder) PruneCache(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("cache-prune context must not be nil")
	}
	if err := b.acquireCacheLock(ctx, true); err != nil {
		return err
	}
	defer b.cacheMu.Unlock()
	cacheDir := filepath.Join(b.dataDir, "build-cache")
	info, err := os.Lstat(cacheDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect build cache: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("build cache path must be a real directory")
	}
	if err := os.RemoveAll(cacheDir); err != nil {
		return fmt.Errorf("prune build cache: %w", err)
	}
	return nil
}

func (b *GoBuilder) acquireCacheLock(ctx context.Context, exclusive bool) error {
	if ctx == nil {
		return fmt.Errorf("build cache context must not be nil")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if exclusive && b.cacheMu.TryLock() {
			if err := ctx.Err(); err != nil {
				b.cacheMu.Unlock()
				return err
			}
			return nil
		}
		if !exclusive && b.cacheMu.TryRLock() {
			if err := ctx.Err(); err != nil {
				b.cacheMu.RUnlock()
				return err
			}
			return nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type releaseManifest struct {
	ID          string    `json:"id"`
	SourceHash  string    `json:"source_hash"`
	Runtime     string    `json:"runtime"`
	ArtifactDir string    `json:"artifact_dir"`
	Command     []string  `json:"command"`
	CreatedAt   time.Time `json:"created_at"`
}

// Build copies source to a private workspace, runs the configured build, and
// atomically publishes its output as a read-only release.
func (b *GoBuilder) Build(ctx context.Context, req contracts.BuildRequest) (*contracts.Release, error) {
	if ctx == nil {
		return nil, fmt.Errorf("build context must not be nil")
	}
	if strings.TrimSpace(b.dataDir) == "" {
		return nil, fmt.Errorf("builder data directory must not be empty")
	}
	if !config.ValidName(req.AppName) {
		return nil, fmt.Errorf("invalid application name %q", req.AppName)
	}
	if req.Runtime != "go" && req.Runtime != "node" {
		return nil, fmt.Errorf("unsupported runtime %q: only go and node are supported", req.Runtime)
	}
	if strings.TrimSpace(req.BuildCommand) == "" {
		return nil, fmt.Errorf("build command must not be empty")
	}
	if req.Runtime == "go" && strings.TrimSpace(req.StartCommand) != "/app/server" {
		return nil, fmt.Errorf("unsupported start command %q: expected /app/server", req.StartCommand)
	}
	if req.Runtime == "node" && strings.TrimSpace(req.StartCommand) == "" {
		return nil, fmt.Errorf("start command must not be empty")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if os.Geteuid() == 0 && runtime.GOOS != "linux" {
		return nil, fmt.Errorf("root builds require Linux systemd-run")
	}

	dataDir, err := filepath.Abs(b.dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve builder data directory: %w", err)
	}
	cacheDir := filepath.Join(dataDir, "build-cache")
	buildTimeout := req.BuildTimeout
	if req.BuildTimeout > 0 && (req.BuildTimeout < time.Second || req.BuildTimeout > 30*time.Minute) {
		return nil, fmt.Errorf("build timeout must be between one second and thirty minutes")
	}
	if buildTimeout <= 0 {
		buildTimeout = b.timeout
	}
	if err := b.acquireCacheLock(ctx, req.ClearCache); err != nil {
		return nil, err
	}
	if req.ClearCache {
		defer b.cacheMu.Unlock()
	} else {
		defer b.cacheMu.RUnlock()
	}
	if req.ClearCache {
		if info, statErr := os.Lstat(cacheDir); statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, fmt.Errorf("build cache path must be a real directory")
			}
			if err := os.RemoveAll(cacheDir); err != nil {
				return nil, fmt.Errorf("clear build cache: %w", err)
			}
		} else if !os.IsNotExist(statErr) {
			return nil, fmt.Errorf("inspect build cache before clearing: %w", statErr)
		}
	}
	if err := prepareBuildCache(dataDir, cacheDir, b.buildUser); err != nil {
		return nil, err
	}
	nodeCacheDir := ""
	if req.Runtime == "node" {
		nodeCacheDir, err = prepareNodeBuildCache(cacheDir, req.AppName, b.buildUser)
		if err != nil {
			return nil, err
		}
	}

	workspace, err := os.MkdirTemp("", "paas-build-")
	if err != nil {
		return nil, fmt.Errorf("create build workspace: %w", err)
	}
	defer removeTree(workspace)
	if err := os.Chmod(workspace, 0700); err != nil {
		return nil, fmt.Errorf("secure build workspace: %w", err)
	}

	sourceRoot := filepath.Join(workspace, "source")
	outputDir := filepath.Join(workspace, "out")
	homeDir := filepath.Join(workspace, "home")
	tempDir := filepath.Join(workspace, "tmp")
	for _, dir := range []string{sourceRoot, outputDir, homeDir, tempDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			return nil, fmt.Errorf("create private build directory: %w", err)
		}
	}

	sourceHash, err := copySource(req.SourceDir, sourceRoot)
	if err != nil {
		return nil, err
	}

	logFile, err := openBuildLog(req.LogPath)
	if err != nil {
		return nil, err
	}
	if logFile != nil {
		defer logFile.Close()
	}
	// nil gives exec a real /dev/null descriptor. io.Discard would introduce
	// copy pipes and let a background child keep Wait blocked after shell exit.
	var output io.Writer
	if logFile != nil {
		output = logFile
	}
	var redactedOutput *redactingWriter
	if output != nil {
		redactedOutput = newRedactingWriter(output, req.BuildEnvironment)
		output = redactedOutput
	}

	env := buildEnvironment(outputDir, homeDir, tempDir, cacheDir, nodeCacheDir)
	buildEnv, err := buildEnvironmentEntries(req.BuildEnvironment)
	if err != nil {
		return nil, err
	}
	env = append(env, buildEnv...)
	if os.Geteuid() == 0 {
		if err := assignTree(workspace, b.buildUser); err != nil {
			return nil, err
		}
	}
	buildCtx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	var buildErr error
	if os.Geteuid() == 0 {
		buildErr = runSystemd(buildCtx, b.buildUser, buildTimeout, sourceRoot, env, output, req.BuildCommand)
	} else {
		buildErr = runLocal(buildCtx, sourceRoot, env, output, req.BuildCommand)
	}
	if redactedOutput != nil {
		if flushErr := redactedOutput.Flush(); buildErr == nil && flushErr != nil {
			buildErr = flushErr
		}
	}
	if buildErr != nil {
		return nil, fmt.Errorf("build command failed: %w", buildErr)
	}
	if err := buildCtx.Err(); err != nil {
		return nil, fmt.Errorf("build command failed: %w", err)
	}

	outputInfo, err := os.Lstat(outputDir)
	if err != nil {
		return nil, fmt.Errorf("inspect build output directory: %w", err)
	}
	if !outputInfo.IsDir() {
		return nil, fmt.Errorf("PAAS_OUT must remain a real directory")
	}
	if req.Runtime == "node" {
		entries, _ := os.ReadDir(outputDir)
		if len(entries) == 0 {
			// Copy built sourceRoot into outputDir
			if err := copyDirectory(sourceRoot, outputDir, "", true, io.Discard); err != nil {
				return nil, fmt.Errorf("copy node build output: %w", err)
			}
		}
	} else {
		serverPath := filepath.Join(outputDir, "server")
		serverInfo, err := os.Lstat(serverPath)
		if err != nil {
			return nil, fmt.Errorf("build output is missing /app/server: %w", err)
		}
		if !serverInfo.Mode().IsRegular() || serverInfo.Mode()&0111 == 0 {
			return nil, fmt.Errorf("build output /app/server must be a regular executable file")
		}
	}

	return b.publish(dataDir, outputDir, req.Runtime, req.StartCommand, sourceHash)
}

func buildEnvironment(outputDir, homeDir, tempDir, cacheDir, nodeCacheDir string) []string {
	pathValue := "/usr/local/bin:/usr/local/go/bin:/usr/bin:/bin"
	if home, err := os.UserHomeDir(); err == nil {
		pathValue = filepath.Join(home, ".local", "bin") + ":" + pathValue
	}
	if inheritedPath := os.Getenv("PATH"); inheritedPath != "" {
		pathValue += ":" + inheritedPath
	}
	env := []string{
		"PATH=" + pathValue,
		"HOME=" + homeDir,
		"TMPDIR=" + tempDir,
		"PAAS_OUT=" + outputDir,
		"CGO_ENABLED=0",
		"GOFLAGS=-p=1",
		"GOMAXPROCS=1",
		"GOCACHE=" + filepath.Join(cacheDir, "go-build"),
		"GOMODCACHE=" + filepath.Join(cacheDir, "go", "pkg", "mod"),
	}
	if nodeCacheDir != "" {
		npmCache := filepath.Join(nodeCacheDir, "npm")
		pnpmCache := filepath.Join(nodeCacheDir, "pnpm")
		yarnCache := filepath.Join(nodeCacheDir, "yarn")
		env = append(env,
			"npm_config_cache="+npmCache,
			"npm_config_store_dir="+pnpmCache,
			"pnpm_config_store_dir="+pnpmCache,
			"YARN_CACHE_FOLDER="+yarnCache,
			"YARN_GLOBAL_FOLDER="+yarnCache,
		)
	}
	return env
}

var buildEnvironmentKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

var reservedBuildEnvironment = map[string]struct{}{
	"PATH": {}, "HOME": {}, "TMPDIR": {}, "PAAS_OUT": {}, "CGO_ENABLED": {},
	"GOFLAGS": {}, "GOMAXPROCS": {}, "GOCACHE": {}, "GOMODCACHE": {},
	"npm_config_cache": {}, "npm_config_store_dir": {}, "pnpm_config_store_dir": {},
	"YARN_CACHE_FOLDER": {}, "YARN_GLOBAL_FOLDER": {},
}

func buildEnvironmentEntries(entries []string) ([]string, error) {
	if len(entries) > 100 {
		return nil, fmt.Errorf("too many build environment values")
	}
	values := append([]string(nil), entries...)
	sort.Strings(values)
	seen := make(map[string]struct{}, len(values))
	for _, entry := range values {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !buildEnvironmentKey.MatchString(key) || strings.ContainsRune(value, '\x00') || len(value) > 65_536 {
			return nil, fmt.Errorf("invalid build environment value")
		}
		if _, reserved := reservedBuildEnvironment[key]; reserved {
			return nil, fmt.Errorf("build environment key %q is reserved", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("duplicate build environment key %q", key)
		}
		seen[key] = struct{}{}
	}
	return values, nil
}

type redactingWriter struct {
	mu      sync.Mutex
	dst     io.Writer
	secrets [][]byte
	buffer  []byte
	err     error
	maxLen  int
}

func newRedactingWriter(dst io.Writer, environment []string) *redactingWriter {
	if dst == nil {
		dst = io.Discard
	}
	w := &redactingWriter{dst: dst}
	for _, entry := range environment {
		_, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" {
			continue
		}
		w.secrets = append(w.secrets, []byte(value))
		if len(value) > w.maxLen {
			w.maxLen = len(value)
		}
	}
	sort.Slice(w.secrets, func(i, j int) bool { return len(w.secrets[i]) > len(w.secrets[j]) })
	return w
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	if len(w.secrets) == 0 {
		n, err := w.dst.Write(p)
		if err != nil {
			w.err = err
		}
		return n, err
	}
	w.buffer = append(w.buffer, p...)
	if err := w.drain(false); err != nil {
		w.err = err
		return 0, err
	}
	return len(p), nil
}

func (w *redactingWriter) drain(final bool) error {
	limit := len(w.buffer)
	if !final {
		limit = len(w.buffer) - w.maxLen + 1
		if limit <= 0 {
			return nil
		}
	}
	var output []byte
	consumed := 0
	for consumed < limit {
		matched := 0
		for _, secret := range w.secrets {
			if len(secret) <= len(w.buffer)-consumed && bytes.Equal(w.buffer[consumed:consumed+len(secret)], secret) {
				matched = len(secret)
				break
			}
		}
		if matched > 0 {
			output = append(output, []byte("[redacted]")...)
			consumed += matched
		} else {
			output = append(output, w.buffer[consumed])
			consumed++
		}
	}
	if len(output) != 0 {
		if _, err := w.dst.Write(output); err != nil {
			return err
		}
	}
	w.buffer = append(w.buffer[:0], w.buffer[consumed:]...)
	return nil
}

func (w *redactingWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if err := w.drain(true); err != nil {
		w.err = err
		return err
	}
	return nil
}

// prepareNodeBuildCache returns persistent per-application package-manager
// cache paths. Package managers create their own internal files under each
// directory, so the app tree is secured before the build command is launched.
func prepareNodeBuildCache(cacheDir, appName, buildUser string) (string, error) {
	if !config.ValidName(appName) {
		return "", fmt.Errorf("invalid application name %q", appName)
	}
	nodeDir := filepath.Join(cacheDir, "node")
	appDir := filepath.Join(nodeDir, appName)
	for _, dir := range []string{nodeDir, appDir} {
		if err := preparePrivateBuildDir(dir, buildUser); err != nil {
			return "", fmt.Errorf("prepare Node build cache: %w", err)
		}
	}
	for _, manager := range []string{"npm", "pnpm", "yarn"} {
		if err := preparePrivateBuildDir(filepath.Join(appDir, manager), buildUser); err != nil {
			return "", fmt.Errorf("prepare Node %s cache: %w", manager, err)
		}
	}
	return appDir, nil
}

func preparePrivateBuildDir(path, buildUser string) error {
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s must be a real directory", path)
	}
	if os.Geteuid() == 0 {
		uid, gid, err := unprivilegedUserIDs(buildUser)
		if err != nil {
			return err
		}
		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("assign %s to %s: %w", path, buildUser, err)
		}
	} else {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() {
			return fmt.Errorf("%s must be owned by the current user", path)
		}
	}
	if err := os.Chmod(path, 0700); err != nil {
		return fmt.Errorf("secure %s: %w", path, err)
	}
	return nil
}

func prepareBuildCache(dataDir, cacheDir, buildUser string) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("create builder data directory: %w", err)
	}
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return fmt.Errorf("create Go build cache: %w", err)
	}
	info, err := os.Lstat(cacheDir)
	if err != nil {
		return fmt.Errorf("inspect Go build cache: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("Go build cache path must be a real directory")
	}
	if os.Geteuid() == 0 {
		uid, gid, err := unprivilegedUserIDs(buildUser)
		if err != nil {
			return err
		}
		if err := os.Chown(cacheDir, uid, gid); err != nil {
			return fmt.Errorf("assign Go build cache to %s: %w", buildUser, err)
		}
	} else {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() {
			return fmt.Errorf("Go build cache must be owned by the current user")
		}
	}
	if err := os.Chmod(cacheDir, 0700); err != nil {
		return fmt.Errorf("secure Go build cache: %w", err)
	}
	return nil
}

func openBuildLog(path string) (*os.File, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create build log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, fmt.Errorf("open build log %s: %w", path, err)
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, fmt.Errorf("secure build log %s: %w", path, err)
	}
	return file, nil
}

func copySource(sourceDir, destination string) (string, error) {
	info, err := os.Lstat(sourceDir)
	if err != nil {
		return "", fmt.Errorf("inspect source directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("source path must be a real directory")
	}

	hash := sha256.New()
	if err := copyDirectory(sourceDir, destination, "", true, hash); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyDirectory(source, destination, relative string, excludePlatformEntries bool, hash io.Writer) error {
	rootInfo, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("inspect source directory %s: %w", source, err)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("source path %s must be a real directory", source)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return fmt.Errorf("read source directory %s: %w", source, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if excludePlatformEntries && (name == ".git" || name == "bin" || name == ".paas") {
			continue
		}
		sourcePath := filepath.Join(source, name)
		destinationPath := filepath.Join(destination, name)
		relPath := name
		if relative != "" {
			relPath = filepath.Join(relative, name)
		}
		info, err := os.Lstat(sourcePath)
		if err != nil {
			return fmt.Errorf("inspect source entry %s: %w", relPath, err)
		}
		switch {
		case info.IsDir():
			if err := os.Mkdir(destinationPath, 0700); err != nil {
				return fmt.Errorf("create source directory %s: %w", relPath, err)
			}
			if err := copyDirectory(sourcePath, destinationPath, relPath, false, hash); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyRegularFile(sourcePath, destinationPath, relPath, info, hash); err != nil {
				return err
			}
		default:
			return fmt.Errorf("source entry %s is a symlink or unsupported special file", relPath)
		}
	}
	return nil
}

func copyRegularFile(source, destination, relative string, info os.FileInfo, hash io.Writer) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open source file %s: %w", relative, err)
	}
	defer input.Close()
	openedInfo, err := input.Stat()
	if err != nil {
		return fmt.Errorf("stat source file %s: %w", relative, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return fmt.Errorf("source file %s changed while it was being copied", relative)
	}

	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create copied source file %s: %w", relative, err)
	}
	executable := info.Mode() & 0111
	if executable != 0 {
		if err := output.Chmod(0700); err != nil {
			output.Close()
			return fmt.Errorf("set copied source permissions %s: %w", relative, err)
		}
	}

	pathBytes := []byte(filepath.ToSlash(relative))
	var integer [8]byte
	binary.BigEndian.PutUint64(integer[:], uint64(len(pathBytes)))
	_, _ = hash.Write(integer[:])
	_, _ = hash.Write(pathBytes)
	binary.BigEndian.PutUint64(integer[:], uint64(info.Size()))
	_, _ = hash.Write(integer[:])
	_, _ = hash.Write([]byte{byte(executable >> 8), byte(executable)})

	written, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	closeErr := output.Close()
	if copyErr != nil {
		return fmt.Errorf("copy source file %s: %w", relative, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close copied source file %s: %w", relative, closeErr)
	}
	if written != info.Size() {
		return fmt.Errorf("source file %s changed size while it was being copied", relative)
	}
	return nil
}

func runLocal(ctx context.Context, directory string, env []string, output io.Writer, buildCommand string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	command := exec.Command("/bin/sh", "-c", buildCommand)
	command.Dir = directory
	command.Env = env
	command.Stdout = output
	command.Stderr = output
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start build shell: %w", err)
	}

	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	select {
	case err := <-finished:
		killProcessGroup(command.Process.Pid)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	case <-ctx.Done():
		killProcessGroup(command.Process.Pid)
		<-finished
		return ctx.Err()
	}
}

func killProcessGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func runSystemd(ctx context.Context, buildUser string, timeout time.Duration, directory string, env []string, output io.Writer, buildCommand string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("root builds require Linux systemd-run")
	}
	if _, _, err := unprivilegedUserIDs(buildUser); err != nil {
		return err
	}
	systemdRun, err := exec.LookPath("systemd-run")
	if err != nil {
		return fmt.Errorf("find systemd-run: %w", err)
	}
	unitSuffix := make([]byte, 8)
	if _, err := rand.Read(unitSuffix); err != nil {
		return fmt.Errorf("generate build unit name: %w", err)
	}
	unit := "paas-build-" + hex.EncodeToString(unitSuffix) + ".service"
	args := []string{
		"--wait", "--pipe", "--collect",
		"--unit=" + unit,
		"--property=User=" + buildUser,
		"--property=WorkingDirectory=" + directory,
		"--property=RuntimeMaxSec=" + strconv.FormatInt(timeout.Milliseconds(), 10) + "ms",
		"--property=MemoryMax=512M",
		"--property=MemorySwapMax=0",
		"--property=TasksMax=128",
		"--property=CPUQuota=100%",
		"--property=KillMode=control-group",
	}
	for _, value := range env {
		args = append(args, "--setenv="+value)
	}
	args = append(args, "/bin/sh", "-c", buildCommand)
	command := exec.CommandContext(ctx, systemdRun, args...)
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			stopBuildUnit(unit)
			return ctx.Err()
		}
		return fmt.Errorf("systemd-run: %w", err)
	}
	return nil
}

func stopBuildUnit(unit string) {
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = exec.CommandContext(stopCtx, "systemctl", "kill", "--kill-who=all", "--signal=KILL", unit).Run()
	_ = exec.CommandContext(stopCtx, "systemctl", "stop", unit).Run()
}

func assignTree(root, buildUser string) error {
	uid, gid, err := unprivilegedUserIDs(buildUser)
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unexpected symlink in private build workspace: %s", path)
		}
		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("assign build workspace to %s: %w", buildUser, err)
		}
		return nil
	})
}

func unprivilegedUserIDs(name string) (int, int, error) {
	identity, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("lookup build user %q: %w", name, err)
	}
	uid, err := strconv.Atoi(identity.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse build user uid: %w", err)
	}
	if uid == 0 {
		return 0, 0, fmt.Errorf("build user %q must not be root", name)
	}
	gid, err := strconv.Atoi(identity.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse build user gid: %w", err)
	}
	return uid, gid, nil
}

func (b *GoBuilder) publish(dataDir, outputDir, runtimeName, startCommand, sourceHash string) (*contracts.Release, error) {
	releasesDir := filepath.Join(dataDir, "releases")
	if err := os.MkdirAll(releasesDir, 0755); err != nil {
		return nil, fmt.Errorf("create releases directory: %w", err)
	}
	if err := os.Chmod(releasesDir, 0755); err != nil {
		return nil, fmt.Errorf("set releases directory permissions: %w", err)
	}

	idBytes := make([]byte, 6)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, fmt.Errorf("generate release ID: %w", err)
	}
	id := "rel_" + hex.EncodeToString(idBytes)
	createdAt := time.Now().UTC()
	artifactDir := filepath.Join(releasesDir, id, "app")
	command := []string{filepath.Join(artifactDir, "server")}
	if runtimeName == "node" {
		command = []string{"/bin/sh", "-c", "exec " + startCommand}
	}

	stagingDir, err := os.MkdirTemp(releasesDir, ".staging-")
	if err != nil {
		return nil, fmt.Errorf("create release staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			removeTree(stagingDir)
		}
	}()
	appDir := filepath.Join(stagingDir, "app")
	if err := os.Mkdir(appDir, 0700); err != nil {
		return nil, fmt.Errorf("create release app directory: %w", err)
	}
	if err := copyDirectory(outputDir, appDir, "", false, io.Discard); err != nil {
		return nil, fmt.Errorf("copy build output into release: %w", err)
	}

	manifest := releaseManifest{
		ID:          id,
		SourceHash:  sourceHash,
		Runtime:     runtimeName,
		ArtifactDir: artifactDir,
		Command:     command,
		CreatedAt:   createdAt,
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode release manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := os.WriteFile(filepath.Join(stagingDir, "manifest.json"), manifestBytes, 0600); err != nil {
		return nil, fmt.Errorf("write release manifest: %w", err)
	}
	if runtimeName == "go" {
		if err := os.Chmod(filepath.Join(appDir, "server"), 0555); err != nil {
			return nil, fmt.Errorf("make release executable immutable: %w", err)
		}
	}
	if err := freezeRelease(stagingDir); err != nil {
		return nil, err
	}
	finalDir := filepath.Join(releasesDir, id)
	if err := os.Rename(stagingDir, finalDir); err != nil {
		return nil, fmt.Errorf("publish release atomically: %w", err)
	}
	published = true

	return &contracts.Release{
		ID:          id,
		SourceHash:  sourceHash,
		Runtime:     runtimeName,
		ArtifactDir: filepath.Join(finalDir, "app"),
		Command:     command,
		CreatedAt:   createdAt,
	}, nil
}

func freezeRelease(root string) error {
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("release contains unexpected symlink: %s", path)
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return fmt.Errorf("inspect staged release: %w", err)
	}
	for i := len(paths) - 1; i >= 0; i-- {
		path := paths[i]
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect staged release entry %s: %w", path, err)
		}
		if info.IsDir() {
			if err := os.Chmod(path, 0555); err != nil {
				return fmt.Errorf("make release directory immutable: %w", err)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("release contains unsupported special file: %s", path)
		}
		mode := os.FileMode(0444)
		if info.Mode()&0111 != 0 {
			mode = 0555
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("make release file immutable: %w", err)
		}
	}
	return nil
}

func removeTree(path string) {
	if path == "" {
		return
	}
	_ = filepath.WalkDir(path, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(current, 0700)
		} else {
			_ = os.Chmod(current, 0600)
		}
		return nil
	})
	_ = os.RemoveAll(path)
}

var _ contracts.Builder = (*GoBuilder)(nil)
