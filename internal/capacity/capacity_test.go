package capacity

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestSampleReadsHostRuntimeBuildAndHarborStorage(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	cgroupRoot := filepath.Join(root, "cgroup")
	dataDir := filepath.Join(root, "data")
	writeFixture(t, filepath.Join(procRoot, "meminfo"), "MemTotal: 4096 kB\nMemAvailable: 2048 kB\n")
	writeFixture(t, filepath.Join(procRoot, "stat"), "cpu 100 20 30 850 10 5 6 2 4 3\n")

	runtimeGroup := "/system.slice/paas-deploy-example.service"
	runtimeDir := filepath.Join(cgroupRoot, runtimeGroup[1:])
	writeCgroup(t, runtimeDir, "65536", "131072", "usage_usec 100000\nnr_periods 10\n", "2")
	buildGroup := "/user.slice/paas-build-0123456789abcdef.service"
	buildDir := filepath.Join(cgroupRoot, buildGroup[1:])
	writeCgroup(t, buildDir, "32768", "65536", "usage_usec 250000\n", "1")
	writeFixture(t, filepath.Join(procRoot, "101", "cgroup"), "0::"+buildGroup+"\n")
	writeFixture(t, filepath.Join(dataDir, "paas.db"), "db")
	writeFixture(t, filepath.Join(dataDir, "releases", "one", "app"), "release")
	writeFixture(t, filepath.Join(dataDir, "build-cache", "module"), "cache")
	writeFixture(t, filepath.Join(dataDir, "logs", "one.log"), "log")

	collector := New(dataDir)
	collector.procRoot = procRoot
	collector.cgroupRoot = cgroupRoot
	collector.platform = "linux"
	runtimes := []RuntimeLimit{{
		Identity: "deployment-one", Name: "sample-app", State: "RUNNING", PID: 42,
		ControlGroup: runtimeGroup, MemoryLimit: 128 << 20, CPUQuotaCores: 0.5, TasksLimit: 64,
	}}

	first, err := collector.Sample(context.Background(), runtimes)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Platform.Supported || first.Host.MemoryTotalBytes == nil || *first.Host.MemoryTotalBytes != 4<<20 {
		t.Fatalf("host memory sample = %+v", first.Host)
	}
	if first.Host.MemoryAvailableBytes == nil || *first.Host.MemoryAvailableBytes != 2<<20 {
		t.Fatalf("host available memory = %+v", first.Host.MemoryAvailableBytes)
	}
	if first.Headroom.MemoryReservePercent != 10 || first.Headroom.EstimatedMemoryAfterReserveBytes == nil || *first.Headroom.EstimatedMemoryAfterReserveBytes != (2<<20)-(4<<20)/10 {
		t.Fatalf("memory headroom = %+v", first.Headroom)
	}
	if len(first.Apps) != 1 || first.Apps[0].MemoryCurrentBytes == nil || *first.Apps[0].MemoryCurrentBytes != 65536 {
		t.Fatalf("app memory current = %+v", first.Apps)
	}
	app := first.Apps[0]
	if app.MemoryPeakBytes == nil || *app.MemoryPeakBytes != 131072 || app.CPUUsageCores != nil {
		t.Fatalf("app peak or first CPU sample = %+v", app)
	}
	if app.TasksCurrent == nil || *app.TasksCurrent != 2 || app.MemoryLimitBytes == nil || *app.MemoryLimitBytes != 128<<20 {
		t.Fatalf("app limits/current tasks = %+v", app)
	}
	if first.Builds.SystemdManagedActiveCount == nil || *first.Builds.SystemdManagedActiveCount != 1 {
		t.Fatalf("build count = %+v", first.Builds)
	}
	if first.Builds.MemoryCurrentBytes == nil || *first.Builds.MemoryCurrentBytes != 32768 || first.Builds.CPUUsageCores != nil {
		t.Fatalf("build metrics = %+v", first.Builds)
	}
	if !first.Disk.HarborDataComplete || first.Disk.HarborDataBytes == nil || *first.Disk.HarborDataBytes != int64(len("dbreleasecachelog")) {
		t.Fatalf("Harbor data usage = %+v", first.Disk)
	}
	if first.Disk.ReleasesBytes == nil || *first.Disk.ReleasesBytes != int64(len("release")) || first.Disk.BuildCacheBytes == nil || *first.Disk.BuildCacheBytes != int64(len("cache")) || first.Disk.LogsBytes == nil || *first.Disk.LogsBytes != int64(len("log")) {
		t.Fatalf("Harbor subdirectory usage = %+v", first.Disk)
	}

	writeFixture(t, filepath.Join(runtimeDir, "cpu.stat"), "usage_usec 110000\n")
	writeFixture(t, filepath.Join(buildDir, "cpu.stat"), "usage_usec 260000\n")
	writeFixture(t, filepath.Join(procRoot, "stat"), "cpu 120 20 30 850 10 5 6 2 4 3\n")
	time.Sleep(10 * time.Millisecond)
	second, err := collector.Sample(context.Background(), runtimes)
	if err != nil {
		t.Fatal(err)
	}
	if second.Host.CPUUsagePercent == nil || !finite(*second.Host.CPUUsagePercent) {
		t.Fatalf("host CPU sample = %+v", second.Host.CPUUsagePercent)
	}
	if second.Apps[0].CPUUsageCores == nil || !finite(*second.Apps[0].CPUUsageCores) {
		t.Fatalf("app CPU sample = %+v", second.Apps[0].CPUUsageCores)
	}
	if second.Builds.CPUUsageCores == nil || !finite(*second.Builds.CPUUsageCores) {
		t.Fatalf("build CPU sample = %+v", second.Builds.CPUUsageCores)
	}
}

