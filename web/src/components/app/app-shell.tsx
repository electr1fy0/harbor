import {
  IconActivity,
  IconApps,
  IconLayersIntersect,
  IconSettings,
  IconShip,
} from "@tabler/icons-react"
import { Link, useRouterState } from "@tanstack/react-router"
import type { ReactNode } from "react"
import { buttonVariants } from "@/components/ui/button"
import { cn } from "@/lib/utils"

const navigation = [
  {
    label: "Projects",
    to: "/",
    icon: IconApps,
    match: (path: string) =>
      path === "/" || path.startsWith("/projects/") || path === "/new",
  },
  {
    label: "Deployments",
    to: "/deployments",
    icon: IconActivity,
    match: (path: string) => path.startsWith("/deployments"),
  },
  {
    label: "Services",
    to: "/services",
    icon: IconLayersIntersect,
    match: (path: string) => path.startsWith("/services"),
  },
  {
    label: "Settings",
    to: "/settings",
    icon: IconSettings,
    match: (path: string) => path.startsWith("/settings"),
  },
] as const

export function AppShell({ children }: { children: ReactNode }) {
  const pathname = useRouterState({
    select: (state) => state.location.pathname,
  })

  return (
    <div className="min-h-svh bg-[#f8f9f8] text-foreground">
      <aside className="fixed inset-y-0 left-0 z-30 hidden w-[236px] flex-col border-r border-border/80 bg-white/90 px-3 py-4 md:flex">
        <Brand />
        <nav aria-label="Main navigation" className="mt-7 space-y-1">
          {navigation.map((item) => {
            const active = item.match(pathname)
            const Icon = item.icon
            return (
              <Link
                key={item.to}
                to={item.to}
                aria-current={active ? "page" : undefined}
                className={cn(
                  "group flex h-9 items-center gap-2.5 rounded-lg px-2.5 text-[13px] font-medium",
                  active
                    ? "bg-primary/15 text-foreground"
                    : "text-muted-foreground hover:bg-muted/70 hover:text-foreground"
                )}
              >
                <Icon
                  size={17}
                  stroke={1.75}
                  className={cn(
                    active
                      ? "text-primary-foreground"
                      : "text-muted-foreground group-hover:text-foreground"
                  )}
                />
                {item.label}
              </Link>
            )
          })}
        </nav>
      </aside>

      <div className="md:pl-[236px]">
        <header className="sticky top-0 z-20 border-b border-border/70 bg-white/90 px-4 py-3 backdrop-blur md:hidden">
          <div className="flex items-center justify-between gap-3">
            <Brand compact />
            <Link to="/new" className={buttonVariants({ size: "sm" })}>
              Import project
            </Link>
          </div>
          <nav
            aria-label="Main navigation"
            className="mt-3 flex gap-1 overflow-x-auto pb-0.5"
          >
            {navigation.map((item) => {
              const active = item.match(pathname)
              const Icon = item.icon
              return (
                <Link
                  key={item.to}
                  to={item.to}
                  aria-current={active ? "page" : undefined}
                  className={cn(
                    "flex h-8 shrink-0 items-center gap-1.5 rounded-md px-2.5 text-xs font-medium",
                    active
                      ? "bg-primary/15 text-foreground"
                      : "text-muted-foreground hover:bg-muted/60"
                  )}
                >
                  <Icon size={15} stroke={1.8} />
                  {item.label}
                </Link>
              )
            })}
          </nav>
        </header>

        <header className="hidden h-14 items-center justify-end border-b border-border/70 bg-white/80 px-8 md:flex">
          <span className="text-[11px] text-muted-foreground">Local</span>
        </header>

        <main className="mx-auto min-h-[calc(100svh-57px)] w-full max-w-[1440px] px-4 py-7 sm:px-6 lg:px-9 lg:py-9">
          {children}
        </main>
      </div>
    </div>
  )
}

function Brand({ compact = false }: { compact?: boolean }) {
  return (
    <Link
      to="/"
      className="flex w-fit items-center gap-2.5 rounded-md outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      <span className="grid size-8 place-items-center rounded-[10px] bg-[#202726] text-primary shadow-sm">
        <IconShip size={18} stroke={1.9} />
      </span>
      <span
        className={cn(
          "text-[16px] font-semibold tracking-[-0.04em] text-[#17211f]",
          compact && "text-[15px]"
        )}
      >
        harbor
      </span>
    </Link>
  )
}
