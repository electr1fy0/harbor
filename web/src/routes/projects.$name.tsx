import { useCallback, useMemo, useState, type ReactNode } from "react"
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
import { appQueryOptions, overviewQueryOptions } from "@/lib/queries"
import { rollbackApp, stopApp } from "@/lib/harbor.functions"
import type { ReleaseView } from "@/lib/types"

export const Route = createFileRoute("/projects/$name")({
  component: ProjectDetailPage,
})

const releaseFeatures = tableFeatures({})
const releaseColumn = createColumnHelper<typeof releaseFeatures, ReleaseView>()

type PendingAction =
  { kind: "stop" } | { kind: "rollback"; release: ReleaseView }

function ProjectDetailPage() {
  const { name } = Route.useParams()
  const queryClient = useQueryClient()
  const details = useQuery(appQueryOptions(name))
  const [pendingAction, setPendingAction] = useState<PendingAction | null>(null)

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
  const releases = details.data.releases
  const canStop =
    deployment &&
    [
      "QUEUED",
      "BUILDING",
      "BUILT",
      "STARTING",
      "HEALTH_CHECKING",
      "RUNNING",
    ].includes(deployment.State)
  const canCancelAttempt =
    deployment && ["QUEUED", "BUILDING", "BUILT"].includes(deployment.State)
  const mutationPending = stopMutation.isPending || rollbackMutation.isPending
  const mutationError =
    stopMutation.error?.message || rollbackMutation.error?.message

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
            <StatusBadge state={deployment?.State ?? "Not deployed"} />
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
                <CardTitle className="text-sm">Current deployment</CardTitle>
              </div>
            </div>
            {deployment ? <StatusBadge state={deployment.State} /> : null}
          </CardHeader>
          {deployment ? (
            <CardContent className="grid gap-x-6 gap-y-4 pt-4 sm:grid-cols-2">
              <DetailField
                label="Deployment ID"
                value={
                  <span className="font-mono text-xs">
                    {shortHash(deployment.ID)}
                  </span>
                }
              />
              <DetailField
                label="Last updated"
                value={formatDate(deployment.StartedAt || deployment.CreatedAt)}
              />
              <DetailField label="Port" value={deployment.Port || "—"} />
              <DetailField
                label="Release"
                value={
                  <span className="font-mono text-xs">
                    {shortHash(deployment.ReleaseID)}
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
              {deployment.Error ? (
                <div className="sm:col-span-2">
                  <Separator className="mb-3" />
                  <p className="text-[10px] font-semibold tracking-[0.1em] text-muted-foreground uppercase">
                    Latest error
                  </p>
                  <p className="mt-1.5 text-sm leading-6 text-[#a4463f]">
                    {deployment.Error}
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
                <p className="text-sm font-medium">No deployment yet</p>
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
              value={deployment?.MemoryLimit || "—"}
            />
            <DetailField
              label="CPU quota"
              value={
                deployment?.CPUQuota
                  ? `${Number(deployment.CPUQuota.toFixed(2))} vCPU`
                  : "—"
              }
            />
            <DetailField
              label="Tasks limit"
              value={deployment?.TasksLimit || "—"}
            />
            <DetailField
              label="Health path"
              value={deployment?.HealthPath || "—"}
            />
          </CardContent>
        </Card>
      </div>

      <section className="mt-7" aria-label="Deployment activity">
        <Tabs defaultValue="history" className="gap-4">
          <div className="flex flex-col gap-3 border-b border-border/70 sm:flex-row sm:items-center sm:justify-between">
            <TabsList variant="line" className="h-9 rounded-none px-0">
              <TabsTrigger value="history" className="flex-none gap-1.5 px-2.5">
                <IconClock size={14} />
                Release history
              </TabsTrigger>
              <TabsTrigger value="logs" className="flex-none gap-1.5 px-2.5">
                <IconTerminal2 size={14} />
                Logs
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
            {details.data.logs.trim() ? (
              <pre className="max-h-[540px] overflow-auto rounded-xl border border-[#242927] bg-[#202726] p-4 font-mono text-xs leading-6 text-[#e6ebe7] shadow-sm">
                <code>{details.data.logs}</code>
              </pre>
            ) : (
              <Card className="border-dashed bg-white/70 shadow-none">
                <CardContent className="py-8 text-center text-sm text-muted-foreground">
                  No logs are available for the latest deployment attempt.
                </CardContent>
              </Card>
            )}
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
                : `Harbor will make release ${pendingAction?.kind === "rollback" ? shortHash(pendingAction.release.id) : ""} active for ${name}.`}
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
                  rollbackMutation.mutate(pendingAction.release.id)
              }}
            >
              {mutationPending
                ? "Working…"
                : pendingAction?.kind === "stop" &&
                    deployment &&
                    ["QUEUED", "BUILDING", "BUILT"].includes(deployment.State)
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
              disabled={row.original.isActive || rollbackPending}
              onClick={() => onRollback(row.original)}
            >
              <IconRotateClockwise data-icon="inline-start" /> Roll back
            </Button>
          ),
        }),
      ]),
    [onRollback, rollbackPending]
  )
  const table = useTable({ features: releaseFeatures, columns, data: releases })

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
