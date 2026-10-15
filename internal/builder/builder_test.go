package builder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"paas/internal/config"
	"paas/internal/contracts"
	"paas/internal/fsutil"
	"paas/internal/sandbox"
)

func TestRedactingWriterHandlesSecretsAcrossWrites(t *testing.T) {
	var output bytes.Buffer
	writer := newRedactingWriter(&output, []string{"PRIVATE_TOKEN=redact-this-token", "SHORT=redact"})
	if _, err := writer.Write([]byte("token=redact-this-")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("token finished")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "redact-this-token") || !strings.Contains(output.String(), "[redacted]") {
		t.Fatalf("secret was not redacted from streaming output: %q", output.String())
	}
	var overlap bytes.Buffer
	writer = newRedactingWriter(&overlap, []string{"A=abc", "B=abcdef"})
	_, _ = writer.Write([]byte("abcdef"))
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if overlap.String() != "[redacted]" {
		t.Fatalf("overlapping secret was partially exposed: %q", overlap.String())
	}
}

func TestPruneCacheHonorsContextWhileWaitingForBuild(t *testing.T) {
	b := New(Options{DataDir: t.TempDir()})
	b.cacheMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := b.PruneCache(ctx)
	b.cacheMu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PruneCache error = %v, want context deadline exceeded", err)
	}
}

