import { useMemo, useState, type ReactNode } from "react"
import { useQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"
import {
  IconArrowUpRight,
  IconBox,
  IconCirclePlus,
  IconSearch,
  IconServer,
  IconWorld,
} from "@tabler/icons-react"
import { Button, buttonVariants } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { ConnectionBanner } from "@/components/app/connection-banner"
import { formatRelative } from "@/components/app/format"
import { PageError, PageLoading } from "@/components/app/page-state"
import { PageHeader } from "@/components/app/page-header"
import { StatusBadge } from "@/components/app/status-badge"
import { overviewQueryOptions } from "@/lib/queries"
import type { AppView } from "@/lib/types"

export const Route = createFileRoute("/")({ component: ProjectsPage })

function ProjectsPage() {
  const [search, setSearch] = useState("")
  const overview = useQuery(overviewQueryOptions())
  const apps = overview.data?.apps ?? []
  const filteredApps = useMemo(
    () =>
      apps.filter((app) =>
        app.Application.Name.toLowerCase().includes(search.trim().toLowerCase())
      ),
    [apps, search]
  )
  const running = apps.filter(
    (app) => app.Production?.State.toLowerCase() === "running"
  ).length
  const failed = apps.filter((app) =>
    [
      "failed",
      "error",
      "build_failed",
      "start_failed",
      "unhealthy",
      "crashed",
      "oom_killed",
    ].includes(app.LatestAttempt?.State.toLowerCase() ?? "")
  ).length

  if (overview.isPending) return <PageLoading rows={4} />
  if (overview.isError && !overview.data) {
    return (
      <PageError
        message={
          overview.error.message ||
          "The Harbor control plane could not be reached."
        }
        onRetry={() => void overview.refetch()}
      />
    )
  }

  return (
    <>
      <PageHeader
        title="Projects"
        actions={
          <Link to="/new" className={buttonVariants({ size: "default" })}>
            <IconCirclePlus data-icon="inline-start" /> Import project
          </Link>
        }
      />

      {!overview.data.connected || overview.data.error || overview.isError ? (
        <ConnectionBanner
          connected={overview.data.connected}
          error={
            overview.data.error ||
            (overview.isError ? overview.error.message : undefined)
          }
        />
      ) : null}

      {apps.length > 0 ? (
        <div className="mb-6 flex flex-wrap items-center gap-x-5 gap-y-2 rounded-xl border border-border/70 bg-white px-4 py-3.5">
          <SummaryMetric
            icon={<IconBox size={16} />}
            label="Projects"
            value={apps.length}
          />
          <span className="hidden h-6 w-px bg-border sm:block" />
          <SummaryMetric
            icon={<IconWorld size={16} />}
            label="Running"
            value={running}
            tone="green"
          />
          <span className="hidden h-6 w-px bg-border sm:block" />
          <SummaryMetric
            icon={<IconServer size={16} />}
            label="Needs attention"
            value={failed}
            tone={failed > 0 ? "red" : undefined}
          />
        </div>
      ) : null}

      <section aria-label="Projects">
        {apps.length > 0 ? (
          <div className="mb-3 flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
            <h2 className="text-[15px] font-semibold tracking-tight">
              All projects
            </h2>
            <div className="relative w-full sm:max-w-[270px]">
              <IconSearch
                size={16}
                className="pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2 text-muted-foreground"
              />
              <Input
                value={search}
                onChange={(event) => setSearch(event.target.value)}
                placeholder="Search projects"
                className="h-9 pl-8"
                aria-label="Search projects"
              />
            </div>
          </div>
        ) : null}

        {apps.length === 0 ? (
          <NoProjects connected={overview.data.connected} />
        ) : filteredApps.length === 0 ? (
          <Card className="border-dashed bg-white/70 shadow-none">
            <CardContent className="flex flex-col items-center py-12 text-center">
              <span className="grid size-10 place-items-center rounded-xl bg-muted text-muted-foreground">
                <IconSearch size={18} />
              </span>
              <h3 className="mt-3 text-sm font-semibold">
                No projects match “{search}”
              </h3>
              <Button
                variant="outline"
                size="sm"
                className="mt-4"
                onClick={() => setSearch("")}
              >
                Clear search
              </Button>
            </CardContent>
          </Card>
        ) : (
          <div className="grid gap-3 xl:grid-cols-2">
            {filteredApps.map((app) => (
              <ProjectCard key={app.Application.ID} app={app} />
            ))}
          </div>
        )}
      </section>
    </>
  )
}

function SummaryMetric({
  icon,
  label,
  value,
  tone,
}: {
  icon: ReactNode
  label: string
  value: number
  tone?: "green" | "red"
}) {
  return (
    <div className="flex items-center gap-2.5">
      <span
        className={
          tone === "green"
            ? "text-[#37866c]"
            : tone === "red"
              ? "text-[#b2544c]"
              : "text-muted-foreground"
        }
      >
        {icon}
      </span>
      <span className="text-sm font-semibold text-[#202a27] tabular-nums">
        {value}
      </span>
      <span className="text-xs text-muted-foreground">{label}</span>
    </div>
  )
}

function ProjectCard({ app }: { app: AppView }) {
  const production = app.Production
  const latestAttempt = app.LatestAttempt
  const state = production?.State ?? latestAttempt?.State ?? "Not deployed"
  const separateAttempt =
    latestAttempt !== null && latestAttempt.ID !== production?.ID

  return (
    <Link
      to="/projects/$name"
      params={{ name: app.Application.Name }}
      className="group block rounded-xl outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      <Card className="h-full border-border/80 bg-white group-hover:border-[#c5d9d5] group-hover:shadow-sm">
        <CardContent className="flex min-h-[118px] flex-col justify-between gap-4 py-4">
          <div className="flex items-start justify-between gap-3">
            <div className="flex min-w-0 items-center gap-3">
              <span className="grid size-9 shrink-0 place-items-center rounded-lg border border-border/70 bg-[#fafcfb] text-primary-foreground">
                <IconBox size={18} stroke={1.7} />
              </span>
              <div className="min-w-0">
                <h3 className="truncate text-sm font-semibold tracking-[-0.015em] group-hover:text-primary-foreground">
                  {app.Application.Name}
                </h3>
              </div>
            </div>
            <IconArrowUpRight
              size={16}
              className="mt-0.5 shrink-0 text-muted-foreground/60 group-hover:text-primary-foreground"
            />
          </div>
          <div className="flex flex-wrap items-center justify-between gap-2 border-t border-border/60 pt-3">
            <StatusBadge state={state} />
            <span className="text-[11px] text-muted-foreground">
              {latestAttempt
                ? `Updated ${formatRelative(latestAttempt.CreatedAt)}`
                : "No deployment yet"}
            </span>
          </div>
          {separateAttempt ? (
            <div className="flex flex-wrap items-center gap-x-2 gap-y-1 border-t border-border/50 pt-2">
              <span className="text-[11px] text-muted-foreground">
                Latest attempt
              </span>
              <StatusBadge state={latestAttempt.State} />
              {latestAttempt.Error ? (
                <p className="line-clamp-1 min-w-0 text-xs text-[#a4463f]">
                  {latestAttempt.Error}
                </p>
              ) : null}
            </div>
          ) : latestAttempt?.Error ? (
            <p className="line-clamp-1 text-xs text-[#a4463f]">
              {latestAttempt.Error}
            </p>
          ) : null}
        </CardContent>
      </Card>
    </Link>
  )
}

function NoProjects({ connected }: { connected: boolean }) {
  return (
    <Card className="border-dashed bg-white/70 shadow-none">
      <CardContent className="flex flex-col items-center py-14 text-center">
        <span className="grid size-11 place-items-center rounded-xl bg-primary/15 text-primary-foreground">
          <IconBox size={20} />
        </span>
        <h3 className="mt-4 text-sm font-semibold">
          {connected ? "No projects yet" : "No projects available"}
        </h3>
        <Link
          to="/new"
          className={buttonVariants({ className: "mt-5", size: "sm" })}
        >
          Import a project
        </Link>
      </CardContent>
    </Card>
  )
}
