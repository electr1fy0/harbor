import { useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { FeatureCard, fieldClass, useFeatureAction } from "./feature-card"
import { featureCall } from "@/lib/features"
import { formatDate } from "./format"

export interface OperationSettings {
  favorite: boolean
  tags: string[]
  archived: boolean
  requiredEnvironmentKeys: string[]
  settingsRevision: number
  appliedSettingsRevision: number
  pendingSettings: boolean
  healthProbeIntervalSeconds: number
  healthProbeTimeoutSeconds: number
  healthFailureThreshold: number
  healthStatus: string
  healthFailureCount: number
  lastHealthCheckAt?: string
  restartAttempt: number
  nextRestartAt?: string
  crashLoop: boolean
  retainReleaseCount: number
  automaticRestartEnabled?: boolean
  automaticRestartMaxAttempts?: number
  automaticRestartBaseDelaySeconds?: number
}
export interface AttemptView {
  id: string
  appName: string
  releaseId?: string
  state: string
  error?: string
  createdAt: string
}
export interface AttemptHistory {
  deployments: Array<{
    deployment: AttemptView
    stages: Partial<Record<string, number>>
  }>
  total: number
  hasMore: boolean
  offset: number
  limit: number
}
export function useOperations(name: string) {
  return useQuery({
    queryKey: ["harbor", "operation-settings", name],
    queryFn: () =>
      featureCall<{ operations: OperationSettings }>("operations", {
        action: "settings",
        appName: name,
      }),
    refetchInterval: 5000,
  })
}
export function OperationsPanel({ name }: { name: string }) {
  const query = useOperations(name)
  if (query.isPending)
    return <p className="text-sm">Loading application controls…</p>
  if (query.isError && !query.data)
    return (
      <p role="alert" className="text-red-700">
        {query.error?.message}
      </p>
    )
  return (
    <>
      {query.isError ? (
        <p role="status" className="mb-3 text-xs text-amber-800">
          Latest settings could not be refreshed: {query.error?.message}
        </p>
      ) : null}
      <OperationForms key={name} name={name} initial={query.data.operations} />
    </>
  )
}
function OperationForms({
  name,
  initial,
}: {
  name: string
  initial: OperationSettings
}) {
  const action = useFeatureAction("operations")
  const navigate = useNavigate()
  const remove = useFeatureAction(
    "operations",
    () => void navigate({ to: "/" })
  )
  const [tags, setTags] = useState(initial.tags.join(", "))
  const [required, setRequired] = useState(
    initial.requiredEnvironmentKeys.join(", ")
  )
  const [favorite, setFavorite] = useState(initial.favorite)
  const [interval, setInterval] = useState(initial.healthProbeIntervalSeconds)
  const [timeout, setTimeout] = useState(initial.healthProbeTimeoutSeconds)
  const [failures, setFailures] = useState(initial.healthFailureThreshold)
  const [retention, setRetention] = useState(initial.retainReleaseCount)
  const [confirmation, setConfirmation] = useState("")
  const [preserveData, setPreserveData] = useState(true)
  const [enabled, setEnabled] = useState(
    initial.automaticRestartEnabled ?? false
  )
  const [maxAttempts, setMaxAttempts] = useState(
    initial.automaticRestartMaxAttempts ?? 5
  )
  const [baseDelay, setBaseDelay] = useState(
    initial.automaticRestartBaseDelaySeconds ?? 2
  )
  const preflight = useQuery({
    queryKey: ["harbor", "preflight", name],
    queryFn: () =>
      featureCall<{
        preflight: {
          ready: boolean
          missingRequiredEnvironmentKeys: string[]
          capacity: {
            supported: boolean
            canFit?: boolean
            message: string
            requestedBytes: number
          }
        }
      }>("operations", { action: "preflight", appName: name }),
    enabled: false,
  })
  const tokens = (s: string) =>
    s
      .split(",")
      .map((v) => v.trim())
      .filter(Boolean)
  return (
    <div className="grid gap-4">
      <FeatureCard title="Project organization">
        <form
          className="grid gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            action.mutate({
              action: "configureMetadata",
              appName: name,
              favorite,
              tags: tokens(tags),
              requiredEnvironmentKeys: tokens(required),
            })
          }}
        >
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={favorite}
              onChange={(e) => setFavorite(e.target.checked)}
            />
            Favorite project
          </label>
          <label className={fieldClass}>
            Tags · comma separated
            <Input value={tags} onChange={(e) => setTags(e.target.value)} />
          </label>
          <label className={fieldClass}>
            Required runtime environment keys · comma separated
            <Input
              value={required}
              onChange={(e) => setRequired(e.target.value)}
              placeholder="API_KEY, APP_MODE"
            />
          </label>
          <Button className="w-fit" disabled={action.isPending} type="submit">
            Save project metadata
          </Button>
        </form>
        <Button
          variant="outline"
          disabled={action.isPending}
          onClick={() =>
            action.mutate({
              action: initial.archived ? "unarchive" : "archive",
              appName: name,
            })
          }
        >
          {initial.archived ? "Unarchive project" : "Archive project"}
        </Button>
      </FeatureCard>
      <FeatureCard
        title="Ongoing health checks"
        description="Check application responsiveness after deployment. Set the interval to 0 to disable probes."
      >
        <p className="text-xs">
          Status: {initial.healthStatus || "unavailable"} · Consecutive
          failures: {initial.healthFailureCount}
          {initial.lastHealthCheckAt
            ? ` · Last checked ${formatDate(initial.lastHealthCheckAt)}`
            : ""}
        </p>
        <form
          className="grid gap-3 sm:grid-cols-3"
          onSubmit={(e) => {
            e.preventDefault()
            action.mutate({
              action: "healthPolicy",
              appName: name,
              intervalSeconds: interval,
              timeoutSeconds: timeout,
              failureThreshold: failures,
            })
          }}
        >
          <label className={fieldClass}>
            Interval (seconds)
            <Input
              type="number"
              min={0}
              value={interval}
              onChange={(e) => setInterval(Number(e.target.value))}
            />
          </label>
          <label className={fieldClass}>
            Probe timeout (seconds)
            <Input
              type="number"
              min={1}
              value={timeout}
              onChange={(e) => setTimeout(Number(e.target.value))}
            />
          </label>
          <label className={fieldClass}>
            Failure threshold
            <Input
              type="number"
              min={1}
              value={failures}
              onChange={(e) => setFailures(Number(e.target.value))}
            />
          </label>
          <Button type="submit" disabled={action.isPending} className="w-fit">
            Save health policy
          </Button>
        </form>
      </FeatureCard>
      <FeatureCard
        title="Crash recovery"
        description="Restart the promoted release after an unexpected exit. Retry limits and increasing delays prevent endless crash loops."
      >
        <p className="text-xs">
          Attempts: {initial.restartAttempt} ·{" "}
          {initial.crashLoop ? "Crash loop detected" : "No crash loop"}
          {initial.nextRestartAt
            ? ` · Next restart ${formatDate(initial.nextRestartAt)}`
            : ""}
        </p>
        <form
          className="grid gap-3"
          onSubmit={(e) => {
            e.preventDefault()
            action.mutate({
              action: "restartPolicy",
              appName: name,
              enabled,
              maxAttempts,
              baseDelaySeconds: baseDelay,
            })
          }}
        >
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={enabled}
              onChange={(e) => setEnabled(e.target.checked)}
            />
            Enable automatic restart
          </label>
          <div className="grid gap-3 sm:grid-cols-2">
            <label className={fieldClass}>
              Maximum attempts
              <Input
                type="number"
                min={1}
                max={20}
                value={maxAttempts}
                onChange={(e) => setMaxAttempts(Number(e.target.value))}
              />
            </label>
            <label className={fieldClass}>
              Initial delay (seconds)
              <Input
                type="number"
                min={1}
                max={300}
                value={baseDelay}
                onChange={(e) => setBaseDelay(Number(e.target.value))}
              />
            </label>
          </div>
          <Button type="submit" className="w-fit" disabled={action.isPending}>
            Save recovery policy
          </Button>
        </form>
      </FeatureCard>
      <FeatureCard
        title="Deployment preflight"
        description="Check required variables and estimated memory overlap before deploying. Limits are ceilings rather than reservations."
      >
        <Button
          variant="outline"
          disabled={preflight.isFetching}
          onClick={() => void preflight.refetch()}
        >
          Check deployment readiness
        </Button>
        {preflight.isError ? (
          <p role="alert" className="text-sm text-red-700">
            {preflight.error.message}
          </p>
        ) : null}
        {preflight.data ? (
          <div className="space-y-2 text-sm">
            <p>
              {preflight.data.preflight.ready
                ? "Ready for deployment"
                : "Review before deploying"}
            </p>
            <p>{preflight.data.preflight.capacity.message}</p>
            {preflight.data.preflight.missingRequiredEnvironmentKeys.length ? (
              <p className="text-red-700">
                Missing:{" "}
                {preflight.data.preflight.missingRequiredEnvironmentKeys.join(
                  ", "
                )}
              </p>
            ) : null}
          </div>
        ) : null}
      </FeatureCard>
      <FeatureCard
        title="Release retention"
        description="Keep the active and protected rollback/in-flight releases. Retention removes old artifacts while preserving deployment history."
      >
        <form
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => {
            e.preventDefault()
            action.mutate({
              action: "retention",
              appName: name,
              retainReleaseCount: retention,
            })
          }}
        >
          <label className={fieldClass}>
            Releases to retain
            <Input
              type="number"
              min={2}
              value={retention}
              onChange={(e) => setRetention(Number(e.target.value))}
            />
          </label>
          <Button disabled={action.isPending} type="submit">
            Save retention and prune
          </Button>
        </form>
        <Button
          variant="outline"
          disabled={action.isPending}
          onClick={() =>
            action.mutate({ action: "pruneReleases", appName: name })
          }
        >
          Prune eligible releases now
        </Button>
      </FeatureCard>
      <FeatureCard
        title="Delete project"
        description="Stop the app and remove its routes, source configuration, and project history. Database retention is your choice."
      >
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={preserveData}
            onChange={(e) => setPreserveData(e.target.checked)}
          />
          Preserve database files
        </label>
        <label className={fieldClass}>
          Type {name} to confirm deletion
          <Input
            value={confirmation}
            onChange={(e) => setConfirmation(e.target.value)}
          />
        </label>
        <Button
          variant="destructive"
          disabled={confirmation !== name || remove.isPending}
          onClick={() =>
            remove.mutate({ action: "delete", appName: name, preserveData })
          }
        >
          Delete project
        </Button>
      </FeatureCard>
    </div>
  )
}
export function AttemptTimeline({
  name,
  onSelect,
}: {
  name: string
  onSelect?: (id: string) => void
}) {
  const [offset, setOffset] = useState(0)
  const [selected, setSelected] = useState("")
  const [eventOffset, setEventOffset] = useState(0)
  const history = useQuery({
    queryKey: ["harbor", "attempts", name, offset],
    queryFn: () =>
      featureCall<AttemptHistory>("operations", {
        action: "history",
        appName: name,
        limit: 20,
        offset,
      }),
    refetchInterval: 5000,
  })
  const events = useQuery({
    queryKey: ["harbor", "events", name, selected, eventOffset],
    queryFn: () =>
      featureCall<{
        hasMore: boolean
        total: number
        events: Array<{
          id: number
          type: string
          payload?: string
          createdAt: string
        }>
      }>("operations", {
        action: "events",
        appName: name,
        deploymentId: selected,
        limit: 50,
        offset: eventOffset,
      }),
    enabled: Boolean(selected),
    refetchInterval: 5000,
  })
  return (
    <FeatureCard
      title="All deployment attempts"
      description="Includes failed builds, canceled candidates, restarts, and rollbacks."
    >
      {history.isError ? (
        <p role="alert" className="text-red-700">
          {history.error.message}
        </p>
      ) : null}
      <div className="overflow-x-auto">
        <table className="w-full text-left text-xs">
          <thead>
            <tr>
              <th className="pb-2">Attempt</th>
              <th>State</th>
              <th>Created</th>
              <th>Stage durations</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {history.data?.deployments.map(({ deployment: d, stages }) => (
              <tr key={d.id} className="border-t">
                <td className="py-3 font-mono">{d.id.slice(-8)}</td>
                <td>
                  {d.state}
                  {d.error ? (
                    <p className="max-w-xs text-red-700">{d.error}</p>
                  ) : null}
                </td>
                <td>{formatDate(d.createdAt)}</td>
                <td>
                  {Object.entries(stages).map(([key, ms]) => (
                    <p key={key}>
                      {key.replace("Milliseconds", "")}:{" "}
                      {((ms ?? 0) / 1000).toFixed(2)}s
                    </p>
                  ))}
                </td>
                <td>
                  <Button
                    variant="outline"
                    size="xs"
                    onClick={() => {
                      setSelected(d.id)
                      setEventOffset(0)
                      onSelect?.(d.id)
                    }}
                  >
                    View events
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="flex gap-2">
        <Button
          variant="outline"
          size="sm"
          disabled={offset === 0}
          onClick={() => setOffset(Math.max(0, offset - 20))}
        >
          Previous
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={!history.data?.hasMore}
          onClick={() => setOffset(offset + 20)}
        >
          Next
        </Button>
        <span className="self-center text-xs">
          {history.data?.total ?? 0} attempts
        </span>
      </div>
      {events.isError ? (
        <p role="alert" className="text-red-700">
          {events.error.message}
        </p>
      ) : null}
      {selected ? (
        <div className="flex flex-wrap gap-2">
          <Button
            size="sm"
            variant="outline"
            disabled={!eventOffset}
            onClick={() => setEventOffset(Math.max(0, eventOffset - 50))}
          >
            Previous events
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={!events.data?.hasMore}
            onClick={() => setEventOffset(eventOffset + 50)}
          >
            Next events
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={!events.data?.total}
            onClick={() =>
              setEventOffset(
                Math.max(
                  0,
                  (Math.ceil((events.data?.total ?? 0) / 50) - 1) * 50
                )
              )
            }
          >
            Latest events
          </Button>
          <span className="self-center text-xs">
            {events.data?.total ?? 0} events
          </span>
        </div>
      ) : null}
      {selected ? (
        <ol className="border-l-2 border-green-200 pl-4 text-xs">
          {events.data?.events.map((e) => (
            <li key={e.id} className="py-2">
              <p className="font-medium">{e.type}</p>
              <p className="text-muted-foreground">{formatDate(e.createdAt)}</p>
              {e.payload ? <p className="mt-1 break-all">{e.payload}</p> : null}
            </li>
          ))}
        </ol>
      ) : null}
    </FeatureCard>
  )
}
