// Package capacity reads best-effort host, cgroup, and Harbor storage metrics.
// It does not invoke external commands and reports unavailable values as null.
package capacity

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	buildMemoryLimit = int64(512 << 20)
	buildCPULimit    = 1.0
	reservePercent   = 10.0
	maxSizeEntries   = 20_000
	maxCPUSamples    = 4096
)

// RuntimeLimit contains only the configured limits and process identity needed
// to sample one application. Limits are the configured ceilings, not reserves.
type RuntimeLimit struct {
	Identity      string
	Name          string
	State         string
	PID           int
	ControlGroup  string
	MemoryLimit   int64
	CPUQuotaCores float64
	TasksLimit    int
}

type Collector struct {
	dataDir    string
	procRoot   string
	cgroupRoot string
	platform   string
	scanLimit  int

	mu         sync.Mutex
	previous   map[string]cpuReading
	prevHost   *hostCPUReading
	prevHostAt time.Time
	prevDisk   *diskBaseline
}

type cpuReading struct {
	usageUsec uint64
	sampledAt time.Time
}
type hostCPUReading struct{ total, idle uint64 }

type diskBaseline struct {
	sampledAt                     time.Time
	harbor, releases, cache, logs int64
}

type Snapshot struct {
	SampledAt time.Time `json:"sampledAt"`
	Platform  Platform  `json:"platform"`
	Host      Host      `json:"host"`
	Disk      Disk      `json:"disk"`
	Apps      []App     `json:"apps"`
	Builds    Builds    `json:"builds"`
	Headroom  Headroom  `json:"headroom"`
	Caveats   []string  `json:"caveats"`
}

type Platform struct {
	Supported bool   `json:"supported"`
	Name      string `json:"name"`
	Reason    string `json:"reason,omitempty"`
}

type Host struct {
	MemoryTotalBytes     *int64   `json:"memoryTotalBytes"`
	MemoryAvailableBytes *int64   `json:"memoryAvailableBytes"`
	MemoryUsedBytes      *int64   `json:"memoryUsedBytes"`
	CPUUsagePercent      *float64 `json:"cpuUsagePercent"`
	LogicalCPUs          *int     `json:"logicalCPUs"`
}

type Disk struct {
	FilesystemTotalBytes     *int64   `json:"filesystemTotalBytes"`
	FilesystemAvailableBytes *int64   `json:"filesystemAvailableBytes"`
	FilesystemUsedBytes      *int64   `json:"filesystemUsedBytes"`
	HarborDataBytes          *int64   `json:"harborDataBytes"`
	HarborDataComplete       bool     `json:"harborDataComplete"`
	HarborDataChangeBytes    *int64   `json:"harborDataChangeBytes"`
	ReleasesChangeBytes      *int64   `json:"releasesChangeBytes"`
	BuildCacheChangeBytes    *int64   `json:"buildCacheChangeBytes"`
	LogsChangeBytes          *int64   `json:"logsChangeBytes"`
	ChangeIntervalSeconds    *float64 `json:"changeIntervalSeconds"`
	ReleasesBytes            *int64   `json:"releasesBytes"`
	BuildCacheBytes          *int64   `json:"buildCacheBytes"`
	LogsBytes                *int64   `json:"logsBytes"`
}

type App struct {
	DeploymentID       string   `json:"deploymentId"`
	Name               string   `json:"name"`
	State              string   `json:"state"`
	MemoryLimitBytes   *int64   `json:"memoryLimitBytes"`
	MemoryCurrentBytes *int64   `json:"memoryCurrentBytes"`
	MemoryPeakBytes    *int64   `json:"memoryPeakBytes"`
	CPUQuotaCores      *float64 `json:"cpuQuotaCores"`
	CPUUsageCores      *float64 `json:"cpuUsageCores"`
	TasksLimit         *int     `json:"tasksLimit"`
	TasksCurrent       *int     `json:"tasksCurrent"`
	MetricsSource      string   `json:"metricsSource"`
}

