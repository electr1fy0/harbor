import {
  useCallback,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import {
  createColumnHelper,
  tableFeatures,
  useTable,
} from "@tanstack/react-table"
import { createFileRoute, Link } from "@tanstack/react-router"
import {
  IconArrowLeft,
  IconArrowUpRight,
  IconClock,
  IconExternalLink,
  IconPlayerStop,
  IconRotateClockwise,
  IconServer,
  IconTerminal2,
} from "@tabler/icons-react"
import { toast } from "sonner"
import { Button, buttonVariants } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Separator } from "@/components/ui/separator"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { formatDate, shortHash } from "@/components/app/format"
import { PageError, PageLoading } from "@/components/app/page-state"
import { StatusBadge } from "@/components/app/status-badge"
import { ProjectSettings } from "@/components/app/project-settings"
import { AdvancedLogs } from "@/components/app/advanced-logs"
import { AppObservability } from "@/components/app/app-observability"
import { AppAutomation } from "@/components/app/app-automation"
import { FailureExplanation } from "@/components/app/failure-explanation"
import {
  OperationsPanel,
  AttemptTimeline,
  useOperations,
  type AttemptHistory,
} from "@/components/app/app-operations"
import { EnvironmentImport } from "@/components/app/environment-import"
import { useFeatureAction } from "@/components/app/feature-card"
import { featureCall } from "@/lib/features"
import {
  appQueryOptions,
  deploymentJobQueryOptions,
  overviewQueryOptions,
} from "@/lib/queries"
import {
  retryGitHubDeployment,
  rollbackApp,
  stopApp,
} from "@/lib/harbor.functions"
import type { ReleaseView } from "@/lib/types"

export const Route = createFileRoute("/projects/$name")({
  component: ProjectDetailPage,
})

const releaseFeatures = tableFeatures({})
const releaseColumn = createColumnHelper<typeof releaseFeatures, ReleaseView>()

type PendingAction =
  | { kind: "stop" }
  | { kind: "rollback"; release: ReleaseView | null; releaseID?: string }

