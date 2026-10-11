import { useQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"
import { IconDatabase, IconServer, IconStack2 } from "@tabler/icons-react"
import { Card, CardContent } from "@/components/ui/card"
import { ConnectionBanner } from "@/components/app/connection-banner"
import { formatRelative } from "@/components/app/format"
import { PageError, PageLoading } from "@/components/app/page-state"
import { PageHeader } from "@/components/app/page-header"
import { StatusBadge } from "@/components/app/status-badge"
import { overviewQueryOptions } from "@/lib/queries"

export const Route = createFileRoute("/services")({ component: ServicesPage })

function ServicesPage() {
  const overview = useQuery(overviewQueryOptions())

  if (overview.isPending) return <PageLoading rows={3} />
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

  const services = overview.data.services

  return (
    <>
      <PageHeader title="Services" />
      {!overview.data.connected || overview.data.error || overview.isError ? (
        <ConnectionBanner
          connected={overview.data.connected}
          error={
            overview.data.error ||
            (overview.isError ? overview.error.message : undefined)
          }
        />
      ) : null}

      {services.length === 0 ? (
        <Card className="border-dashed bg-white/70 shadow-none">
          <CardContent className="flex flex-col items-center py-14 text-center">
            <span className="grid size-11 place-items-center rounded-xl bg-muted/70 text-muted-foreground">
              <IconStack2 size={21} />
            </span>
            <h2 className="mt-4 text-sm font-semibold">
              {overview.data.connected
                ? "No services yet"
                : "Services unavailable"}
            </h2>
          </CardContent>
        </Card>
      ) : (
        <div className="overflow-hidden rounded-xl border border-border/80 bg-white">
          <div className="hidden grid-cols-[minmax(0,1.8fr)_minmax(90px,0.8fr)_minmax(80px,0.6fr)_minmax(95px,0.8fr)_minmax(90px,0.8fr)] gap-4 border-b border-border/70 bg-[#fbfcfb] px-4 py-3 text-[10px] font-semibold tracking-[0.12em] text-muted-foreground uppercase sm:grid sm:px-5">
            <span>Service</span>
            <span>Type / version</span>
            <span>Port</span>
            <span>Status</span>
            <span>Created</span>
          </div>
          <div className="divide-y divide-border/60">
            {services.map((service) => (
              <div
                key={service.id}
                className="grid grid-cols-1 gap-3 px-4 py-4 sm:grid-cols-[minmax(0,1.8fr)_minmax(90px,0.8fr)_minmax(80px,0.6fr)_minmax(95px,0.8fr)_minmax(90px,0.8fr)] sm:items-center sm:gap-4 sm:px-5"
              >
                <div className="flex min-w-0 items-center gap-3">
                  <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-[#f5f6f4] text-primary-foreground">
                    <IconDatabase size={18} />
                  </span>
                  <div className="min-w-0">
                    <p className="truncate text-sm font-medium">
                      {service.name}
                    </p>
                    <p className="mt-0.5 truncate font-mono text-[10px] text-muted-foreground">
                      {service.id}
                    </p>
                  </div>
                </div>
                <div className="flex items-center gap-2 sm:block">
                  <span className="text-[10px] font-semibold text-muted-foreground uppercase sm:hidden">
                    Type / version
                  </span>
                  <span className="text-sm text-foreground">
                    {service.type}
                    {service.version ? (
                      <span className="text-muted-foreground">
                        {" "}
                        · {service.version}
                      </span>
                    ) : null}
                  </span>
                </div>
                <div className="flex items-center gap-2 sm:block">
                  <span className="text-[10px] font-semibold text-muted-foreground uppercase sm:hidden">
                    Port
                  </span>
                  <span className="inline-flex items-center gap-1.5 font-mono text-xs text-muted-foreground">
                    <IconServer size={13} />
                    {service.port || "—"}
                  </span>
                </div>
                <div className="flex items-center gap-2 sm:block">
                  <span className="text-[10px] font-semibold text-muted-foreground uppercase sm:hidden">
                    Status
                  </span>
                  <StatusBadge state={service.state || "Unknown"} />
                </div>
                <div className="flex items-center gap-2 sm:block">
                  <span className="text-[10px] font-semibold text-muted-foreground uppercase sm:hidden">
                    Created
                  </span>
                  <span className="text-xs text-muted-foreground">
                    {formatRelative(service.createdAt)}
                  </span>
                </div>
              </div>
            ))}
          </div>
        </div>
      )}
    </>
  )
}