func TestBuildTimeoutCoversWaitingForBuildCache(t *testing.T) {
	if os.Geteuid() == 0 && runtime.GOOS != "linux" {
		t.Skip("root builds require Linux systemd-run")
	}
	root := t.TempDir()
	source := writeGoFixture(t, filepath.Join(root, "source"))
	b := New(Options{DataDir: filepath.Join(root, "data"), Timeout: 20 * time.Millisecond})
	b.cacheMu.Lock()
	locked := true
	defer func() {
		if locked {
			b.cacheMu.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := b.Build(context.Background(), fixtureRequest(source, ""))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Build while waiting for cache = %v, want context deadline exceeded", err)
		}
	case <-time.After(200 * time.Millisecond):
		b.cacheMu.Unlock()
		locked = false
		select {
		case err := <-done:
			t.Fatalf("Build did not apply its timeout while waiting for cache; after release it returned %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("Build remained stuck after the cache lock was released")
		}
	}
	b.cacheMu.Unlock()
	locked = false
}

func TestBuildPublishesDeterministicImmutableReleases(t *testing.T) {
	skipRootBuild(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	defer removeTree(dataDir)
	sourceA := writeGoFixture(t, filepath.Join(root, "source-a"))
	sourceB := writeGoFixture(t, filepath.Join(root, "source-b"))
	for _, source := range []string{sourceA, sourceB} {
		for _, ignored := range []string{".git", "bin", ".paas"} {
			if err := os.Mkdir(filepath.Join(source, ignored), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, ignored, "ignored.txt"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}

	b := New(Options{DataDir: dataDir, Timeout: 5 * time.Minute})
	firstLog := filepath.Join(root, "logs", "first.log")
	first, err := b.Build(context.Background(), fixtureRequest(sourceA, firstLog))
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, err := b.Build(context.Background(), fixtureRequest(sourceB, filepath.Join(root, "second.log")))
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if first.SourceHash != second.SourceHash {
		t.Fatalf("same copied source produced different hashes: %s != %s", first.SourceHash, second.SourceHash)
	}
	if first.ID == second.ID {
		t.Fatalf("separate builds reused release ID %q", first.ID)
	}
	if !strings.HasPrefix(first.ID, "rel_") || first.Runtime != "go" {
		t.Fatalf("unexpected release identity: %+v", first)
	}
	if first.ArtifactDir != filepath.Join(dataDir, "releases", first.ID, "app") {
		t.Fatalf("artifact directory is not release-local: %q", first.ArtifactDir)
	}
	if len(first.Command) != 1 || first.Command[0] != filepath.Join(first.ArtifactDir, "server") {
		t.Fatalf("release command is not a direct executable: %#v", first.Command)
	}
	args, err := sandbox.BuildBwrapArgs(contracts.SandboxSpec{ID: "release-contract", Command: first.Command, Dir: first.ArtifactDir})
	if err != nil || args[len(args)-1] != "/app/server" {
		t.Fatalf("release cannot start in Bubblewrap: %v %v", args, err)
	}

	server := filepath.Join(first.ArtifactDir, "server")
	info, err := os.Stat(server)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0555 {
		t.Fatalf("release executable mode = %04o, want 0555", info.Mode().Perm())
	}
	appInfo, err := os.Stat(first.ArtifactDir)
	if err != nil {
		t.Fatal(err)
	}
	if appInfo.Mode().Perm() != 0555 {
		t.Fatalf("release app directory mode = %04o, want 0555", appInfo.Mode().Perm())
	}
	assetInfo, err := os.Stat(filepath.Join(first.ArtifactDir, "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if assetInfo.Mode().Perm() != 0444 {
		t.Fatalf("release data file mode = %04o, want 0444", assetInfo.Mode().Perm())
	}
	if os.Geteuid() != 0 {
		if err := os.WriteFile(filepath.Join(first.ArtifactDir, "mutation"), []byte("no"), 0600); err == nil {
			t.Fatal("release app directory accepted a write")
		}
	}
	manifestBytes, err := os.ReadFile(filepath.Join(filepath.Dir(first.ArtifactDir), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest releaseManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	manifestInfo, err := os.Stat(filepath.Join(filepath.Dir(first.ArtifactDir), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if manifestInfo.Mode().Perm() != 0444 {
		t.Fatalf("manifest mode = %04o, want 0444", manifestInfo.Mode().Perm())
	}
	if manifest.ID != first.ID || manifest.SourceHash != first.SourceHash || manifest.ArtifactDir != first.ArtifactDir {
		t.Fatalf("manifest does not describe returned release: %+v", manifest)
	}

	if err := os.RemoveAll(sourceA); err != nil {
		t.Fatal(err)
	}
	result, err := exec.Command(server).CombinedOutput()
	if err != nil {
		t.Fatalf("published artifact failed after source removal: %v: %s", err, result)
	}
	if string(result) != "fixture\n" {
		t.Fatalf("published artifact output = %q", result)
	}
	if data, err := os.ReadFile(firstLog); err != nil {
		t.Fatal(err)
	} else if len(data) == 0 {
		t.Fatal("build output was not written to LogPath")
	}
}

func TestBuildFailureIsLoggedAndDoesNotPublish(t *testing.T) {
	skipRootBuild(t)
	root := t.TempDir()
	source := writeGoFixture(t, filepath.Join(root, "source"))
	dataDir := filepath.Join(root, "data")
	logPath := filepath.Join(root, "build.log")
	request := fixtureRequest(source, logPath)
	request.BuildCommand = "printf 'intentional failure\\n' >&2; exit 7"

	_, err := New(Options{DataDir: dataDir}).Build(context.Background(), request)
	if err == nil {
		t.Fatal("Build succeeded for a failing command")
	}
	contents, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(contents), "intentional failure") {
		t.Fatalf("build log does not contain stderr: %q", contents)
	}
	entries, readErr := os.ReadDir(filepath.Join(dataDir, "releases"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed build published release entries: %v", entries)
	}
}

func TestHelloExampleBuildsFromItsOwnSourceDirectory(t *testing.T) {
	skipRootBuild(t)
	source := filepath.Join("..", "..", "examples", "hello-go")
	cfg, err := config.Load(source)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	defer removeTree(dataDir)
	release, err := New(Options{DataDir: dataDir}).Build(context.Background(), contracts.BuildRequest{
		SourceDir:    source,
		AppName:      cfg.Name,
		Runtime:      cfg.Runtime,
		BuildCommand: cfg.Build.Command,
		StartCommand: cfg.Start.Command,
	})
	if err != nil {
		t.Fatalf("build example without its parent module: %v", err)
	}
	if _, err := os.Stat(filepath.Join(release.ArtifactDir, "server")); err != nil {
		t.Fatal(err)
	}
}

func TestBrokenBuildExampleFailsAndLogsItsOutput(t *testing.T) {
	skipRootBuild(t)
	source := filepath.Join("..", "..", "examples", "broken-build")
	cfg, err := config.Load(source)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	logPath := filepath.Join(root, "broken.log")
	_, err = New(Options{DataDir: filepath.Join(root, "data")}).Build(context.Background(), contracts.BuildRequest{
		SourceDir:    source,
		AppName:      cfg.Name,
		Runtime:      cfg.Runtime,
		BuildCommand: cfg.Build.Command,
		StartCommand: cfg.Start.Command,
		LogPath:      logPath,
	})
	if err == nil {
		t.Fatal("broken build fixture unexpectedly succeeded")
	}
	logBytes, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(logBytes), "intentional broken-build fixture") {
		t.Fatalf("broken build output missing from log: %q", logBytes)
	}
}

func TestBuildTimeoutKillsDescendants(t *testing.T) {
	skipRootBuild(t)
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is not available")
	}
	root := t.TempDir()
	source := writeGoFixture(t, filepath.Join(root, "source"))
	pidFile := filepath.Join(root, "child.pid")
	command := "sleep 30 & child=$!; printf '%s' \"$child\" > " + shellQuote(pidFile) + "; wait \"$child\""
	request := fixtureRequest(source, "")
	request.BuildCommand = command

	started := time.Now()
	_, err := New(Options{DataDir: filepath.Join(root, "data"), Timeout: 500 * time.Millisecond}).Build(context.Background(), request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Build error = %v, want context deadline exceeded", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatalf("timed build took too long: %s", time.Since(started))
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatalf("invalid child pid %q: %v", pidBytes, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) || isZombie(pid) {
			return
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			t.Fatalf("check child process: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("build descendant %d survived the timeout", pid)
}

func TestBuildWithoutLogsDoesNotWaitForBackgroundOutput(t *testing.T) {
	skipRootBuild(t)
	root := t.TempDir()
	data := filepath.Join(root, "data")
	defer removeTree(data)
	request := fixtureRequest(writeGoFixture(t, filepath.Join(root, "source")), "")
	request.BuildCommand = `sleep 30 & printf '#!/bin/sh\nexit 0\n' > "$PAAS_OUT/server"; chmod +x "$PAAS_OUT/server"`
	if _, err := New(Options{DataDir: data, Timeout: time.Second}).Build(context.Background(), request); err != nil {
		t.Fatalf("successful shell waited for background output: %v", err)
	}
}

func TestBuildRejectsSymlinksAndUnsafeStartPaths(t *testing.T) {
	skipRootBuild(t)
	root := t.TempDir()
	source := writeGoFixture(t, filepath.Join(root, "source"))
	if err := os.Symlink("main.go", filepath.Join(source, "linked.go")); err != nil {
		t.Fatal(err)
	}
	request := fixtureRequest(source, "")
	_, err := New(Options{DataDir: filepath.Join(root, "data")}).Build(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink source error = %v", err)
	}

	if err := os.Remove(filepath.Join(source, "linked.go")); err != nil {
		t.Fatal(err)
	}
	request.StartCommand = "/app/../etc/passwd"
	_, err = New(Options{DataDir: filepath.Join(root, "data")}).Build(context.Background(), request)
	if err == nil {
		t.Fatal("Build accepted a start path outside /app/server")
	}
}

func TestBuildRejectsOutputSymlinkAndNonExecutableServer(t *testing.T) {
	skipRootBuild(t)
	root := t.TempDir()
	source := writeGoFixture(t, filepath.Join(root, "source"))
	b := New(Options{DataDir: filepath.Join(root, "data")})
	request := fixtureRequest(source, "")
	request.BuildCommand = "printf data > \"$PAAS_OUT/server\""
	if _, err := b.Build(context.Background(), request); err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("non-executable server error = %v", err)
	}

	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	request.BuildCommand = "rmdir \"$PAAS_OUT\"; ln -s " + shellQuote(outside) + " \"$PAAS_OUT\""
	if _, err := b.Build(context.Background(), request); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("output symlink error = %v", err)
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("outside directory was unexpectedly changed: info=%v err=%v", info, err)
	}
}

func TestCopySourceHashIncludesExecutableMode(t *testing.T) {
	root := t.TempDir()
	sourceA := t.TempDir()
	sourceB := t.TempDir()
	for _, source := range []string{sourceA, sourceB} {
		if err := os.WriteFile(filepath.Join(source, "tool"), []byte("same"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(sourceB, "tool"), 0700); err != nil {
		t.Fatal(err)
	}
	destinationA := filepath.Join(root, "a")
	destinationB := filepath.Join(root, "b")
	if err := os.Mkdir(destinationA, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destinationB, 0700); err != nil {
		t.Fatal(err)
	}
	hashA, err := copySource(sourceA, destinationA)
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := copySource(sourceB, destinationB)
	if err != nil {
		t.Fatal(err)
	}
	if hashA == hashB {
		t.Fatal("source hash did not include executable mode")
	}
}

func TestCopySourceHashIncludesEmptyDirectories(t *testing.T) {
	root := t.TempDir()
	sourceA := t.TempDir()
	sourceB := t.TempDir()
	if err := os.Mkdir(filepath.Join(sourceB, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	destinationA := filepath.Join(root, "a")
	destinationB := filepath.Join(root, "b")
	if err := os.Mkdir(destinationA, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destinationB, 0700); err != nil {
		t.Fatal(err)
	}
	hashA, err := copySource(sourceA, destinationA)
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := copySource(sourceB, destinationB)
	if err != nil {
		t.Fatal(err)
	}
	if hashA == hashB {
		t.Fatal("source hash did not include an empty directory")
	}
}

type cancelOnWrite struct{ cancel context.CancelFunc }

func (w cancelOnWrite) Write(value []byte) (int, error) {
	w.cancel()
	return len(value), nil
}

func TestCopyRegularFileStopsWhenContextIsCanceledMidCopy(t *testing.T) {
	source := filepath.Join(t.TempDir(), "large-file")
	if err := os.WriteFile(source, bytes.Repeat([]byte("x"), 1<<20), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(source)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "copy")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = copyRegularFileContext(ctx, source, destination, "large-file", info, cancelOnWrite{cancel: cancel})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copyRegularFileContext() error = %v, want cancellation", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled copy left a partial destination: %v", err)
	}
}

func fixtureRequest(source, logPath string) contracts.BuildRequest {
	return contracts.BuildRequest{
		SourceDir:    source,
		AppName:      "fixture",
		Runtime:      "go",
		BuildCommand: `printf 'asset\n' > "$PAAS_OUT/data.txt"; printf 'building fixture\n'; go build -o "$PAAS_OUT/server" .`,
		StartCommand: "/app/server",
		LogPath:      logPath,
	}
}

func writeGoFixture(t *testing.T, directory string) string {
	t.Helper()
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module fixture\n\ngo 1.23\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"fixture\") }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return directory
}

func skipRootBuild(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root builds must run through the configured systemd build user")
	}
}

func TestBuildUserMustNotBeRoot(t *testing.T) {
	if _, _, err := unprivilegedUserIDs("root"); err == nil {
		t.Fatal("root was accepted as a build user")
	}
}

func TestNodeBuildPublishesRelease(t *testing.T) {
	skipRootBuild(t)
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	if err := os.MkdirAll(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "package.json"), []byte(`{"name":"test-node","version":"1.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "server.js"), []byte(`console.log("hello node")`), 0600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, "data")
	defer removeTree(dataDir)
	b := New(Options{DataDir: dataDir})
	release, err := b.Build(context.Background(), contracts.BuildRequest{
		SourceDir:    sourceDir,
		AppName:      "test-node",
		Runtime:      "node",
		BuildCommand: "echo 'node build step'",
		StartCommand: "node /app/server.js",
	})
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if release.Runtime != "node" {
		t.Fatalf("unexpected runtime %q", release.Runtime)
	}
	if _, err := os.Stat(filepath.Join(release.ArtifactDir, "server.js")); err != nil {
		t.Fatalf("server.js not found in artifact dir: %v", err)
	}
}

func TestNodeBuildKeepsRootBinDirectoryInFallbackArtifact(t *testing.T) {
	skipRootBuild(t)
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(sourceDir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "package.json"), []byte(`{"name":"bin-entrypoint","version":"1.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join(sourceDir, "bin", "www")
	if err := os.WriteFile(entrypoint, []byte("console.log('bin entrypoint')\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, "data")
	defer removeTree(dataDir)
	release, err := New(Options{DataDir: dataDir}).Build(context.Background(), contracts.BuildRequest{
		SourceDir: sourceDir, AppName: "bin-entrypoint", Runtime: "node",
		BuildCommand: "test -f bin/www", StartCommand: "node /app/bin/www",
	})
	if err != nil {
		t.Fatalf("Node build with a root bin/ entrypoint failed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(release.ArtifactDir, "bin", "www")); err != nil || string(data) != "console.log('bin entrypoint')\n" {
		t.Fatalf("fallback artifact omitted Node's bin/ entrypoint: data=%q err=%v", data, err)
	}
}

func TestNodeBuildPublishesInternalPackageManagerSymlinks(t *testing.T) {
	skipRootBuild(t)
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(sourceDir, "node_modules"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "package.json"), []byte(`{"name":"linked-package","version":"1.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, "data")
	defer removeTree(dataDir)
	buildCommand := `set -eu; mkdir -p node_modules/.bin node_modules/pkg; printf '#!/bin/sh\nprintf linked\n' > node_modules/pkg/cli.js; chmod +x node_modules/pkg/cli.js; ln -s ../pkg/cli.js node_modules/.bin/tool`
	release, err := New(Options{DataDir: dataDir}).Build(context.Background(), contracts.BuildRequest{
		SourceDir: sourceDir, AppName: "linked-package", Runtime: "node",
		BuildCommand: buildCommand, StartCommand: "node /app/node_modules/.bin/tool",
	})
	if err != nil {
		t.Fatalf("Node build with an internal package-manager symlink failed: %v", err)
	}
	link := filepath.Join(release.ArtifactDir, "node_modules", ".bin", "tool")
	if target, err := os.Readlink(link); err != nil || target != "../pkg/cli.js" {
		t.Fatalf("published package-manager link target = %q, err=%v", target, err)
	}
	output, err := exec.Command(link).CombinedOutput()
	if err != nil || string(output) != "linked" {
		t.Fatalf("published package-manager link did not run its internal target: output=%q err=%v", output, err)
	}
}

func TestNodeBuildRejectsExternalAndCyclicDependencySymlinks(t *testing.T) {
	skipRootBuild(t)
	for _, test := range []struct {
		name    string
		command string
		want    string
	}{
		{name: "external", command: "ln -s /etc/passwd node_modules/escape", want: "absolute target"},
		{name: "cycle", command: "ln -s second node_modules/first; ln -s first node_modules/second", want: "cyclic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			sourceDir := filepath.Join(root, "source")
			if err := os.MkdirAll(filepath.Join(sourceDir, "node_modules"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sourceDir, "package.json"), []byte(`{"name":"unsafe-links","version":"1.0.0"}`), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := New(Options{DataDir: filepath.Join(root, "data")}).Build(context.Background(), contracts.BuildRequest{
				SourceDir: sourceDir, AppName: "unsafe-links", Runtime: "node",
				BuildCommand: test.command, StartCommand: "node /app/server.js",
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Build() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateArtifactSymlinksRejectsExternalAndCyclicTargets(t *testing.T) {
	for _, test := range []struct {
		name  string
		links map[string]string
		want  string
	}{
		{name: "external", links: map[string]string{"escape": "../outside"}, want: "escapes the release"},
		{name: "cycle", links: map[string]string{"first": "second", "second": "first"}, want: "dangling or cyclic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "artifact")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			if test.name == "external" {
				if err := os.WriteFile(filepath.Join(base, "outside"), []byte("outside"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for name, target := range test.links {
				if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
			}
			if err := validateArtifactSymlinks(root); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateArtifactSymlinks() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestNodePackageCachesPersistPerApplication(t *testing.T) {
	skipRootBuild(t)
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
	if err := os.MkdirAll(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "package.json"), []byte(`{"name":"test-node","version":"1.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, "data")
	defer removeTree(dataDir)
	b := New(Options{DataDir: dataDir})
	build := func(appName, command string) *contracts.Release {
		t.Helper()
		release, err := b.Build(context.Background(), contracts.BuildRequest{
			SourceDir: sourceDir, AppName: appName, Runtime: "node", BuildCommand: command, StartCommand: "node /app/server.js",
		})
		if err != nil {
			t.Fatalf("Build(%s) failed: %v", appName, err)
		}
		return release
	}
	seedCaches := `set -eu; test "$npm_config_store_dir" = "$pnpm_config_store_dir"; test "$YARN_CACHE_FOLDER" = "$YARN_GLOBAL_FOLDER"; for cache in "$npm_config_cache" "$pnpm_config_store_dir" "$YARN_GLOBAL_FOLDER"; do test -d "$cache"; touch "$cache/marker"; done; printf 'ok' > "$PAAS_OUT/result"`
	build("test-node", seedCaches)
	for _, dir := range []string{"npm", "pnpm", "yarn"} {
		info, err := os.Stat(filepath.Join(dataDir, "build-cache", "node", "test-node", dir))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("Node %s cache permissions = %v, err=%v; want 0700", dir, info, err)
		}
	}

	verifyCaches := `set -eu; test "$npm_config_store_dir" = "$pnpm_config_store_dir"; test "$YARN_CACHE_FOLDER" = "$YARN_GLOBAL_FOLDER"; for cache in "$npm_config_cache" "$pnpm_config_store_dir" "$YARN_GLOBAL_FOLDER"; do test -f "$cache/marker"; done; printf 'reused' > "$PAAS_OUT/result"`
	second := build("test-node", verifyCaches)
	if data, err := os.ReadFile(filepath.Join(second.ArtifactDir, "result")); err != nil || string(data) != "reused" {
		t.Fatalf("second build did not reuse Node caches: data=%q err=%v", data, err)
	}

	otherApp := `set -eu; test "$npm_config_store_dir" = "$pnpm_config_store_dir"; test "$YARN_CACHE_FOLDER" = "$YARN_GLOBAL_FOLDER"; for cache in "$npm_config_cache" "$pnpm_config_store_dir" "$YARN_GLOBAL_FOLDER"; do test ! -f "$cache/marker"; touch "$cache/marker"; done; printf 'separate' > "$PAAS_OUT/result"`
	build("other-node", otherApp)
}

func TestNodeBuildCacheRejectsSymlinkedManagerCache(t *testing.T) {
	cacheRoot := t.TempDir()
	if os.Geteuid() == 0 {
		// The production root path requires the configured unprivileged account;
		// the path validation itself is checked before user lookup for symlinks.
		if err := os.Mkdir(filepath.Join(cacheRoot, "node"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), filepath.Join(cacheRoot, "node", "unsafe")); err != nil {
			t.Fatal(err)
		}
		if err := preparePrivateBuildDir(filepath.Join(cacheRoot, "node", "unsafe"), "paas-build"); err == nil || !strings.Contains(err.Error(), "real directory") {
			t.Fatalf("symlinked app cache error = %v", err)
		}
		return
	}
	appCache, err := prepareNodeBuildCache(cacheRoot, "safe", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(appCache, "npm")); err != nil {
		t.Fatalf("npm cache was not created: %v", err)
	}
	if err := os.Remove(filepath.Join(appCache, "pnpm")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(appCache, "pnpm")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareNodeBuildCache(cacheRoot, "safe", ""); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlinked package-manager cache error = %v", err)
	}
}

func isZombie(pid int) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data))
	return len(fields) > 2 && fields[2] == "Z"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestSelectedBuildEnvironmentIsInjectedAndRedactedInActualBuild(t *testing.T) {
	skipRootBuild(t)
	dir := t.TempDir()
	t.Cleanup(func() {
		if err := fsutil.RemoveReadonlyTree(dir); err != nil {
			t.Errorf("clean immutable fixture: %v", err)
		}
	})
	sourceDir := filepath.Join(dir, "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "fixture.txt"), []byte("build environment fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted := "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "'"
	logPath := filepath.Join(dir, "build.log")
	b := New(Options{DataDir: filepath.Join(dir, "data")})
	_, err = b.Build(context.Background(), contracts.BuildRequest{AppName: "environment-probe", SourceDir: sourceDir, Runtime: "go", BuildCommand: "printf '%s\\n' \"$BUILD_TOKEN\"; cp " + quoted + " \"$PAAS_OUT/server\"; chmod 755 \"$PAAS_OUT/server\"", StartCommand: "/app/server", LogPath: logPath, BuildEnvironment: []string{"BUILD_TOKEN=synthetic-selected-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "synthetic-selected-secret") || !strings.Contains(string(data), "[redacted]") {
		t.Fatalf("unsafe or missing injected build output: %s", data)
	}
}