type Builds struct {
	SystemdManagedActiveCount *int     `json:"systemdManagedActiveCount"`
	MemoryLimitBytes          *int64   `json:"memoryLimitBytes"`
	MemoryCurrentBytes        *int64   `json:"memoryCurrentBytes"`
	MemoryPeakBytes           *int64   `json:"memoryPeakBytes"`
	CPUQuotaCores             *float64 `json:"cpuQuotaCores"`
	CPUUsageCores             *float64 `json:"cpuUsageCores"`
}

type Headroom struct {
	MemoryReservePercent             float64 `json:"memoryReservePercent"`
	MemoryReserveBytes               *int64  `json:"memoryReserveBytes"`
	EstimatedMemoryAfterReserveBytes *int64  `json:"estimatedMemoryAfterReserveBytes"`
}

// New creates a collector for one daemon lifetime.
func New(dataDir string) *Collector {
	return &Collector{
		dataDir:    dataDir,
		procRoot:   "/proc",
		cgroupRoot: "/sys/fs/cgroup",
		platform:   runtime.GOOS,
		scanLimit:  maxSizeEntries,
		previous:   make(map[string]cpuReading),
	}
}

// Sample returns current metrics and a delta-based CPU reading when a previous
// sample exists. A canceled request stops filesystem scans promptly.
func (c *Collector) Sample(ctx context.Context, runtimes []RuntimeLimit) (Snapshot, error) {
	if ctx == nil {
		return Snapshot{}, errors.New("capacity sample context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UTC()
	snapshot := Snapshot{
		SampledAt: now,
		Platform:  Platform{Supported: c.platform == "linux", Name: c.platform},
		Apps:      make([]App, 0, len(runtimes)),
		Headroom:  Headroom{MemoryReservePercent: reservePercent},
		Caveats: []string{
			"Configured application memory and CPU values are ceilings, not reservations; enforcement depends on the runtime backend.",
			"Estimated memory after reserve subtracts 10% of host RAM from MemAvailable. It is an estimate, not a deployment guarantee.",
			"Host available memory reflects current host use, includes reclaimable cache, and can change as other workloads run.",
			"Build resource metrics cover active systemd-managed build cgroups; builds outside systemd may not appear.",
			"Build peak memory adds each active build's recorded peak; those peaks may have occurred at different times.",
			"Harbor storage sizes are bounded scans and exclude temporary build workspaces outside the Harbor data directory.",
		},
	}
	if c.platform != "linux" {
		snapshot.Platform.Reason = "Linux /proc and cgroup metrics are unavailable on this platform; host, runtime, and build usage are not measured."
	}

	if c.platform == "linux" {
		c.sampleHost(ctx, now, &snapshot)
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if err := c.sampleDisk(ctx, now, &snapshot); err != nil {
		return Snapshot{}, err
	}
	for _, configured := range runtimes {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		if c.platform == "linux" {
			snapshot.Apps = append(snapshot.Apps, c.sampleApp(now, configured))
		} else {
			snapshot.Apps = append(snapshot.Apps, configuredApp(configured))
		}
	}
	if c.platform == "linux" {
		c.sampleBuilds(ctx, now, &snapshot)
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if snapshot.Host.MemoryTotalBytes != nil && snapshot.Host.MemoryAvailableBytes != nil {
		reserve := int64(float64(*snapshot.Host.MemoryTotalBytes) * reservePercent / 100)
		available := *snapshot.Host.MemoryAvailableBytes - reserve
		if available < 0 {
			available = 0
		}
		snapshot.Headroom.MemoryReserveBytes = int64Ptr(reserve)
		snapshot.Headroom.EstimatedMemoryAfterReserveBytes = int64Ptr(available)
	}
	c.pruneCPUSamples(now)
	return snapshot, nil
}

func (c *Collector) sampleHost(_ context.Context, now time.Time, snapshot *Snapshot) {
	mem, err := readMemInfo(filepath.Join(c.procRoot, "meminfo"))
	if err == nil && mem.total > 0 {
		available := min(mem.available, mem.total)
		snapshot.Host.MemoryTotalBytes = int64Ptr(mem.total)
		snapshot.Host.MemoryAvailableBytes = int64Ptr(available)
		snapshot.Host.MemoryUsedBytes = int64Ptr(mem.total - available)
	}
	if cpus := runtime.NumCPU(); cpus > 0 {
		snapshot.Host.LogicalCPUs = intPtr(cpus)
	}
	reading, err := readHostCPU(filepath.Join(c.procRoot, "stat"))
	if err == nil {
		if c.prevHost != nil && c.prevHostAt.Before(now) && reading.total >= c.prevHost.total && reading.idle >= c.prevHost.idle {
			deltaTotal := reading.total - c.prevHost.total
			deltaIdle := reading.idle - c.prevHost.idle
			if deltaTotal > 0 && deltaIdle <= deltaTotal {
				percent := 100 * float64(deltaTotal-deltaIdle) / float64(deltaTotal)
				snapshot.Host.CPUUsagePercent = finiteFloatPtr(percent)
			}
		}
		copyReading := reading
		c.prevHost = &copyReading
		c.prevHostAt = now
	}
}

func (c *Collector) sampleDisk(ctx context.Context, now time.Time, snapshot *Snapshot) error {
	if strings.TrimSpace(c.dataDir) == "" {
		return nil
	}
	filesystem, err := readFilesystem(c.dataDir)
	if err == nil {
		snapshot.Disk.FilesystemTotalBytes = int64Ptr(filesystem.total)
		snapshot.Disk.FilesystemAvailableBytes = int64Ptr(filesystem.available)
		snapshot.Disk.FilesystemUsedBytes = int64Ptr(filesystem.used)
	}
	total, releases, cache, logs, complete, available, err := scanHarborData(ctx, c.dataDir, c.scanLimit)
	if err != nil {
		return err
	}
	snapshot.Disk.HarborDataComplete = complete
	if available {
		snapshot.Disk.HarborDataBytes = int64Ptr(total)
		snapshot.Disk.ReleasesBytes = int64Ptr(releases)
		snapshot.Disk.BuildCacheBytes = int64Ptr(cache)
		snapshot.Disk.LogsBytes = int64Ptr(logs)
	}
	if complete {
		if c.prevDisk != nil && c.prevDisk.sampledAt.Before(now) {
			interval := now.Sub(c.prevDisk.sampledAt).Seconds()
			if interval > 0 && finite(interval) {
				snapshot.Disk.HarborDataChangeBytes = int64Ptr(total - c.prevDisk.harbor)
				snapshot.Disk.ReleasesChangeBytes = int64Ptr(releases - c.prevDisk.releases)
				snapshot.Disk.BuildCacheChangeBytes = int64Ptr(cache - c.prevDisk.cache)
				snapshot.Disk.LogsChangeBytes = int64Ptr(logs - c.prevDisk.logs)
				snapshot.Disk.ChangeIntervalSeconds = finiteFloatPtr(interval)
			}
		}
		if available {
			c.prevDisk = &diskBaseline{
				sampledAt: now, harbor: total, releases: releases, cache: cache, logs: logs,
			}
		}
	}
	return nil
}

func scanHarborData(ctx context.Context, root string, maxEntries int) (total, releases, cache, logs int64, complete, available bool, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, 0, 0, false, false, err
	}
	info, statErr := os.Lstat(root)
	if statErr != nil || !info.IsDir() {
		return 0, 0, 0, 0, false, false, nil
	}
	available, complete = true, true
	if maxEntries < 1 {
		return 0, 0, 0, 0, false, true, nil
	}
	// WalkDir reads an entire directory before invoking the callback. Read small
	// batches instead so a single large cache directory cannot bypass the cap.
	entries := 1 // Include the root, as with the former WalkDir scan.
	pending := []string{root}
	for len(pending) > 0 {
		directory := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if err := ctx.Err(); err != nil {
			return total, releases, cache, logs, false, available, err
		}
		handle, openErr := os.Open(directory)
		if openErr != nil {
			complete = false
			continue
		}
		for {
			if err := ctx.Err(); err != nil {
				handle.Close()
				return total, releases, cache, logs, false, available, err
			}
			batch, readErr := handle.ReadDir(min(128, max(1, maxEntries-entries+1)))
			for _, entry := range batch {
				entries++
				if entries > maxEntries {
					handle.Close()
					return total, releases, cache, logs, false, available, nil
				}
				if err := ctx.Err(); err != nil {
					handle.Close()
					return total, releases, cache, logs, false, available, err
				}
				if entry.Type()&os.ModeSymlink != 0 {
					continue
				}
				path := filepath.Join(directory, entry.Name())
				if entry.IsDir() {
					pending = append(pending, path)
					continue
				}
				info, infoErr := entry.Info()
				if infoErr != nil {
					complete = false
					continue
				}
				if !info.Mode().IsRegular() || info.Size() < 0 {
					continue
				}
				total = saturatingAdd(total, info.Size())
				relative, relErr := filepath.Rel(root, path)
				if relErr != nil {
					complete = false
					continue
				}
				topLevel := strings.SplitN(relative, string(filepath.Separator), 2)[0]
				switch topLevel {
				case "releases":
					releases = saturatingAdd(releases, info.Size())
				case "build-cache":
					cache = saturatingAdd(cache, info.Size())
				case "logs":
					logs = saturatingAdd(logs, info.Size())
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					complete = false
				}
				break
			}
		}
		if err := handle.Close(); err != nil {
			complete = false
		}
	}
	return total, releases, cache, logs, complete, available, nil
}

func (c *Collector) pruneCPUSamples(now time.Time) {
	cutoff := now.Add(-10 * time.Minute)
	for key, sample := range c.previous {
		if sample.sampledAt.Before(cutoff) {
			delete(c.previous, key)
		}
	}
	if len(c.previous) <= maxCPUSamples {
		return
	}
	type keyedSample struct {
		key       string
		sampledAt time.Time
	}
	oldest := make([]keyedSample, 0, len(c.previous))
	for key, sample := range c.previous {
		oldest = append(oldest, keyedSample{key: key, sampledAt: sample.sampledAt})
	}
	sort.Slice(oldest, func(i, j int) bool { return oldest[i].sampledAt.Before(oldest[j].sampledAt) })
	for _, sample := range oldest[:len(oldest)-maxCPUSamples] {
		delete(c.previous, sample.key)
	}
}

func (c *Collector) sampleApp(now time.Time, configured RuntimeLimit) App {
	app := configuredApp(configured)
	if configured.PID <= 0 {
		return app
	}

	procDir := filepath.Join(c.procRoot, strconv.Itoa(configured.PID))
	cgroup := configured.ControlGroup
	if cgroup != "" {
		if dir, err := safeCgroupDir(c.cgroupRoot, cgroup); err == nil {
			mem, cpu, tasks := readCgroupMetrics(dir)
			if mem.current != nil || mem.peak != nil || cpu != nil || tasks != nil {
				app.MemoryCurrentBytes = mem.current
				app.MemoryPeakBytes = mem.peak
				app.TasksCurrent = tasks
				identity := configured.Identity
				if identity == "" {
					identity = configured.Name + ":" + cgroup
				}
				app.CPUUsageCores = c.cpuCores(now, "app:"+identity, cpu)
				app.MetricsSource = "cgroup v2"
				return app
			}
		}
	}
	current, peak := readProcessMemory(filepath.Join(procDir, "status"))
	app.MemoryCurrentBytes = current
	app.MemoryPeakBytes = peak
	if current != nil || peak != nil {
		app.MetricsSource = "process status"
	}
	return app
}

func configuredApp(configured RuntimeLimit) App {
	app := App{DeploymentID: configured.Identity, Name: configured.Name, State: configured.State, MetricsSource: "unavailable"}
	if configured.MemoryLimit > 0 {
		app.MemoryLimitBytes = int64Ptr(configured.MemoryLimit)
	}
	if configured.CPUQuotaCores > 0 && finite(configured.CPUQuotaCores) {
		app.CPUQuotaCores = finiteFloatPtr(configured.CPUQuotaCores)
	}
	if configured.TasksLimit > 0 {
		app.TasksLimit = intPtr(configured.TasksLimit)
	}
	return app
}

type cgroupMemory struct{ current, peak *int64 }

func readCgroupMetrics(dir string) (cgroupMemory, *uint64, *int) {
	var memory cgroupMemory
	if value, err := readNonNegative(filepath.Join(dir, "memory.current")); err == nil {
		memory.current = int64Ptr(value)
	}
	if value, err := readNonNegative(filepath.Join(dir, "memory.peak")); err == nil {
		memory.peak = int64Ptr(value)
	}
	var cpu *uint64
	if value, err := readCPUUsage(filepath.Join(dir, "cpu.stat")); err == nil {
		cpu = &value
	}
	var tasks *int
	if value, err := readNonNegative(filepath.Join(dir, "pids.current")); err == nil && value <= int64(math.MaxInt) {
		tasks = intPtr(int(value))
	}
	return memory, cpu, tasks
}

func (c *Collector) cpuCores(now time.Time, key string, usage *uint64) *float64 {
	if usage == nil {
		return nil
	}
	previous, ok := c.previous[key]
	c.previous[key] = cpuReading{usageUsec: *usage, sampledAt: now}
	if !ok || !previous.sampledAt.Before(now) || *usage < previous.usageUsec {
		return nil
	}
	elapsed := now.Sub(previous.sampledAt).Seconds()
	if elapsed <= 0 {
		return nil
	}
	return finiteFloatPtr(float64(*usage-previous.usageUsec) / (elapsed * 1_000_000))
}

func (c *Collector) sampleBuilds(ctx context.Context, now time.Time, snapshot *Snapshot) {
	groups, complete := findBuildCgroups(ctx, c.procRoot)
	if !complete {
		return
	}
	count := len(groups)
	snapshot.Builds.SystemdManagedActiveCount = intPtr(count)
	snapshot.Builds.MemoryLimitBytes = int64Ptr(saturatingMultiply(int64(count), buildMemoryLimit))
	snapshot.Builds.CPUQuotaCores = finiteFloatPtr(float64(count) * buildCPULimit)
	if count == 0 {
		snapshot.Builds.MemoryCurrentBytes = int64Ptr(0)
		snapshot.Builds.MemoryPeakBytes = int64Ptr(0)
		snapshot.Builds.CPUUsageCores = finiteFloatPtr(0)
		return
	}
	currentAvailable := true
	peakAvailable := true
	cpuAvailable := true
	var totalUsageCores float64
	haveUsage := true
	for group := range groups {
		if ctx.Err() != nil {
			return
		}
		dir, err := safeCgroupDir(c.cgroupRoot, group)
		if err != nil {
			currentAvailable = false
			peakAvailable = false
			cpuAvailable = false
			haveUsage = false
			continue
		}
		memory, cpu, _ := readCgroupMetrics(dir)
		if memory.current != nil {
			snapshot.Builds.MemoryCurrentBytes = addPtr(snapshot.Builds.MemoryCurrentBytes, *memory.current)
		} else {
			currentAvailable = false
		}
		if memory.peak != nil {
			snapshot.Builds.MemoryPeakBytes = addPtr(snapshot.Builds.MemoryPeakBytes, *memory.peak)
		} else {
			peakAvailable = false
		}
		if cpu != nil {
			key := "build:" + group
			if prior, ok := c.previous[key]; ok && *cpu >= prior.usageUsec && prior.sampledAt.Before(now) {
				elapsed := now.Sub(prior.sampledAt).Seconds()
				if elapsed > 0 {
					totalUsageCores += float64(*cpu-prior.usageUsec) / (elapsed * 1_000_000)
				} else {
					haveUsage = false
				}
			} else {
				haveUsage = false
			}
			c.previous[key] = cpuReading{usageUsec: *cpu, sampledAt: now}
		} else {
			cpuAvailable = false
			haveUsage = false
		}
	}
	if !currentAvailable {
		snapshot.Builds.MemoryCurrentBytes = nil
	}
	if !peakAvailable {
		snapshot.Builds.MemoryPeakBytes = nil
	}
	if cpuAvailable && haveUsage {
		snapshot.Builds.CPUUsageCores = finiteFloatPtr(totalUsageCores)
	}
}

func findBuildCgroups(ctx context.Context, procRoot string) (map[string]struct{}, bool) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return map[string]struct{}{}, false
	}
	groups := make(map[string]struct{})
	complete := true
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return groups, false
		}
		if !entry.IsDir() || !allDigits(entry.Name()) {
			continue
		}
		group, err := readCgroupPath(filepath.Join(procRoot, entry.Name(), "cgroup"))
		if err != nil {
			// Exiting processes may disappear during a scan; inaccessible or malformed
			// cgroup metadata means the active-build count cannot be measured reliably.
			if !errors.Is(err, os.ErrNotExist) {
				complete = false
			}
			continue
		}
		if strings.HasPrefix(filepath.Base(group), "paas-build-") && strings.HasSuffix(filepath.Base(group), ".service") {
			groups[group] = struct{}{}
		}
	}
	return groups, complete
}

