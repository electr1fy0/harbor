import { QueryClientProvider, type QueryClient } from "@tanstack/react-query"
import { Outlet, createRootRouteWithContext } from "@tanstack/react-router"
import { TanStackDevtools } from "@tanstack/react-devtools"
import { TanStackRouterDevtoolsPanel } from "@tanstack/react-router-devtools"
import { Toaster } from "@/components/ui/sonner"
import { AppShell } from "@/components/app/app-shell"
import { PageError } from "@/components/app/page-state"

interface RouterContext {
  queryClient: QueryClient
}

export const Route = createRootRouteWithContext<RouterContext>()({
  head: () => ({
    meta: [
      { charSet: "utf-8" },
      { name: "viewport", content: "width=device-width, initial-scale=1" },
      { name: "theme-color", content: "#f8f9f8" },
      { title: "Harbor" },
      {
        name: "description",
        content: "Manage Harbor applications and deployments.",
      },
    ],
  }),
  component: RootLayout,
  errorComponent: RootError,
  notFoundComponent: NotFound,
})

function RootLayout() {
  const { queryClient } = Route.useRouteContext()

  return (
    <QueryClientProvider client={queryClient}>
      <AppShell>
        <Outlet />
      </AppShell>
      <Toaster position="bottom-right" theme="light" />
      {import.meta.env.DEV ? (
        <TanStackDevtools
          config={{ position: "bottom-right" }}
          plugins={[
            {
              name: "TanStack Router",
              render: <TanStackRouterDevtoolsPanel />,
            },
          ]}
        />
      ) : null}
    </QueryClientProvider>
  )
}

function RootError({ error, reset }: { error: unknown; reset: () => void }) {
  return (
    <main className="mx-auto flex min-h-[60vh] max-w-3xl items-center px-6 py-12">
      <PageError
        message={
          error instanceof Error
            ? error.message
            : "An unexpected error occurred."
        }
        onRetry={reset}
      />
    </main>
  )
}

function NotFound() {
  return (
    <main className="mx-auto flex min-h-[60vh] max-w-xl flex-col items-center justify-center px-6 text-center">
      <span className="text-xs font-semibold tracking-[0.14em] text-muted-foreground uppercase">
        404
      </span>
      <h1 className="mt-3 text-2xl font-semibold tracking-tight">
        Page not found
      </h1>
    </main>
  )
}
