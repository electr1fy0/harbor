# Capacity reporting

Open **Capacity** in the dashboard to inspect server memory and CPU, the data
filesystem, application usage and limits, and active systemd build usage. The
page refreshes every 15 seconds while open; **Refresh** requests another sample.
The local daemon API also accepts `POST /capacity` with `{}` or
`{"Name":"my-app"}` to filter applications.

On Linux, host memory comes from `MemTotal` and `MemAvailable`. Runtime memory,
peak memory, CPU usage, and task counts come from the sandbox's dedicated cgroup
when available. Without a dedicated cgroup, memory can fall back to the main
process's resident and peak resident size; that measurement excludes child
processes. CPU uses counter deltas between samples of the same runtime. The
first sample, inaccessible counters, and unsupported metrics return `null`.
macOS development still reports accessible filesystem sizes and configured app
limits, but does not claim Linux runtime or build usage.

Filesystem capacity describes the filesystem containing the daemon data
directory, including other programs' files. Harbor's own data, releases, build
cache, and logs use a scan bounded to 20,000 entries. Partial scans are marked;
symlinks are not followed. Temporary build workspaces outside the data directory
are excluded. Storage changes are signed differences between complete scans,
with their actual interval; they are not long-term retention history.

The memory headroom estimate subtracts 10% of total host RAM from currently
available memory. Resource limits are ceilings, not reservations. During a
deployment the current application can overlap with the build and candidate
runtime, so the estimate does not guarantee that another deployment will fit.
Build measurements cover visible systemd-managed build cgroups. Missing or
inaccessible measurements are shown as unavailable.
