package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveReadonlyTreeDoesNotTouchSymlinkTargets(t *testing.T) {
	outside := t.TempDir()
	protected := filepath.Join(outside, "keep")
	if err := os.WriteFile(protected, []byte("keep"), 0400); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "managed")
	nested := filepath.Join(root, "module")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "file"), []byte("remove"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0555); err != nil {
		t.Fatal(err)
	}
	if err := RemoveReadonlyTree(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("managed tree survived")
	}
	data, err := os.ReadFile(protected)
	if err != nil || string(data) != "keep" {
		t.Fatalf("linked target changed: %s %v", data, err)
	}
	info, err := os.Stat(protected)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatal("linked target permissions changed")
	}
}
