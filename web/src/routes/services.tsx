import { useQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"
import { FeatureCard } from "@/components/app/feature-card"
import { ConnectionBanner } from "@/components/app/connection-banner"
import { PageError, PageLoading } from "@/components/app/page-state"
import { PageHeader } from "@/components/app/page-header"
import {
  DataServiceTools,
  type DataServiceSummary,
} from "@/components/app/service-tools"
import { featureCall } from "@/lib/features"
import { overviewQueryOptions } from "@/lib/queries"

export const Route = createFileRoute("/services")({ component: ServicesPage })

interface DataServicesResponse {
  services: DataServiceSummary[]
}

function ServicesPage() {
  const overview = useQuery(overviewQueryOptions())
  const dataServices = useQuery({
    queryKey: ["harbor", "data-services"],
    queryFn: () =>
      featureCall<DataServicesResponse>("dataServices", { action: "list" }),
    enabled: overview.data?.connected ?? false,
    refetchInterval: 30_000,
    retry: false,
  })

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
      {overview.data.connected ? (
        dataServices.isPending ? (
          <PageLoading rows={3} />
        ) : dataServices.isError && !dataServices.data ? (
          <FeatureCard title="Data services are unavailable">
            <p role="alert" className="text-sm text-red-700">
              {dataServices.error?.message}
            </p>
            <button
              className="text-xs font-medium underline"
              onClick={() => void dataServices.refetch()}
            >
              Try again
            </button>
          </FeatureCard>
        ) : (
          <>
            {dataServices.isError ? (
              <p role="status" className="mb-3 text-xs text-amber-800">
                Latest service data could not be refreshed:{" "}
                {dataServices.error?.message}
              </p>
            ) : null}
            <DataServiceTools
              services={dataServices.data.services}
              apps={overview.data.apps}
            />
          </>
        )
      ) : null}
    </>
  )
}
