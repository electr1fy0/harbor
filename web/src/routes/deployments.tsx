import { useMemo } from "react"
import {
  createColumnHelper,
  tableFeatures,
  useTable,
} from "@tanstack/react-table"
import { useQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"
import { IconArrowUpRight, IconActivity, IconBox } from "@tabler/icons-react"
import { buttonVariants } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { ConnectionBanner } from "@/components/app/connection-banner"
import { formatDate, shortHash } from "@/components/app/format"
import { PageError, PageLoading } from "@/components/app/page-state"
import { PageHeader } from "@/components/app/page-header"
import { StatusBadge } from "@/components/app/status-badge"
import { overviewQueryOptions } from "@/lib/queries"
import type { AppView } from "@/lib/types"

export const Route = createFileRoute("/deployments")({
  component: DeploymentsPage,
})

const features = tableFeatures({})
const emptyApps: AppView[] = []
const column = createColumnHelper<typeof features, AppView>()
const columns = column.columns([
  column.accessor((app) => app.Application.Name, {
    id: "project",
    header: "Project",
    cell: ({ row }) => (
      <Link
        to="/projects/$name"
        params={{ name: row.original.Application.Name }}
        className="group flex w-fit items-center gap-2.5 font-medium hover:text-primary-foreground"
      >
        <span className="grid size-8 place-items-center rounded-md bg-[#f5f6f4] text-muted-foreground">
          <IconBox size={16} />
        </span>
        {row.original.Application.Name}
      </Link>
    ),
  }),
  column.accessor((app) => app.LatestAttempt?.ID ?? "", {
    id: "attempt",
    header: "Latest attempt",
    cell: ({ getValue }) => (
      <span className="font-mono text-xs text-muted-foreground">
        {shortHash(getValue())}
      </span>
    ),
  }),
  column.accessor((app) => app.LatestAttempt?.State ?? "Unknown", {
    id: "status",
    header: "Attempt status",
    cell: ({ getValue }) => <StatusBadge state={getValue()} />,
  }),
  column.accessor((app) => app.Production?.State ?? "No production route", {
    id: "production",
    header: "Production",
    cell: ({ getValue }) => <StatusBadge state={getValue()} />,
  }),
  column.accessor((app) => app.LatestAttempt?.CreatedAt ?? "", {
    id: "started",
    header: "Attempt time",
    cell: ({ getValue }) => (
      <span className="text-xs text-muted-foreground">
        {formatDate(getValue())}
      </span>
    ),
  }),
  column.accessor((app) => app.LatestAttempt?.Error ?? "", {
    id: "details",
    header: "Details",
    cell: ({ getValue }) => (
      <span
        className="block max-w-[280px] truncate text-xs text-muted-foreground"
        title={getValue()}
      >
        {getValue() || "—"}
      </span>
    ),
  }),
])

function DeploymentsPage() {
  const overview = useQuery(overviewQueryOptions())
  const apps = useMemo(
    () =>
      (overview.data?.apps ?? emptyApps).filter(
        (app) => app.LatestAttempt !== null
      ),
    [overview.data?.apps]
  )
  const table = useTable({ features, columns, data: apps })

  if (overview.isPending) return <PageLoading rows={5} />
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
        title="Deployments"
        actions={
          <Link to="/new" className={buttonVariants({ size: "default" })}>
            Import project
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

      {apps.length === 0 ? (
        <Card className="border-dashed bg-white/70 shadow-none">
          <CardContent className="flex flex-col items-center py-14 text-center">
            <span className="grid size-11 place-items-center rounded-xl bg-muted/70 text-muted-foreground">
              <IconActivity size={20} />
            </span>
            <h2 className="mt-4 text-sm font-semibold">
              {overview.data.connected
                ? "No deployments yet"
                : "Deployments unavailable"}
            </h2>
            <Link
              to="/new"
              className={buttonVariants({ className: "mt-5", size: "sm" })}
            >
              Import a project
            </Link>
          </CardContent>
        </Card>
      ) : (
        <div className="overflow-hidden rounded-xl border border-border/80 bg-white">
          <div className="overflow-x-auto">
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
                    <TableHead className="w-10" />
                  </TableRow>
                ))}
              </TableHeader>
              <TableBody>
                {table.getRowModel().rows.map((row) => (
                  <TableRow key={row.id} className="group/row">
                    {row.getAllCells().map((cell) => (
                      <TableCell key={cell.id} className="py-3.5">
                        {" "}
                        <table.FlexRender cell={cell} />{" "}
                      </TableCell>
                    ))}
                    <TableCell className="pr-4 text-right">
                      <Link
                        to="/projects/$name"
                        params={{ name: row.original.Application.Name }}
                        aria-label={`Open ${row.original.Application.Name}`}
                        className="inline-flex size-7 items-center justify-center rounded-md text-muted-foreground opacity-60 group-hover/row:opacity-100 hover:bg-muted hover:text-foreground"
                      >
                        <IconArrowUpRight size={15} />
                      </Link>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </div>
      )}
    </>
  )
}
