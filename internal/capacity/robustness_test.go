package capacity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorageRootMustBeAnActualDirectory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	writeFixture(t, file, "not a data directory")
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, link} {
		total, _, _, _, complete, available, err := scanHarborData(context.Background(), path, 100)
		if err != nil || complete || available || total != 0 {
			t.Fatalf("unsafe root %q reported storage: %d %v %v %v", path, total, complete, available, err)
		}
	}
}

func TestBuildMetricsUnavailableWhenProcessMetadataUnreadable(t *testing.T) {
	root := t.TempDir()
	// A directory where a proc file should be produces a read error on every
	// platform, without relying on root-sensitive permission behavior.
	if err := os.MkdirAll(filepath.Join(root, "42", "cgroup"), 0700); err != nil {
		t.Fatal(err)
	}
	groups, complete := findBuildCgroups(context.Background(), root)
	if complete || len(groups) != 0 {
		t.Fatalf("incomplete proc scan reported complete: %v %v", groups, complete)
	}
	collector := New("")
	collector.procRoot = root
	var snapshot Snapshot
	collector.sampleBuilds(context.Background(), time.Now(), &snapshot)
	if snapshot.Builds.SystemdManagedActiveCount != nil || snapshot.Builds.MemoryCurrentBytes != nil {
		t.Fatalf("unavailable builds reported as measured: %+v", snapshot.Builds)
	}
}

func TestBuildScanToleratesProcessesThatExit(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "42"), 0700); err != nil {
		t.Fatal(err)
	}
	_, complete := findBuildCgroups(context.Background(), root)
	if !complete {
		t.Fatal("disappearing process made metrics unavailable")
	}
}

func TestBoundedScanRejectsEmptyBudgetAndHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	_, _, _, _, complete, available, err := scanHarborData(context.Background(), root, 0)
	if err != nil || complete || !available {
		t.Fatalf("zero budget scan: %v %v %v", complete, available, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, _, _, _, err = scanHarborData(ctx, root, 100)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan cancellation: %v", err)
	}
}