func readCgroupPath(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), ":", 3)
		if len(fields) == 3 && fields[1] == "" {
			group := fields[2]
			if !filepath.IsAbs(group) || filepath.Clean(group) != group || group == "/" {
				return "", fmt.Errorf("invalid cgroup path")
			}
			return group, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("unified cgroup v2 path unavailable")
}

func safeCgroupDir(root, group string) (string, error) {
	if !filepath.IsAbs(group) || filepath.Clean(group) != group || group == "/" {
		return "", errors.New("invalid cgroup path")
	}
	dir := filepath.Join(root, strings.TrimLeft(group, string(filepath.Separator)))
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("cgroup path escapes mount")
	}
	return dir, nil
}

func readProcessMemory(path string) (current, peak *int64) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, rest, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || value < 0 || value > math.MaxInt64/1024 {
			continue
		}
		value *= 1024
		switch key {
		case "VmRSS":
			current = int64Ptr(value)
		case "VmHWM":
			peak = int64Ptr(value)
		}
	}
	return current, peak
}

type memInfo struct{ total, available int64 }

func readMemInfo(path string) (memInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return memInfo{}, err
	}
	defer file.Close()
	values := make(map[string]int64, 2)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, rest, ok := strings.Cut(scanner.Text(), ":")
		if !ok || key != "MemTotal" && key != "MemAvailable" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || value < 0 {
			continue
		}
		if len(fields) > 1 && fields[1] == "kB" {
			if value > math.MaxInt64/1024 {
				continue
			}
			value *= 1024
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return memInfo{}, err
	}
	total, hasTotal := values["MemTotal"]
	available, hasAvailable := values["MemAvailable"]
	if !hasTotal || !hasAvailable || total <= 0 {
		return memInfo{}, errors.New("host memory values unavailable")
	}
	return memInfo{total: total, available: available}, nil
}

