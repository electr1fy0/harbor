import { useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { FeatureCard, fieldClass, useFeatureAction } from "./feature-card"
import {
  downloadText,
  featureCall,
  type ObserveSettings,
  type ObserveView,
} from "@/lib/features"
import { formatDate } from "./format"

export function AppObservability({ name }: { name: string }) {
  const [historyWindow, setHistoryWindow] = useState("hour")
  const query = useQuery({
    queryKey: ["harbor", "observe", name, historyWindow],
    queryFn: () =>
      featureCall<ObserveView>("observe", { action: "get", name, limit: 120 }),
    refetchInterval: 15000,
  })
  const bundle = useFeatureAction<Record<string, unknown>>("observe", (data) =>
    downloadText(
      `${name}-diagnostics.json`,
      JSON.stringify(data, null, 2),
      "application/json"
    )
  )
  if (query.isPending)
    return <p className="text-sm">Loading metrics and routing settings…</p>
  if (query.isError)
    return (
      <p role="alert" className="text-sm text-red-700">
        {query.error.message}
      </p>
    )
  const view = query.data
  const points = [...view.history].reverse().map((p) => ({
    at: p.at,
    value:
      p.snapshot.apps.find((a) => a.name === name)?.memoryCurrentBytes ?? null,
  }))
  return (
    <div className="grid gap-4">
      <FeatureCard
        title="Memory history"
        description="Measurements persist on this server. Missing measurements are left unavailable."
      >
        <label className={fieldClass}>
          History window
          <select
            className="h-9 w-fit rounded-md border px-2"
            value={historyWindow}
            onChange={(e) => setHistoryWindow(e.target.value)}
          >
            <option value="hour">Last hour</option>
            <option value="day">Last 24 hours</option>
          </select>
        </label>
        <MemoryChart points={points} />
        <MetricChart
          label="CPU usage"
          unit="cores"
          points={[...view.history].reverse().map((p) => ({
            at: p.at,
            value:
              p.snapshot.apps.find((a) => a.name === name)?.cpuUsageCores ??
              null,
          }))}
        />
        <MetricChart
          label="Task count"
          unit="tasks"
          points={[...view.history].reverse().map((p) => ({
            at: p.at,
            value:
              p.snapshot.apps.find((a) => a.name === name)?.tasksCurrent ??
              null,
          }))}
        />
        <p className="text-xs text-muted-foreground">
          Showing {points.length} samples, collected every 15 seconds.
        </p>
      </FeatureCard>
      <FeatureCard
        title="HTTP traffic"
        description="Counters cover the current daemon lifetime and include routing responses."
      >
        <div className="overflow-x-auto">
          <table className="w-full text-left text-xs">
            <thead>
              <tr>
                <th className="pb-2">Hostname</th>
                <th>Requests</th>
                <th>Transferred</th>
                <th>Mean latency</th>
                <th>Statuses</th>
              </tr>
            </thead>
            <tbody>
              {view.traffic.map((t) => (
                <tr key={t.host}>
                  <td className="py-2">{t.host}</td>
                  <td>{t.requests}</td>
                  <td>{(t.bytes / 1024).toFixed(1)} KiB</td>
                  <td>
                    {t.requests
                      ? (t.totalLatencyMs / t.requests).toFixed(1)
                      : "—"}{" "}
                    ms
                  </td>
                  <td>
                    {Object.entries(t.statusCodes)
                      .map(([code, count]) => `${code}: ${count}`)
                      .join(" · ")}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {!view.traffic.length ? (
          <p className="text-xs text-muted-foreground">
            No requests recorded yet.
          </p>
        ) : null}
      </FeatureCard>
      <ObserveForm key={name} initial={view.settings} />
      <FeatureCard title="Notifications and diagnostics">
        {view.notifications.map((n, i) => (
          <p key={i} className="text-xs">
            {formatDate(n.at)} · {n.event} ·{" "}
            {n.success ? "Delivered" : "Delivery failed"}
          </p>
        ))}
        {!view.notifications.length ? (
          <p className="text-xs text-muted-foreground">
            No notification deliveries recorded.
          </p>
        ) : null}
        <Button
          variant="outline"
          disabled={bundle.isPending}
          onClick={() => bundle.mutate({ action: "bundle", name })}
        >
          Download diagnostic bundle
        </Button>
      </FeatureCard>
    </div>
  )
}
function ObserveForm({ initial }: { initial: ObserveSettings }) {
  const [settings, setSettings] = useState(initial)
  const [webhook, setWebhook] = useState("")
  const [changeWebhook, setChangeWebhook] = useState(false)
  const save = useFeatureAction<{ settings: ObserveSettings }>(
    "observe",
    (data) => {
      setSettings(data.settings)
      setWebhook("")
      setChangeWebhook(false)
    }
  )
  const update = <TKey extends keyof ObserveSettings>(
    key: TKey,
    value: ObserveSettings[TKey]
  ) => setSettings((s) => ({ ...s, [key]: value }))
  return (
    <FeatureCard
      title="Routing and notifications"
      description="Maintenance returns HTTP 503 with your message."
    >
      <form
        className="grid gap-4"
        onSubmit={(e) => {
          e.preventDefault()
          save.mutate({
            action: "configure",
            name: settings.name,
            settings,
            ...(changeWebhook ? { webhookURL: webhook } : {}),
          })
        }}
      >
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={settings.maintenance}
            onChange={(e) => update("maintenance", e.target.checked)}
          />
          Enable maintenance mode
        </label>
        <label className={fieldClass}>
          Maintenance message
          <Input
            value={settings.maintenanceMessage}
            maxLength={1024}
            onChange={(e) => update("maintenanceMessage", e.target.value)}
          />
        </label>
        <div className="grid gap-4 sm:grid-cols-3">
          <label className={fieldClass}>
            Host memory alert (%)
            <Input
              type="number"
              min={0}
              max={100}
              value={settings.memoryPercent}
              onChange={(e) => update("memoryPercent", Number(e.target.value))}
            />
          </label>
          <label className={fieldClass}>
            Disk alert (%)
            <Input
              type="number"
              min={0}
              max={100}
              value={settings.diskPercent}
              onChange={(e) => update("diskPercent", Number(e.target.value))}
            />
          </label>
          <label className={fieldClass}>
            Notification cooldown (seconds)
            <Input
              type="number"
              min={30}
              max={86400}
              value={settings.cooldownSeconds}
              onChange={(e) =>
                update("cooldownSeconds", Number(e.target.value))
              }
            />
          </label>
        </div>
        <p className="text-xs text-muted-foreground">
          Set an alert threshold to 0 to disable it.
        </p>
        <label className="flex items-center gap-2 text-xs">
          <input
            type="checkbox"
            checked={changeWebhook}
            onChange={(e) => setChangeWebhook(e.target.checked)}
          />
          Change notification webhook{" "}
          {settings.webhookConfigured
            ? "(currently configured)"
            : "(none configured)"}
        </label>
        {changeWebhook ? (
          <label className={fieldClass}>
            HTTPS webhook URL · leave empty to remove
            <Input
              type="password"
              autoComplete="off"
              value={webhook}
              onChange={(e) => setWebhook(e.target.value)}
            />
          </label>
        ) : null}
        <fieldset className="flex flex-wrap gap-3">
          <legend className="mb-2 text-xs font-medium">Notify on</legend>
          {[
            "BUILD_FAILED",
            "START_FAILED",
            "UNHEALTHY",
            "CRASHED",
            "OOM_KILLED",
            "RUNNING",
            "STOPPED",
          ].map((event) => (
            <label key={event} className="flex items-center gap-1 text-xs">
              <input
                type="checkbox"
                checked={settings.events.includes(event)}
                onChange={(e) =>
                  update(
                    "events",
                    e.target.checked
                      ? [...settings.events, event]
                      : settings.events.filter((v) => v !== event)
                  )
                }
              />
              {event.toLowerCase().replaceAll("_", " ")}
            </label>
          ))}
        </fieldset>
        <Button className="w-fit" disabled={save.isPending} type="submit">
          Save routing and alerts
        </Button>
      </form>
    </FeatureCard>
  )
}
function MemoryChart(props: {
  points: Array<{ at: string; value: number | null }>
}) {
  return (
    <MetricChart
      label="Memory usage"
      unit="MiB"
      points={props.points.map((p) => ({
        ...p,
        value: p.value === null ? null : p.value / 1048576,
      }))}
    />
  )
}
function MetricChart({
  label,
  unit,
  points,
}: {
  points: Array<{ at: string; value: number | null }>
  label: string
  unit: string
}) {
  const measured = points.filter((p) => p.value !== null)
  if (!measured.length)
    return (
      <p className="text-sm text-muted-foreground">
        {label} history is unavailable until the host provides measurements.
      </p>
    )
  const max = Math.max(...measured.map((p) => p.value!), 1)
  const groups: string[][] = []
  let group: string[] = []
  points.forEach((p, i) => {
    if (p.value === null) {
      if (group.length) groups.push(group)
      group = []
    } else {
      group.push(
        `${20 + (i / Math.max(points.length - 1, 1)) * 660},${145 - (p.value / max) * 120}`
      )
    }
  })
  if (group.length) groups.push(group)
  return (
    <>
      <svg
        viewBox="0 0 700 170"
        role="img"
        aria-label={`${label} over time`}
        className="w-full"
      >
        <title>
          {label}, maximum {max.toFixed(2)} {unit}
        </title>
        <path d="M20 20V145H680" fill="none" stroke="#cbd5d1" />
        {groups.map((g, i) => (
          <polyline
            key={i}
            points={g.join(" ")}
            fill="none"
            stroke="#37866c"
            strokeWidth="2"
          />
        ))}
        <text x="25" y="18" fontSize="10" fill="#65736d">
          {max.toFixed(2)} {unit}
        </text>
      </svg>
      <p className="text-xs text-muted-foreground">
        {formatDate(points[0].at)} — {formatDate(points[points.length - 1].at)}
      </p>
    </>
  )
}
