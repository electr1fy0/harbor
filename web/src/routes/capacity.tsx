import { useQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"
import type { ReactNode } from "react"
import {
  IconActivity,
  IconCpu,
  IconDatabase,
  IconDeviceDesktopAnalytics,
  IconMemory,
  IconServer,
} from "@tabler/icons-react"
import { PageError, PageLoading } from "@/components/app/page-state"
import { PageHeader } from "@/components/app/page-header"
import { StatusBadge } from "@/components/app/status-badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { capacityQueryOptions } from "@/lib/queries"
import type { CapacitySnapshot } from "@/lib/types"

export const Route = createFileRoute("/capacity")({ component: CapacityPage })

function CapacityPage() {
  const query = useQuery(capacityQueryOptions())

  if (query.isPending) return <PageLoading rows={5} />
  if (query.isError && !query.data) {
    return (
      <PageError
        message={query.error.message || "Capacity metrics could not be loaded."}
        onRetry={() => void query.refetch()}
      />
    )
  }

  const snapshot = query.data
  return (
    <>
      <PageHeader
        title="Capacity"
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => void query.refetch()}
            disabled={query.isFetching}
          >
            {query.isFetching ? "Refreshing…" : "Refresh"}
          </Button>
        }
      />

      {!snapshot.platform.supported ? (
        <div className="mb-5 rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-950">
          <p className="font-medium">Linux runtime metrics are unavailable</p>
          <p className="mt-1 text-xs leading-5 text-amber-900/80">
            {snapshot.platform.reason ||
              "Host and application usage could not be measured on this platform."}
            {snapshot.disk.filesystemTotalBytes !== null
              ? " Filesystem capacity and Harbor data sizes are still shown when accessible."
              : " Filesystem metrics are unavailable too."}
          </p>
        </div>
      ) : null}

      {query.isError ? (
        <div className="mb-5 rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-xs text-amber-950">
          Showing the last sample. Refresh failed: {query.error.message}
        </div>
      ) : null}

      <div className="mb-5 grid gap-4 xl:grid-cols-2">
        <HostMemory snapshot={snapshot} />
        <HostDisk snapshot={snapshot} />
      </div>

      <BuildUsage
        builds={snapshot.builds}
        supported={snapshot.platform.supported}
      />
      <AppUsage apps={snapshot.apps} supported={snapshot.platform.supported} />

      <div className="mt-5 grid gap-4 xl:grid-cols-2">
        <Card className="border-border/80 bg-white">
          <CardHeader className="border-b border-border/60">
            <CardTitle className="flex items-center gap-2 text-sm">
              <IconDeviceDesktopAnalytics
                size={17}
                className="text-muted-foreground"
              />
              Deployment headroom estimate
            </CardTitle>
            <CardDescription>
              {snapshot.headroom.estimatedMemoryAfterReserveBytes === null
                ? "Host memory headroom is unavailable."
                : `${formatBytes(snapshot.headroom.estimatedMemoryAfterReserveBytes)} memory after a ${snapshot.headroom.memoryReservePercent}% host RAM reserve.`}
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-2 pt-4 text-xs leading-5 text-muted-foreground">
            <p>
              The estimate starts from current available memory, which reflects
              host workload use, and keeps 10% of RAM in reserve.
            </p>
            <p>
              Limits are not reservations. During deploy, the current app may
              overlap with the build and candidate runtime.
            </p>
          </CardContent>
        </Card>

        <Card className="border-border/80 bg-white">
          <CardHeader className="border-b border-border/60">
            <CardTitle className="flex items-center gap-2 text-sm">
              <IconActivity size={17} className="text-muted-foreground" />
              Sample notes
            </CardTitle>
            <CardDescription>
              Sampled {formatTimestamp(snapshot.sampledAt)}
            </CardDescription>
          </CardHeader>
          <CardContent className="pt-4">
            <p className="text-xs leading-5 text-muted-foreground">
              Live CPU readings use the interval since the previous sample. A
              first sample may show CPU as unavailable.
            </p>
            <details className="mt-3 border-t border-border/60 pt-3">
              <summary className="cursor-pointer text-xs font-medium text-foreground">
                How to read these estimates
              </summary>
              <ul className="mt-2 space-y-2 text-xs leading-5 text-muted-foreground">
                {snapshot.caveats.map((note) => (
                  <li key={note} className="flex gap-2">
                    <span
                      aria-hidden
                      className="mt-2 size-1 shrink-0 rounded-full bg-muted-foreground/60"
                    />
                    {note}
                  </li>
                ))}
              </ul>
            </details>
          </CardContent>
        </Card>
      </div>
    </>
  )
}