func readHostCPU(path string) (hostCPUReading, error) {
	file, err := os.Open(path)
	if err != nil {
		return hostCPUReading{}, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return hostCPUReading{}, errors.New("host CPU line unavailable")
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return hostCPUReading{}, errors.New("invalid host CPU line")
	}
	var values []uint64
	end := min(len(fields), 9) // Guest counters are already included in user and nice.
	for _, field := range fields[1:end] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return hostCPUReading{}, err
		}
		values = append(values, value)
	}
	var total uint64
	for _, value := range values {
		if math.MaxUint64-total < value {
			return hostCPUReading{}, errors.New("host CPU counters overflow")
		}
		total += value
	}
	idle := values[3]
	if len(values) > 4 {
		if math.MaxUint64-idle < values[4] {
			return hostCPUReading{}, errors.New("host idle counters overflow")
		}
		idle += values[4]
	}
	return hostCPUReading{total: total, idle: idle}, nil
}

func readNonNegative(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New("invalid non-negative metric")
	}
	return value, nil
}

func readCPUUsage(path string) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), " ")
		if !ok || key != "usage_usec" {
			continue
		}
		return strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("CPU usage unavailable")
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func saturatingAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	if right < 0 && left < math.MinInt64-right {
		return math.MinInt64
	}
	return left + right
}

func saturatingMultiply(left, right int64) int64 {
	if left <= 0 || right <= 0 {
		return 0
	}
	if left > math.MaxInt64/right {
		return math.MaxInt64
	}
	return left * right
}

func addPtr(current *int64, value int64) *int64 {
	if current == nil {
		return int64Ptr(value)
	}
	return int64Ptr(saturatingAdd(*current, value))
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func finiteFloatPtr(value float64) *float64 {
	if !finite(value) || value < 0 {
		return nil
	}
	return &value
}

func int64Ptr(value int64) *int64 { return &value }
func intPtr(value int) *int       { return &value }