function ProjectDetailPage() {
  const { name } = Route.useParams()
  return <ProjectDetailContent key={name} name={name} />
}
function ProjectDetailContent({ name }: { name: string }) {
  const queryClient = useQueryClient()
  const details = useQuery(appQueryOptions(name))
  const operations = useOperations(name)
  const operationalAction = useFeatureAction("operations")
  const sourceAvailability = useQuery({
    queryKey: ["harbor", "source", name],
    queryFn: () => featureCall("automation", { action: "getSource", name }),
    retry: false,
  })
  const attemptsQuery = useQuery({
    queryKey: ["harbor", "attempts", name, "selector"],
    queryFn: () =>
      featureCall<AttemptHistory>("operations", {
        action: "history",
        appName: name,
        limit: 100,
      }),
    refetchInterval: 5000,
  })
  const [pendingAction, setPendingAction] = useState<PendingAction | null>(null)
  const [activeTab, setActiveTab] = useState("history")
  const [jobID, setJobID] = useState("")
  const jobQuery = useQuery({
    ...deploymentJobQueryOptions(jobID),
    enabled: Boolean(jobID),
  })

  const refresh = useCallback(async () => {
    await Promise.all([
      queryClient.invalidateQueries({
        queryKey: appQueryOptions(name).queryKey,
      }),
      queryClient.invalidateQueries({
        queryKey: overviewQueryOptions().queryKey,
      }),
    ])
  }, [name, queryClient])

  const stopMutation = useMutation({
    mutationFn: () => stopApp({ data: { name } }),
    onSuccess: async () => {
      await refresh()
      setPendingAction(null)
      toast.success(`Stop requested for ${name}`)
    },
    onError: (error) => toast.error(error.message || `Could not stop ${name}`),
  })

  const rollbackMutation = useMutation({
    mutationFn: (releaseID: string) =>
      rollbackApp({ data: { name, releaseID } }),
    onSuccess: async (deployment) => {
      await refresh()
      setPendingAction(null)
      if (deployment.deployment?.State === "RUNNING") {
        toast.success(`Rollback completed for ${name}`)
      } else {
        toast.error(
          deployment.deployment?.Error ||
            `Rollback for ${name} did not reach a running state.`
        )
      }
    },
    onError: (error) =>
      toast.error(error.message || `Could not roll back ${name}`),
  })

  const retryMutation = useMutation({
    mutationFn: () => retryGitHubDeployment({ data: { name } }),
    onSuccess: async (job) => {
      setJobID(job.id)
      queryClient.setQueryData(["harbor", "job", job.id], job)
      await refresh()
      toast.success(`Redeployment queued for ${name}`)
    },
    onError: (error) => toast.error(error.message || `Could not retry ${name}`),
  })

  useEffect(() => {
    if (
      jobQuery.data &&
      ["SUCCEEDED", "FAILED", "CANCELED"].includes(jobQuery.data.state)
    ) {
      void refresh()
    }
  }, [jobQuery.data?.state, refresh])

  if (details.isPending) return <PageLoading rows={4} />
  if (details.isError && !details.data) {
    return (
      <PageError
        message={details.error.message || `Could not load ${name}.`}
        onRetry={() => void details.refetch()}
      />
    )
  }

  const deployment = details.data.deployment
  const diagnostics = details.data.diagnostics
  const production = diagnostics
    ? diagnostics.production
    : deployment?.State === "RUNNING"
      ? deployment
      : null
  const latestAttempt = diagnostics ? diagnostics.latestAttempt : deployment
  const runtimeDeployment = production ?? latestAttempt
  const releases = details.data.releases
  const canStop =
    production?.State === "RUNNING" ||
    Boolean(
      latestAttempt &&
      [
        "QUEUED",
        "BUILDING",
        "BUILT",
        "STARTING",
        "HEALTH_CHECKING",
        "RUNNING",
      ].includes(latestAttempt.State)
    )
  const canCancelAttempt =
    latestAttempt &&
    ["QUEUED", "BUILDING", "BUILT"].includes(latestAttempt.State)
  const mutationPending =
    stopMutation.isPending ||
    rollbackMutation.isPending ||
    retryMutation.isPending
  const mutationError =
    stopMutation.error?.message ||
    rollbackMutation.error?.message ||
    retryMutation.error?.message

  const diagnosticAction = (id: string) =>
    diagnostics?.actions.find((action) => action.id === id)

  return (
    <>
      <div className="mb-5">
        <Link
          to="/"
          className="inline-flex items-center gap-1.5 text-xs font-medium text-muted-foreground hover:text-foreground"
        >
          <IconArrowLeft size={14} /> Projects
        </Link>
      </div>

      <div className="mb-6 flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2.5">
            <h1 className="max-w-full truncate text-[26px] leading-tight font-semibold tracking-[-0.045em] text-[#18211f] sm:text-[30px]">
              {name}
            </h1>
            <StatusBadge
              state={
                production?.State ?? latestAttempt?.State ?? "Not deployed"
              }
            />
          </div>
        </div>
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          {details.data.url ? (
            <a
              href={details.data.url}
              target="_blank"
              rel="noreferrer"
              className={buttonVariants({ variant: "outline", size: "sm" })}
            >
              Open app <IconExternalLink data-icon="inline-end" />
            </a>
          ) : null}
          <Button
            variant="outline"
            size="sm"
            disabled={
              operationalAction.isPending ||
              (!production &&
                !releases.some((release) => release.artifactAvailable))
            }
            onClick={() =>
              operationalAction.mutate({
                action: production ? "restart" : "start",
                appName: name,
              })
            }
          >
            {production ? "Restart" : "Start"}
          </Button>
          {sourceAvailability.data ? (
            <Button
              size="sm"
              disabled={retryMutation.isPending}
              onClick={() => retryMutation.mutate()}
            >
              Redeploy
            </Button>
          ) : null}
          {latestAttempt &&
          latestAttempt.ID !== production?.ID &&
          [
            "QUEUED",
            "BUILDING",
            "BUILT",
            "STARTING",
            "HEALTH_CHECKING",
          ].includes(latestAttempt.State) ? (
            <Button
              variant="outline"
              size="sm"
              disabled={operationalAction.isPending}
              onClick={() =>
                operationalAction.mutate({
                  action: "cancelCandidate",
                  appName: name,
                  deploymentId: latestAttempt.ID,
                })
              }
            >
              Cancel deployment
            </Button>
          ) : null}
          {canStop ? (
            <Button
              variant="outline"
              size="sm"
              onClick={() => setPendingAction({ kind: "stop" })}
              disabled={mutationPending}
            >
              <IconPlayerStop data-icon="inline-start" /> Stop app
            </Button>
          ) : null}
        </div>
      </div>

      {operations.data?.operations.pendingSettings ? (
        <div className="mb-5 flex flex-wrap items-center justify-between gap-3 rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 text-sm">
          <p>Saved configuration has not been applied to the running app.</p>
          <Button
            variant="outline"
            size="sm"
            disabled={operationalAction.isPending}
            onClick={() =>
              operationalAction.mutate({
                action: production ? "restart" : "start",
                appName: name,
              })
            }
          >
            Apply saved configuration
          </Button>
        </div>
      ) : null}
      {details.isError ? (
        <div className="mb-5 rounded-lg border border-[#edc8c5] bg-[#fcf0ef] px-3 py-2 text-sm text-[#a4463f]">
          {details.error.message}
        </div>
      ) : null}
      {mutationError ? (
        <div
          role="alert"
          className="mb-5 flex items-start justify-between gap-3 rounded-lg border border-[#edc8c5] bg-[#fcf0ef] px-3 py-2.5 text-sm text-[#a4463f]"
        >
          <span>{mutationError}</span>
          <button
            type="button"
            aria-label="Dismiss error"
            className="text-xs font-medium underline underline-offset-2"
            onClick={() => {
              stopMutation.reset()
              rollbackMutation.reset()
              retryMutation.reset()
            }}
          >
            Dismiss
          </button>
        </div>
      ) : null}

      <div className="grid gap-4 lg:grid-cols-[minmax(0,1.45fr)_minmax(250px,0.8fr)]">
        <Card className="border-border/80 bg-white">
          <CardHeader className="flex-row items-center justify-between gap-3 border-b border-border/60 pb-4">
            <div className="flex items-center gap-2.5">
              <span className="grid size-8 place-items-center rounded-lg bg-[#f5f6f4] text-muted-foreground">
                <IconServer size={17} />
              </span>
              <div>
                <CardTitle className="text-sm">Production</CardTitle>
              </div>
            </div>
            {production ? <StatusBadge state={production.State} /> : null}
          </CardHeader>
          {production ? (
            <CardContent className="grid gap-x-6 gap-y-4 pt-4 sm:grid-cols-2">
              <DetailField
                label="Deployment ID"
                value={
                  <span className="font-mono text-xs">
                    {shortHash(production.ID)}
                  </span>
                }
              />
              <DetailField
                label="Last updated"
                value={formatDate(production.StartedAt || production.CreatedAt)}
              />
              <DetailField label="Port" value={production.Port || "—"} />
              <DetailField
                label="Release"
                value={
                  <span className="font-mono text-xs">
                    {shortHash(production.ReleaseID)}
                  </span>
                }
              />
              {details.data.url ? (
                <DetailField
                  label="Application URL"
                  value={
                    <a
                      href={details.data.url}
                      target="_blank"
                      rel="noreferrer"
                      className="inline-flex max-w-full items-center gap-1 truncate text-primary-foreground hover:underline"
                    >
                      <span className="truncate">{details.data.url}</span>
                      <IconArrowUpRight size={13} />
                    </a>
                  }
                />
              ) : null}
              {production.Error ? (
                <div className="sm:col-span-2">
                  <Separator className="mb-3" />
                  <p className="text-[10px] font-semibold tracking-[0.1em] text-muted-foreground uppercase">
                    Latest error
                  </p>
                  <p className="mt-1.5 text-sm leading-6 text-[#a4463f]">
                    {production.Error}
                  </p>
                </div>
              ) : null}
            </CardContent>
          ) : (
            <CardContent className="flex items-start gap-3 py-5">
              <span className="grid size-8 shrink-0 place-items-center rounded-lg bg-muted/70 text-muted-foreground">
                <IconServer size={16} />
              </span>
              <div>
                <p className="text-sm font-medium">
                  No production route is serving traffic.
                </p>
                <p className="mt-1 text-xs text-muted-foreground">
                  {latestAttempt
                    ? "The latest attempt is shown in deployment diagnostics."
                    : "Deploy the application to start production."}
                </p>
              </div>
            </CardContent>
          )}
        </Card>

        <Card className="border-border/80 bg-white">
          <CardHeader className="flex-row items-center gap-2.5 border-b border-border/60 pb-4">
            <span className="grid size-8 place-items-center rounded-lg bg-[#f5f6f4] text-muted-foreground">
              <IconClock size={17} />
            </span>
            <div>
              <CardTitle className="text-sm">Runtime</CardTitle>
            </div>
          </CardHeader>
          <CardContent className="grid grid-cols-2 gap-x-4 gap-y-4 pt-4">
            <DetailField
              label="Memory"
              value={runtimeDeployment?.MemoryLimit || "—"}
            />
            <DetailField
              label="CPU quota"
              value={
                runtimeDeployment?.CPUQuota
                  ? `${Number(runtimeDeployment.CPUQuota.toFixed(2))} vCPU`
                  : "—"
              }
            />
            <DetailField
              label="Tasks limit"
              value={runtimeDeployment?.TasksLimit || "—"}
            />
            <DetailField
              label="Health path"
              value={runtimeDeployment?.HealthPath || "—"}
            />
          </CardContent>
        </Card>
      </div>

      {diagnostics ? (
        <Card
          className="mt-4 border-border/80 bg-white"
          aria-label="Deployment diagnostics"
        >
          <CardHeader className="flex-row items-start justify-between gap-3 border-b border-border/60 pb-4">
            <div>
              <CardTitle className="text-sm">Deployment diagnostics</CardTitle>
              <p className="mt-1 text-xs text-muted-foreground">
                Production health and the latest deployment attempt are tracked
                separately.
              </p>
            </div>
            {latestAttempt ? <StatusBadge state={latestAttempt.State} /> : null}
          </CardHeader>
          <CardContent className="space-y-4 pt-4">
            <div className="grid gap-4 sm:grid-cols-2">
              <div>
                <p className="text-[10px] font-semibold tracking-[0.1em] text-muted-foreground uppercase">
                  Production
                </p>
                <div className="mt-1.5 flex flex-wrap items-center gap-2">
                  <StatusBadge
                    state={production?.State ?? "No production route"}
                  />
                  {production ? (
                    <span className="font-mono text-xs text-muted-foreground">
                      {shortHash(production.ID)}
                    </span>
                  ) : null}
                </div>
              </div>
              <div>
                <p className="text-[10px] font-semibold tracking-[0.1em] text-muted-foreground uppercase">
                  Latest attempt
                </p>
                <div className="mt-1.5 flex flex-wrap items-center gap-2">
                  {latestAttempt ? (
                    <>
                      <StatusBadge state={latestAttempt.State} />
                      <span className="font-mono text-xs text-muted-foreground">
                        {shortHash(latestAttempt.ID)}
                      </span>
                    </>
                  ) : (
                    <span className="text-sm text-muted-foreground">
                      None recorded
                    </span>
                  )}
                </div>
              </div>
            </div>
            {diagnostics.summary ? (
              <p className="text-sm leading-6 text-foreground">
                {diagnostics.summary}
              </p>
            ) : null}
            {diagnostics.excerpt ? (
              <pre className="max-h-40 overflow-auto rounded-lg border border-[#242927] bg-[#202726] p-3 font-mono text-[11px] leading-5 text-[#e6ebe7]">
                <code>{diagnostics.excerpt}</code>
              </pre>
            ) : null}
            {latestAttempt ? (
              <FailureExplanation
                name={name}
                deployment={latestAttempt}
                summary={diagnostics.summary}
                excerpt={diagnostics.excerpt}
                production={production}
              />
            ) : null}
            {jobID && (jobQuery.data || jobQuery.isError) ? (
              <div className="rounded-lg border border-border/70 bg-[#fbfcfb] px-3 py-2.5 text-sm">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-medium">GitHub redeployment</span>
                  <StatusBadge
                    state={
                      jobQuery.isError
                        ? "Status unavailable"
                        : jobQuery.data.state
                    }
                  />
                  {jobQuery.data && !jobQuery.isError ? (
                    <span className="text-xs text-muted-foreground">
                      {jobQuery.data.stage.toLowerCase().replaceAll("_", " ")}
                    </span>
                  ) : null}
                </div>
                {jobQuery.isError ? (
                  <div className="mt-2 flex flex-wrap items-center justify-between gap-2">
                    <p role="alert" className="text-xs text-[#a4463f]">
                      {jobQuery.error.message ||
                        "Redeployment status is unavailable; retry the status check."}
                    </p>
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={jobQuery.isFetching}
                      onClick={() => void jobQuery.refetch()}
                    >
                      {jobQuery.isFetching ? "Checking…" : "Retry status"}
                    </Button>
                  </div>
                ) : jobQuery.data.error ? (
                  <p className="mt-1.5 text-xs text-[#a4463f]">
                    {jobQuery.data.error}
                  </p>
                ) : null}
              </div>
            ) : null}
            <div className="flex flex-wrap gap-2">
              {diagnosticAction("view_attempt_logs")?.available ? (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setActiveTab("logs")}
                >
                  <IconTerminal2 data-icon="inline-start" /> View failed attempt
                  logs
                </Button>
              ) : null}
              {diagnosticAction("configure_settings")?.available ? (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setActiveTab("settings")}
                >
                  Configure memory or environment
                </Button>
              ) : null}
              {diagnosticAction("retry_github_deploy")?.available ? (
                <Button
                  variant="outline"
                  size="sm"
                  disabled={retryMutation.isPending}
                  onClick={() => retryMutation.mutate()}
                >
                  <IconRotateClockwise data-icon="inline-start" />
                  {retryMutation.isPending
                    ? "Queueing…"
                    : "Retry GitHub deployment"}
                </Button>
              ) : null}
              {diagnosticAction("rollback_release")?.available ? (
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() =>
                    setPendingAction({
                      kind: "rollback",
                      release: null,
                      releaseID:
                        diagnosticAction("rollback_release")?.releaseID,
                    })
                  }
                >
                  <IconRotateClockwise data-icon="inline-start" /> Roll back
                  last good release
                </Button>
              ) : null}
            </div>
            {diagnosticAction("retry_github_deploy")?.available === false ? (
              <p className="text-xs text-muted-foreground">
                {diagnosticAction("retry_github_deploy")?.reason}
              </p>
            ) : null}
          </CardContent>
        </Card>
      ) : null}

      <section className="mt-7" aria-label="Deployment activity">
        <Tabs
          value={activeTab}
          onValueChange={(value) => setActiveTab(String(value ?? "history"))}
          className="gap-4"
        >
          <div className="flex flex-col gap-3 border-b border-border/70 sm:flex-row sm:items-center sm:justify-between">
            <TabsList
              variant="line"
              className="h-auto flex-wrap rounded-none px-0"
            >
              <TabsTrigger value="history" className="flex-none gap-1.5 px-2.5">
                <IconClock size={14} />
                Release history
              </TabsTrigger>
              <TabsTrigger value="logs" className="flex-none gap-1.5 px-2.5">
                <IconTerminal2 size={14} />
                Logs
              </TabsTrigger>
              <TabsTrigger value="attempts">Attempts</TabsTrigger>
              <TabsTrigger value="automation">Automation</TabsTrigger>
              <TabsTrigger value="observability">Metrics & routing</TabsTrigger>
              <TabsTrigger value="operations">Operations</TabsTrigger>
              <TabsTrigger value="settings" className="flex-none px-2.5">
                Settings
              </TabsTrigger>
            </TabsList>
            <span className="pb-2 text-[11px] text-muted-foreground">
              {releases.length} {releases.length === 1 ? "release" : "releases"}
            </span>
          </div>
          <TabsContent value="history">
            {releases.length === 0 ? (
              <Card className="border-dashed bg-white/70 shadow-none">
                <CardContent className="py-8 text-center text-sm text-muted-foreground">
                  No release history is available for this application yet.
                </CardContent>
              </Card>
            ) : (
              <ReleaseHistoryTable
                releases={releases}
                onRollback={(release) =>
                  setPendingAction({ kind: "rollback", release })
                }
                rollbackPending={rollbackMutation.isPending}
              />
            )}
          </TabsContent>
          <TabsContent value="logs">
            <AdvancedLogs
              name={name}
              productionId={production?.ID}
              attempts={(attemptsQuery.data?.deployments ?? []).map(
                (entry) => ({
                  id: entry.deployment.id,
                  state: entry.deployment.state,
                })
              )}
            />
          </TabsContent>
          <TabsContent value="attempts">
            <AttemptTimeline name={name} />
          </TabsContent>
          <TabsContent value="automation">
            <AppAutomation name={name} />
          </TabsContent>
          <TabsContent value="observability">
            <AppObservability name={name} />
          </TabsContent>
          <TabsContent value="operations">
            <OperationsPanel name={name} />
          </TabsContent>
          <TabsContent value="settings">
            <div className="grid gap-4">
              <ProjectSettings
                key={name}
                name={name}
                settings={details.data.settings}
                onSaved={async () => refresh()}
              />
              <EnvironmentImport
                key={name}
                name={name}
                existingKeys={details.data.settings.environmentKeys}
              />
            </div>
          </TabsContent>
        </Tabs>
      </section>

      <Dialog
        open={pendingAction !== null}
        onOpenChange={(open) => {
          if (!open && !mutationPending) setPendingAction(null)
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {pendingAction?.kind === "stop"
                ? canCancelAttempt
                  ? "Cancel deployment?"
                  : "Stop application?"
                : "Roll back application?"}
            </DialogTitle>
            <DialogDescription>
              {pendingAction?.kind === "stop"
                ? canCancelAttempt
                  ? `Cancel the in-progress deployment for ${name}?`
                  : `${name} will stop serving traffic. You can deploy it again later.`
                : pendingAction?.kind === "rollback" && pendingAction.release
                  ? `Harbor will make release ${shortHash(pendingAction.release.id)} active for ${name}.`
                  : pendingAction?.kind === "rollback" &&
                      pendingAction.releaseID
                    ? `Harbor will make release ${shortHash(pendingAction.releaseID)} active for ${name}.`
                    : `Harbor will restore the last good release for ${name}.`}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setPendingAction(null)}
              disabled={mutationPending}
            >
              Cancel
            </Button>
            <Button
              variant={
                pendingAction?.kind === "stop" ? "destructive" : "default"
              }
              disabled={mutationPending}
              onClick={() => {
                if (pendingAction?.kind === "stop") stopMutation.mutate()
                if (pendingAction?.kind === "rollback")
                  rollbackMutation.mutate(
                    pendingAction.release?.id ?? pendingAction.releaseID ?? ""
                  )
              }}
            >
              {mutationPending
                ? "Working…"
                : pendingAction?.kind === "stop" &&
                    latestAttempt &&
                    ["QUEUED", "BUILDING", "BUILT"].includes(
                      latestAttempt.State
                    )
                  ? "Cancel attempt"
                  : pendingAction?.kind === "stop"
                    ? "Stop application"
                    : "Start rollback"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

function DetailField({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="min-w-0">
      <p className="text-[10px] font-semibold tracking-[0.1em] text-muted-foreground uppercase">
        {label}
      </p>
      <div className="mt-1.5 truncate text-sm font-medium text-foreground">
        {value}
      </div>
    </div>
  )
}

function ReleaseHistoryTable({
  releases,
  onRollback,
  rollbackPending,
}: {
  releases: ReleaseView[]
  onRollback: (release: ReleaseView) => void
  rollbackPending: boolean
}) {
  const columns = useMemo(
    () =>
      releaseColumn.columns([
        releaseColumn.accessor("sourceHash", {
          id: "source",
          header: "Source",
          cell: ({ getValue }) => (
            <span className="font-mono text-xs text-muted-foreground">
              {shortHash(getValue())}
            </span>
          ),
        }),
        releaseColumn.accessor("runtime", {
          id: "runtime",
          header: "Runtime",
          cell: ({ getValue }) => (
            <span className="text-xs text-muted-foreground">
              {getValue() || "—"}
            </span>
          ),
        }),
        releaseColumn.accessor("createdAt", {
          id: "created",
          header: "Created",
          cell: ({ getValue }) => (
            <span className="text-xs text-muted-foreground">
              {formatDate(getValue())}
            </span>
          ),
        }),
        releaseColumn.accessor("isActive", {
          id: "state",
          header: "Release",
          cell: ({ getValue }) =>
            getValue() ? (
              <StatusBadge state="Active" />
            ) : (
              <span className="text-xs text-muted-foreground">Previous</span>
            ),
        }),
        releaseColumn.display({
          id: "actions",
          header: "",
          cell: ({ row }) => (
            <Button
              variant="outline"
              size="xs"
              disabled={
                row.original.isActive ||
                !row.original.artifactAvailable ||
                rollbackPending
              }
              onClick={() => onRollback(row.original)}
            >
              <IconRotateClockwise data-icon="inline-start" />{" "}
              {row.original.artifactAvailable ? "Roll back" : "Artifact pruned"}
            </Button>
          ),
        }),
      ]),
    [onRollback, rollbackPending]
  )
  const table = useTable({
    features: releaseFeatures,
    columns,
    data: releases,
  })

  return (
    <div className="overflow-x-auto rounded-xl border border-border/80 bg-white">
      <Table>
        <TableHeader>
          {table.getHeaderGroups().map((group) => (
            <TableRow
              key={group.id}
              className="bg-[#fbfcfb] hover:bg-[#fbfcfb]"
            >
              {group.headers.map((header) => (
                <TableHead
                  key={header.id}
                  className="h-10 text-[10px] font-semibold tracking-[0.1em] whitespace-nowrap uppercase"
                >
                  {header.isPlaceholder ? null : (
                    <table.FlexRender header={header} />
                  )}
                </TableHead>
              ))}
            </TableRow>
          ))}
        </TableHeader>
        <TableBody>
          {table.getRowModel().rows.map((row) => (
            <TableRow key={row.id}>
              {row.getAllCells().map((cell) => (
                <TableCell key={cell.id} className="py-3 whitespace-nowrap">
                  {" "}
                  <table.FlexRender cell={cell} />{" "}
                </TableCell>
              ))}
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}