function HostMemory({ snapshot }: { snapshot: CapacitySnapshot }) {
  const host = snapshot.host
  return (
    <Card className="border-border/80 bg-white">
      <CardHeader className="flex-row items-start gap-3 border-b border-border/60">
        <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-[#f5f6f4] text-muted-foreground">
          <IconMemory size={18} />
        </span>
        <div>
          <CardTitle className="text-sm">Server-wide memory</CardTitle>
          <CardDescription>
            {host.logicalCPUs === null
              ? "Host memory and CPU are reported when Linux metrics are accessible."
              : `${host.logicalCPUs} logical CPUs · host-level reading`}
          </CardDescription>
        </div>
      </CardHeader>
      <CardContent className="space-y-4 pt-4">
        <Meter
          label="Used"
          value={host.memoryUsedBytes}
          total={host.memoryTotalBytes}
          detail={`${formatBytes(host.memoryUsedBytes)} of ${formatBytes(host.memoryTotalBytes)}`}
        />
        <div className="grid grid-cols-2 gap-4 border-t border-border/60 pt-3">
          <Metric
            label="Available"
            value={formatBytes(host.memoryAvailableBytes)}
          />
          <Metric
            label="Host CPU"
            value={
              host.cpuUsagePercent === null
                ? snapshot.platform.supported
                  ? "Collecting sample"
                  : "Unavailable"
                : `${host.cpuUsagePercent.toFixed(1)}%`
            }
            icon={<IconCpu size={14} />}
          />
        </div>
      </CardContent>
    </Card>
  )
}

function HostDisk({ snapshot }: { snapshot: CapacitySnapshot }) {
  const disk = snapshot.disk
  return (
    <Card className="border-border/80 bg-white">
      <CardHeader className="flex-row items-start gap-3 border-b border-border/60">
        <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-[#f5f6f4] text-muted-foreground">
          <IconServer size={18} />
        </span>
        <div>
          <CardTitle className="text-sm">Harbor data filesystem</CardTitle>
          <CardDescription>
            Shared filesystem capacity and Harbor file sizes
          </CardDescription>
        </div>
      </CardHeader>
      <CardContent className="space-y-4 pt-4">
        <Meter
          label="Filesystem used"
          value={disk.filesystemUsedBytes}
          total={disk.filesystemTotalBytes}
          detail={`${formatBytes(disk.filesystemUsedBytes)} of ${formatBytes(disk.filesystemTotalBytes)}`}
        />
        <div className="grid grid-cols-2 gap-4 border-t border-border/60 pt-3">
          <Metric
            label="Available"
            value={formatBytes(disk.filesystemAvailableBytes)}
          />
          <Metric
            label="Harbor data scanned"
            value={formatBytes(disk.harborDataBytes)}
          />
        </div>
        <div className="grid grid-cols-3 gap-2 border-t border-border/60 pt-3">
          <Metric label="Releases" value={formatBytes(disk.releasesBytes)} />
          <Metric
            label="Build cache"
            value={formatBytes(disk.buildCacheBytes)}
          />
          <Metric label="Logs" value={formatBytes(disk.logsBytes)} />
        </div>
        <div className="grid grid-cols-2 gap-2 border-t border-border/60 pt-3 sm:grid-cols-4">
          <Metric
            label="Data change"
            value={formatBytesDelta(disk.harborDataChangeBytes)}
          />
          <Metric
            label="Release change"
            value={formatBytesDelta(disk.releasesChangeBytes)}
          />
          <Metric
            label="Cache change"
            value={formatBytesDelta(disk.buildCacheChangeBytes)}
          />
          <Metric
            label="Log change"
            value={formatBytesDelta(disk.logsChangeBytes)}
          />
        </div>
        <p className="text-[10px] text-muted-foreground">
          {disk.changeIntervalSeconds === null
            ? "Change is available after two complete scans."
            : `Change since the previous complete scan (${disk.changeIntervalSeconds.toFixed(0)}s).`}
        </p>
        {!disk.harborDataComplete && disk.harborDataBytes !== null ? (
          <p className="text-[11px] leading-4 text-amber-800">
            Harbor data scan reached its entry limit or skipped inaccessible
            files; the total is partial.
          </p>
        ) : null}
        {!disk.harborDataComplete && disk.harborDataBytes === null ? (
          <p className="text-[11px] leading-4 text-amber-800">
            Harbor data directory is unavailable; size and growth are not
            measured.
          </p>
        ) : null}
      </CardContent>
    </Card>
  )
}

function BuildUsage({
  builds,
  supported,
}: {
  builds: CapacitySnapshot["builds"]
  supported: boolean
}) {
  const count = builds.systemdManagedActiveCount
  return (
    <Card className="mb-5 border-border/80 bg-white">
      <CardHeader className="flex-row items-start gap-3 border-b border-border/60">
        <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-[#f5f6f4] text-muted-foreground">
          <IconDatabase size={18} />
        </span>
        <div>
          <CardTitle className="text-sm">Build activity</CardTitle>
          <CardDescription>
            {count === null
              ? supported
                ? "Build usage could not be measured."
                : "Build metrics require Linux."
              : count === 0
                ? "No active build was visible at this sample."
                : `${count} active ${count === 1 ? "build" : "builds"} detected`}
          </CardDescription>
        </div>
      </CardHeader>
      <CardContent className="grid gap-4 pt-4 sm:grid-cols-2 lg:grid-cols-4">
        <Metric
          label="Current memory"
          value={formatBytes(builds.memoryCurrentBytes)}
        />
        <Metric
          label="Peak memory (sum)"
          value={formatBytes(builds.memoryPeakBytes)}
        />
        <Metric
          label="Configured build ceilings"
          value={formatBytes(builds.memoryLimitBytes)}
        />
        <Metric
          label="CPU usage / limit"
          value={`${formatCores(builds.cpuUsageCores)} / ${formatCores(builds.cpuQuotaCores)}`}
        />
      </CardContent>
    </Card>
  )
}

function AppUsage({
  apps,
  supported,
}: {
  apps: CapacitySnapshot["apps"]
  supported: boolean
}) {
  return (
    <section aria-label="Application capacity" className="mb-5">
      <div className="mb-3 flex items-end justify-between gap-3">
        <div>
          <h2 className="text-[15px] font-semibold tracking-tight">
            Application usage
          </h2>
          <p className="mt-1 text-xs text-muted-foreground">
            Live values come from each app’s sandbox when the host exposes them.
          </p>
        </div>
      </div>
      {apps.length === 0 ? (
        <Card className="border-dashed bg-white/70 shadow-none">
          <CardContent className="py-8 text-center text-sm text-muted-foreground">
            {supported
              ? "No active application deployments."
              : "No active application deployment limits are available."}
          </CardContent>
        </Card>
      ) : (
        <div className="grid gap-3 xl:grid-cols-2">
          {apps.map((app) => (
            <Card
              key={app.deploymentId || app.name}
              className="border-border/80 bg-white"
            >
              <CardHeader className="flex-row items-center justify-between gap-3 border-b border-border/60">
                <div className="min-w-0">
                  <CardTitle className="truncate text-sm">{app.name}</CardTitle>
                  <CardDescription>
                    Usage: {sourceLabel(app.metricsSource)}
                  </CardDescription>
                </div>
                <StatusBadge state={app.state} />
              </CardHeader>
              <CardContent className="grid grid-cols-2 gap-x-4 gap-y-4 pt-4">
                <Metric
                  label="Current memory"
                  value={formatBytes(app.memoryCurrentBytes)}
                />
                <Metric
                  label="Peak memory"
                  value={formatBytes(app.memoryPeakBytes)}
                />
                <Metric
                  label="Configured memory limit"
                  value={formatBytes(app.memoryLimitBytes)}
                />
                <Metric
                  label="CPU usage / configured quota"
                  value={`${formatCores(app.cpuUsageCores)} / ${formatCores(app.cpuQuotaCores)}`}
                />
                <Metric
                  label="Tasks current / configured limit"
                  value={`${app.tasksCurrent ?? "—"} / ${app.tasksLimit ?? "—"}`}
                />
              </CardContent>
            </Card>
          ))}
        </div>
      )}
    </section>
  )
}

function Metric({
  label,
  value,
  icon,
}: {
  label: string
  value: string
  icon?: ReactNode
}) {
  return (
    <div className="min-w-0">
      <p className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
        {icon}
        {label}
      </p>
      <p
        className="mt-1 truncate text-sm font-semibold text-[#202a27] tabular-nums"
        title={value}
      >
        {value}
      </p>
    </div>
  )
}

function Meter({
  label,
  value,
  total,
  detail,
}: {
  label: string
  value: number | null
  total: number | null
  detail: string
}) {
  const ratio =
    value !== null && total !== null && total > 0
      ? Math.max(0, Math.min(100, (value / total) * 100))
      : null
  return (
    <div>
      <div className="flex items-center justify-between gap-3">
        <span className="text-xs text-muted-foreground">{label}</span>
        <span className="text-xs font-medium tabular-nums">{detail}</span>
      </div>
      <div
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={total ?? undefined}
        aria-valuenow={value ?? undefined}
        className="mt-2 h-1.5 overflow-hidden rounded-full bg-muted"
      >
        {ratio !== null ? (
          <div
            className="h-full rounded-full bg-[#4b8c72]"
            style={{ width: `${ratio}%` }}
          />
        ) : null}
      </div>
    </div>
  )
}

function formatBytes(value: number | null): string {
  if (value === null || !Number.isFinite(value) || value < 0) return "—"
  if (value < 1024) return `${value} B`
  const units = ["KiB", "MiB", "GiB", "TiB"]
  let scaled = value / 1024
  let unit = units[0]
  for (let index = 1; index < units.length && scaled >= 1024; index += 1) {
    scaled /= 1024
    unit = units[index] ?? unit
  }
  return `${scaled.toFixed(scaled >= 100 ? 0 : 1)} ${unit}`
}

function formatBytesDelta(value: number | null): string {
  if (value === null || !Number.isSafeInteger(value)) return "—"
  if (value === 0) return "0 B"
  return `${value > 0 ? "+" : "−"}${formatBytes(Math.abs(value))}`
}

function formatCores(value: number | null): string {
  return value === null || !Number.isFinite(value)
    ? "—"
    : `${value.toFixed(2)} cores`
}

function formatTimestamp(value: string): string {
  if (!value) return "time unavailable"
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime())
    ? "time unavailable"
    : parsed.toLocaleString()
}

function sourceLabel(source: string): string {
  switch (source) {
    case "cgroup v2":
      return "Linux runtime"
    case "process status":
      return "process memory only"
    default:
      return "unavailable"
  }
}