func TestHostCPUCounterDecreaseResetsBaseline(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	writeFixture(t, filepath.Join(procRoot, "meminfo"), "MemTotal: 4096 kB\nMemAvailable: 2048 kB\n")
	statPath := filepath.Join(procRoot, "stat")
	writeFixture(t, statPath, "cpu 100 0 50 850\n")
	collector := New("")
	collector.procRoot = procRoot
	collector.platform = "linux"
	if _, err := collector.Sample(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, statPath, "cpu 10 0 5 85\n")
	decreased, err := collector.Sample(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if decreased.Host.CPUUsagePercent != nil {
		t.Fatalf("counter reset produced CPU usage %v", *decreased.Host.CPUUsagePercent)
	}
	writeFixture(t, statPath, "cpu 20 0 10 100\n")
	reset, err := collector.Sample(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if reset.Host.CPUUsagePercent == nil || math.Abs(*reset.Host.CPUUsagePercent-50) > 0.001 {
		t.Fatalf("CPU after counter reset = %v, want 50%%", reset.Host.CPUUsagePercent)
	}
}

func TestSampleFallsBackToProcessMemoryWithoutCgroup(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	cgroupRoot := filepath.Join(root, "cgroup")
	writeFixture(t, filepath.Join(procRoot, "42", "cgroup"), "0::/user.slice/session.scope\n")
	writeFixture(t, filepath.Join(procRoot, "42", "status"), "Name:\tapp\nVmRSS:\t12 kB\nVmHWM:\t19 kB\n")
	writeCgroup(t, filepath.Join(cgroupRoot, "user.slice", "session.scope"), "999999", "999999", "usage_usec 999999\n", "9")
	collector := New("")
	collector.procRoot = procRoot
	collector.cgroupRoot = cgroupRoot
	collector.platform = "linux"

	snapshot, err := collector.Sample(context.Background(), []RuntimeLimit{{
		Name: "host-app", State: "RUNNING", PID: 42, MemoryLimit: 64 << 20,
		CPUQuotaCores: 0.25, TasksLimit: 16,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Apps) != 1 {
		t.Fatalf("apps = %+v", snapshot.Apps)
	}
	app := snapshot.Apps[0]
	if app.MemoryCurrentBytes == nil || *app.MemoryCurrentBytes != 12*1024 || app.MemoryPeakBytes == nil || *app.MemoryPeakBytes != 19*1024 {
		t.Fatalf("process memory fallback = %+v", app)
	}
	if app.CPUUsageCores != nil || app.MetricsSource != "process status" {
		t.Fatalf("unavailable CPU was reported as measured: %+v", app)
	}
}

func TestAppCPUUsesPerIdentityTimestampAcrossInterleavedSamples(t *testing.T) {
	root := t.TempDir()
	cgroupRoot := filepath.Join(root, "cgroup")
	groupA := "/system.slice/app-a.service"
	groupB := "/system.slice/app-b.service"
	writeCgroup(t, filepath.Join(cgroupRoot, groupA[1:]), "1", "1", "usage_usec 1000000\n", "1")
	writeCgroup(t, filepath.Join(cgroupRoot, groupB[1:]), "1", "1", "usage_usec 1000000\n", "1")
	collector := New("")
	collector.cgroupRoot = cgroupRoot
	collector.platform = "linux"
	appA := RuntimeLimit{Identity: "deploy-a", Name: "app-a", PID: 1, ControlGroup: groupA}
	appB := RuntimeLimit{Identity: "deploy-b", Name: "app-b", PID: 2, ControlGroup: groupB}
	start := time.Unix(100, 0)

	if reading := collector.sampleApp(start, appB); reading.CPUUsageCores != nil {
		t.Fatalf("first app B CPU sample should be unavailable: %+v", reading)
	}
	collector.sampleApp(start.Add(5*time.Second), appA) // A-only filtered snapshot between B samples.
	writeFixture(t, filepath.Join(cgroupRoot, groupB[1:], "cpu.stat"), "usage_usec 1150000\n")
	reading := collector.sampleApp(start.Add(10*time.Second), appB)
	if reading.CPUUsageCores == nil || math.Abs(*reading.CPUUsageCores-0.015) > 0.000001 {
		t.Fatalf("interleaved app B CPU sample = %+v, want 0.015 cores over its own 10s interval", reading)
	}
}

func TestDiskGrowthOnlyUsesCompleteScans(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "a"), "ab")
	writeFixture(t, filepath.Join(root, "b"), "four")
	collector := New(root)
	collector.platform = "darwin"
	collector.scanLimit = 2 // The root and one file fit; the second file makes the scan partial.

	partial, err := collector.Sample(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Disk.HarborDataComplete || partial.Disk.HarborDataChangeBytes != nil {
		t.Fatalf("partial initial scan reported growth: %+v", partial.Disk)
	}
	if err := os.Remove(filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	complete, err := collector.Sample(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !complete.Disk.HarborDataComplete || complete.Disk.HarborDataChangeBytes != nil {
		t.Fatalf("first complete scan used a partial baseline: %+v", complete.Disk)
	}
	writeFixture(t, filepath.Join(root, "a"), "abcde")
	changed, err := collector.Sample(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Disk.HarborDataChangeBytes == nil || *changed.Disk.HarborDataChangeBytes != 3 || changed.Disk.ChangeIntervalSeconds == nil || *changed.Disk.ChangeIntervalSeconds <= 0 {
		t.Fatalf("complete scan growth = %+v, want +3 bytes and an interval", changed.Disk)
	}
}

func TestDiskGrowthReportsSignedSubdirectoryChanges(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "releases", "app", "artifact"), "12345")
	writeFixture(t, filepath.Join(root, "build-cache", "module"), "1234")
	writeFixture(t, filepath.Join(root, "logs", "app.log"), "123")
	collector := New(root)
	collector.platform = "darwin"
	first, err := collector.Sample(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Disk.HarborDataChangeBytes != nil {
		t.Fatalf("first scan unexpectedly has a delta: %+v", first.Disk)
	}
	writeFixture(t, filepath.Join(root, "releases", "app", "artifact"), "12")
	writeFixture(t, filepath.Join(root, "logs", "app.log"), "123456")
	second, err := collector.Sample(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Disk.HarborDataChangeBytes == nil || *second.Disk.HarborDataChangeBytes != 0 ||
		second.Disk.ReleasesChangeBytes == nil || *second.Disk.ReleasesChangeBytes != -3 ||
		second.Disk.BuildCacheChangeBytes == nil || *second.Disk.BuildCacheChangeBytes != 0 ||
		second.Disk.LogsChangeBytes == nil || *second.Disk.LogsChangeBytes != 3 {
		t.Fatalf("signed directory growth = %+v", second.Disk)
	}
}

func TestConfiguredAppsRemainVisibleWhenLinuxMetricsAreUnavailable(t *testing.T) {
	app := configuredApp(RuntimeLimit{
		Name: "configured-app", State: "RUNNING", MemoryLimit: 128 << 20,
		CPUQuotaCores: 0.5, TasksLimit: 64,
	})
	if app.Name != "configured-app" || app.State != "RUNNING" || app.MemoryLimitBytes == nil || app.MemoryCurrentBytes != nil || app.CPUUsageCores != nil {
		t.Fatalf("configured app view = %+v", app)
	}
}

func TestCanceledSampleAndBoundedHarborScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New("").Sample(ctx, nil); err == nil {
		t.Fatal("canceled capacity sample succeeded")
	}

	root := t.TempDir()
	for index := 0; index < 5; index++ {
		writeFixture(t, filepath.Join(root, "file-"+string(rune('a'+index))), "x")
	}
	total, _, _, _, complete, available, err := scanHarborData(context.Background(), root, 3)
	if err != nil {
		t.Fatal(err)
	}
	if complete || !available || total > 3 {
		t.Fatalf("bounded scan total=%d complete=%t available=%t", total, complete, available)
	}
}

func TestMissingHarborDataRootIsUnavailable(t *testing.T) {
	collector := New(filepath.Join(t.TempDir(), "missing"))
	collector.platform = "darwin"
	snapshot, err := collector.Sample(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Disk.HarborDataComplete || snapshot.Disk.HarborDataBytes != nil || snapshot.Disk.ReleasesBytes != nil {
		t.Fatalf("missing data root was reported as measured: %+v", snapshot.Disk)
	}
}

func TestCPUReadingPrunesOldAndCapsRetainedIdentities(t *testing.T) {
	collector := New("")
	now := time.Unix(1_000, 0)
	collector.previous["old"] = cpuReading{usageUsec: 1, sampledAt: now.Add(-11 * time.Minute)}
	collector.previous["recent"] = cpuReading{usageUsec: 1, sampledAt: now}
	collector.pruneCPUSamples(now)
	if _, ok := collector.previous["old"]; ok {
		t.Fatal("stale CPU identity was retained")
	}
	for index := 0; index < maxCPUSamples+1; index++ {
		key := "sample-" + strconv.Itoa(index)
		collector.previous[key] = cpuReading{usageUsec: 1, sampledAt: now.Add(time.Duration(index) * time.Nanosecond)}
	}
	collector.pruneCPUSamples(now.Add(time.Second))
	if len(collector.previous) > maxCPUSamples {
		t.Fatalf("CPU reading map has %d identities after pruning", len(collector.previous))
	}
}

func TestSafeCgroupPathRejectsTraversal(t *testing.T) {
	for _, group := range []string{"relative", "/../outside", "/group/../outside", "/"} {
		if _, err := safeCgroupDir("/sys/fs/cgroup", group); err == nil {
			t.Errorf("safeCgroupDir accepted %q", group)
		}
	}
}

func writeCgroup(t *testing.T, dir, current, peak, cpu, tasks string) {
	t.Helper()
	writeFixture(t, filepath.Join(dir, "memory.current"), current+"\n")
	writeFixture(t, filepath.Join(dir, "memory.peak"), peak+"\n")
	writeFixture(t, filepath.Join(dir, "cpu.stat"), cpu)
	writeFixture(t, filepath.Join(dir, "pids.current"), tasks+"\n")
}

func writeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFiniteHelperRejectsNonFiniteValues(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		if finiteFloatPtr(value) != nil {
			t.Errorf("finiteFloatPtr(%v) returned a value", value)
		}
	}
}
