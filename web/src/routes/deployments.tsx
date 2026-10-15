import { useState } from "react"
import { useQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { PageHeader } from "@/components/app/page-header"
import { PageError, PageLoading } from "@/components/app/page-state"
import { StatusBadge } from "@/components/app/status-badge"
import { formatDate } from "@/components/app/format"
import type { AttemptHistory } from "@/components/app/app-operations"
import { featureCall } from "@/lib/features"

export const Route = createFileRoute("/deployments")({
  component: DeploymentsPage,
})
function DeploymentsPage() {
  const [offset, setOffset] = useState(0)
  const [search, setSearch] = useState("")
  const [state, setState] = useState("")
  const query = useQuery({
    queryKey: ["harbor", "all-attempts", offset],
    queryFn: () =>
      featureCall<AttemptHistory>("operations", {
        action: "historyAll",
        limit: 50,
        offset,
      }),
    refetchInterval: 5000,
  })
  if (query.isPending) return <PageLoading rows={5} />
  if (query.isError && !query.data)
    return (
      <PageError
        message={query.error?.message ?? "Deployment history is unavailable."}
        onRetry={() => void query.refetch()}
      />
    )
  const entries = query.data.deployments.filter(
    (e) =>
      (!state || e.deployment.state === state) &&
      [e.deployment.appName, e.deployment.id, e.deployment.releaseId]
        .join(" ")
        .toLowerCase()
        .includes(search.trim().toLowerCase())
  )
  return (
    <>
      <PageHeader title="Deployments" />
      {query.isError ? (
        <p role="status" className="mb-3 text-xs text-amber-800">
          Latest deployment data could not be refreshed: {query.error?.message}
        </p>
      ) : null}
      <p className="mb-4 text-sm text-muted-foreground">
        All attempts across your projects, including failed builds, canceled
        candidates, restarts, and rollbacks.
      </p>
      <div className="mb-4 flex flex-wrap gap-3">
        <Input
          aria-label="Search deployment page"
          className="max-w-sm"
          placeholder="Filter this page by project or ID"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <select
          aria-label="Filter deployment state"
          className="h-9 rounded-md border px-2 text-xs"
          value={state}
          onChange={(e) => setState(e.target.value)}
        >
          <option value="">All states</option>
          {[
            "QUEUED",
            "BUILDING",
            "BUILT",
            "STARTING",
            "HEALTH_CHECKING",
            "RUNNING",
            "BUILD_FAILED",
            "START_FAILED",
            "UNHEALTHY",
            "CRASHED",
            "OOM_KILLED",
            "STOPPED",
          ].map((s) => (
            <option key={s}>{s}</option>
          ))}
        </select>
      </div>
      <div className="overflow-x-auto rounded-xl border bg-white">
        <table className="w-full text-left text-xs">
          <thead className="bg-muted/30">
            <tr>
              {[
                "Project",
                "Attempt",
                "Status",
                "Created",
                "Build",
                "Total",
                "Details",
              ].map((h) => (
                <th key={h} className="px-4 py-3">
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {entries.map(({ deployment: d, stages }) => (
              <tr className="border-t" key={d.id}>
                <td className="px-4 py-3">
                  <Link
                    to="/projects/$name"
                    params={{ name: d.appName }}
                    className="font-medium underline"
                  >
                    {d.appName}
                  </Link>
                </td>
                <td className="px-4 font-mono">{d.id.slice(-8)}</td>
                <td className="px-4">
                  <StatusBadge state={d.state} />
                </td>
                <td className="px-4">{formatDate(d.createdAt)}</td>
                <td className="px-4">
                  {stages.buildMilliseconds != null
                    ? `${(stages.buildMilliseconds / 1000).toFixed(2)}s`
                    : "—"}
                </td>
                <td className="px-4">
                  {stages.totalMilliseconds != null
                    ? `${(stages.totalMilliseconds / 1000).toFixed(2)}s`
                    : "—"}
                </td>
                <td className="max-w-sm px-4">{d.error || "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {!entries.length ? (
          <p className="p-8 text-center text-sm text-muted-foreground">
            No deployment attempts match this page.
          </p>
        ) : null}
      </div>
      <div className="mt-4 flex items-center gap-3">
        <Button
          variant="outline"
          disabled={!offset}
          onClick={() => setOffset(Math.max(0, offset - 50))}
        >
          Previous
        </Button>
        <Button
          variant="outline"
          disabled={!query.data.hasMore}
          onClick={() => setOffset(offset + 50)}
        >
          Next
        </Button>
        <p className="text-xs text-muted-foreground">
          {query.data.total} attempts · Page {Math.floor(offset / 50) + 1}
        </p>
      </div>
    </>
  )
}
