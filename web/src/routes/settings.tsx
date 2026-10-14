import { useQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"
import {
  IconArrowRight,
  IconBrandGithub,
  IconCircleCheck,
  IconPlugConnected,
  IconPlugConnectedX,
  IconShieldLock,
} from "@tabler/icons-react"
import { Button, buttonVariants } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { ConnectionBanner } from "@/components/app/connection-banner"
import { PageLoading } from "@/components/app/page-state"
import { PageHeader } from "@/components/app/page-header"
import { overviewQueryOptions } from "@/lib/queries"
import { FeatureCard, useFeatureAction } from "@/components/app/feature-card"

export const Route = createFileRoute("/settings")({ component: SettingsPage })

function SettingsPage() {
  const overview = useQuery(overviewQueryOptions())
  const pruneCache = useFeatureAction("operations")

  if (overview.isPending) return <PageLoading rows={2} />
  const connected = Boolean(overview.data?.connected)

  return (
    <>
      <PageHeader title="Settings" />
      {!connected || overview.data?.error || overview.isError ? (
        <ConnectionBanner
          connected={connected}
          error={
            overview.data?.error ||
            (overview.isError ? overview.error.message : undefined)
          }
        />
      ) : null}

      <div className="grid max-w-4xl gap-4">
        <FeatureCard
          title="Build cache cleanup"
          description="Remove cached dependencies and build intermediates when no build holds the cache. The next build repopulates it."
        >
          <Button
            variant="outline"
            disabled={pruneCache.isPending || !connected}
            onClick={() => pruneCache.mutate({ action: "pruneCache" })}
          >
            {pruneCache.isPending ? "Cleaning…" : "Prune build cache"}
          </Button>
        </FeatureCard>
        <Card className="border-border/80 bg-white">
          <CardHeader className="flex-row items-start gap-3 border-b border-border/60 pb-4">
            <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-[#f5f6f4] text-muted-foreground">
              {connected ? (
                <IconPlugConnected size={18} />
              ) : (
                <IconPlugConnectedX size={18} />
              )}
            </span>
            <div className="min-w-0">
              <CardTitle className="text-sm">Harbor control plane</CardTitle>
            </div>
          </CardHeader>
          <CardContent className="flex flex-col gap-3 pt-4 sm:flex-row sm:items-center sm:justify-between">
            <div className="flex items-center gap-2 text-sm">
              {connected ? (
                <IconCircleCheck size={16} className="text-[#37866c]" />
              ) : (
                <span className="size-2 rounded-full bg-[#c89028]" />
              )}
              <span className="font-medium">
                {connected ? "Connected" : "Not connected"}
              </span>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void overview.refetch()}
              disabled={overview.isFetching}
            >
              {overview.isFetching ? "Checking…" : "Check connection"}
            </Button>
          </CardContent>
        </Card>

        <Card className="border-border/80 bg-white">
          <CardHeader className="flex-row items-start gap-3 border-b border-border/60 pb-4">
            <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-[#f5f6f4] text-foreground">
              <IconBrandGithub size={18} />
            </span>
            <div className="min-w-0">
              <CardTitle className="text-sm">GitHub sources</CardTitle>
            </div>
          </CardHeader>
          <CardContent className="space-y-4 pt-4">
            <div className="flex gap-3 text-sm leading-6 text-muted-foreground">
              <IconShieldLock
                size={17}
                className="mt-0.5 shrink-0 text-muted-foreground"
              />
              <p>
                Public repositories need no token. For private repositories,
                connect a GitHub token from the import page. Harbor stores it in
                the daemon's private data directory. A daemon-side{" "}
                <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs text-foreground">
                  GITHUB_TOKEN
                </code>{" "}
                is also supported.
              </p>
            </div>
            <div className="flex justify-end border-t border-border/60 pt-4">
              <Link
                to="/new"
                className={buttonVariants({ size: "sm", variant: "outline" })}
              >
                Import repository <IconArrowRight data-icon="inline-end" />
              </Link>
            </div>
          </CardContent>
        </Card>

        <details
          open={!connected}
          className="group rounded-xl border border-border/80 bg-white"
        >
          <summary className="flex cursor-pointer list-none items-center justify-between gap-4 px-4 py-3.5 text-sm font-medium marker:hidden [&::-webkit-details-marker]:hidden">
            Local setup
            <span className="text-xs font-normal text-muted-foreground group-open:hidden">
              Show commands
            </span>
            <span className="hidden text-xs font-normal text-muted-foreground group-open:inline">
              Hide commands
            </span>
          </summary>
          <div className="space-y-4 border-t border-border/60 px-4 py-4 text-sm text-muted-foreground">
            <div>
              <p className="mb-1.5 text-[10px] font-semibold tracking-[0.1em] text-muted-foreground uppercase">
                Terminal 1 · repository root
              </p>
              <pre className="overflow-x-auto rounded-lg bg-[#202726] p-3 font-mono text-xs leading-5 text-[#e6ebe7]">
                <code>
                  {
                    "make build\n./bin/paasd -socket /tmp/paasd.sock -data-dir /tmp/harbor-dev -listen 127.0.0.1:8080 -web-listen 127.0.0.1:3000 -sandbox host"
                  }
                </code>
              </pre>
            </div>
            <div>
              <p className="mb-1.5 text-[10px] font-semibold tracking-[0.1em] text-muted-foreground uppercase">
                Terminal 2 · dashboard
              </p>
              <pre className="overflow-x-auto rounded-lg bg-[#202726] p-3 font-mono text-xs leading-5 text-[#e6ebe7]">
                <code>{"cd web\nbun install\nbun run dev"}</code>
              </pre>
            </div>
            <p className="text-xs leading-5">
              Vite serves the SPA on port 5173 and proxies API requests to the
              Go dashboard on port 3000. The Go daemon accesses the control
              socket directly.
            </p>
          </div>
        </details>
      </div>
    </>
  )
}
