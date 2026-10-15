package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsurePolicyJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := EnsurePolicyJSON(); err != nil {
		t.Fatalf("EnsurePolicyJSON failed: %v", err)
	}

	home, _ := os.UserHomeDir()
	policyPath := filepath.Join(home, ".config", "containers", "policy.json")
	if _, err := os.Stat(policyPath); err != nil {
		t.Fatalf("policy.json does not exist: %v", err)
	}
}

func TestEnsurePolicyJSONRejectsNonFilePolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	policyPath := filepath.Join(home, ".config", "containers", "policy.json")
	if err := os.MkdirAll(policyPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePolicyJSON(); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("EnsurePolicyJSON() error = %v, want non-file policy error", err)
	}
}

func TestFetchAndUnpackRejectsStaleCacheAndIncompleteMountpoints(t *testing.T) {
	target := t.TempDir()
	setupFakeOCI(t, "valid", "")
	if err := FetchAndUnpack(context.Background(), "registry.example/old:v1", target); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"data", "app", "tmp"} {
		info, err := os.Lstat(filepath.Join(target, "rootfs", name))
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			t.Fatalf("mountpoint %q = %v, %v; want a real directory", name, info, err)
		}
	}
	calls, err := os.ReadFile(os.Getenv("FAKE_UMOCI_CALLS"))
	if err != nil || string(calls) != "x" {
		t.Fatalf("initial umoci calls = %q, %v", calls, err)
	}
	if err := FetchAndUnpack(context.Background(), "registry.example/new:v1", target); err == nil || !strings.Contains(err.Error(), "not requested image") {
		t.Fatalf("stale cache error = %v, want image mismatch", err)
	}
	calls, err = os.ReadFile(os.Getenv("FAKE_UMOCI_CALLS"))
	if err != nil || string(calls) != "x" {
		t.Fatalf("mismatched cache ran umoci again: calls=%q err=%v", calls, err)
	}

	// A cache built by an older/incomplete runtime still needs all mountpoints.
	// Restore a missing safe directory in place, but reject a symlinked one.
	appPath := filepath.Join(target, "rootfs", "app")
	if err := os.Remove(appPath); err != nil {
		t.Fatal(err)
	}
	if err := FetchAndUnpack(context.Background(), "registry.example/old:v1", target); err != nil {
		t.Fatalf("repair cached missing mountpoint: %v", err)
	}
	if info, err := os.Lstat(appPath); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("repaired app mountpoint = %v, %v; want real directory", info, err)
	}
	tmpPath := filepath.Join(target, "rootfs", "tmp")
	if err := os.Remove(tmpPath); err != nil {
		t.Fatal(err)
	}
	outsideCache := filepath.Join(t.TempDir(), "outside")
	if err := os.Symlink(outsideCache, tmpPath); err != nil {
		t.Fatal(err)
	}
	if err := FetchAndUnpack(context.Background(), "registry.example/old:v1", target); err == nil || !strings.Contains(err.Error(), "cached rootfs mount point") {
		t.Fatalf("symlinked cached mountpoint error = %v, want failure", err)
	}
	if _, err := os.Lstat(outsideCache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cached symlink unexpectedly created external path: %v", err)
	}

	unsafeTarget := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside", "new")
	setupFakeOCI(t, "data-symlink", outside)
	if err := FetchAndUnpack(context.Background(), "registry.example/linked:v1", unsafeTarget); err == nil || !strings.Contains(err.Error(), "mount point") {
		t.Fatalf("symlink mountpoint error = %v, want failure", err)
	}
	if _, err := os.Lstat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unpack unexpectedly created external path: %v", err)
	}
}

func TestFetchAndUnpackReturnsMetadataWriteFailure(t *testing.T) {
	target := t.TempDir()
	if err := os.Mkdir(filepath.Join(target, "runtime.json"), 0700); err != nil {
		t.Fatal(err)
	}
	setupFakeOCI(t, "valid", "")
	if err := FetchAndUnpack(context.Background(), "registry.example/test:v1", target); err == nil || !strings.Contains(err.Error(), "publish runtime metadata") {
		t.Fatalf("FetchAndUnpack() error = %v, want metadata publication failure", err)
	}
}

func setupFakeOCI(t *testing.T, mode, outside string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	binDir := t.TempDir()
	callsPath := filepath.Join(t.TempDir(), "umoci-calls")
	t.Setenv("FAKE_UNPACK_MODE", mode)
	t.Setenv("FAKE_OUTSIDE", outside)
	t.Setenv("FAKE_UMOCI_CALLS", callsPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for name, script := range map[string]string{
		"skopeo": "#!/bin/sh\nset -eu\nexit 0\n",
		"umoci": `#!/bin/sh
set -eu
printf x >> "$FAKE_UMOCI_CALLS"
target="$5"
mkdir -p "$target/rootfs"
if [ "$FAKE_UNPACK_MODE" = "data-symlink" ]; then
  ln -s "$FAKE_OUTSIDE" "$target/rootfs/data"
fi
`,
	} {
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
}
