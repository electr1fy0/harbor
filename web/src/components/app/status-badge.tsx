import { IconCircleCheckFilled, IconLoader2, IconX } from "@tabler/icons-react"
import { Badge } from "@/components/ui/badge"
import { cn } from "@/lib/utils"

const statusStyles: Record<string, string> = {
  active: "border-[#bfe4d2] bg-[#ecf8f1] text-[#26714f]",
  running: "border-[#bfe4d2] bg-[#ecf8f1] text-[#26714f]",
  ready: "border-[#bfe4d2] bg-[#ecf8f1] text-[#26714f]",
  deployed: "border-[#bfe4d2] bg-[#ecf8f1] text-[#26714f]",
  succeeded: "border-[#bfe4d2] bg-[#ecf8f1] text-[#26714f]",
  success: "border-[#bfe4d2] bg-[#ecf8f1] text-[#26714f]",
  building: "border-[#ead9ad] bg-[#fbf6e8] text-[#8b6b1b]",
  deploying: "border-[#ead9ad] bg-[#fbf6e8] text-[#8b6b1b]",
  pending: "border-[#ead9ad] bg-[#fbf6e8] text-[#8b6b1b]",
  queued: "border-[#ead9ad] bg-[#fbf6e8] text-[#8b6b1b]",
  built: "border-[#ead9ad] bg-[#fbf6e8] text-[#8b6b1b]",
  starting: "border-[#ead9ad] bg-[#fbf6e8] text-[#8b6b1b]",
  health_checking: "border-[#ead9ad] bg-[#fbf6e8] text-[#8b6b1b]",
  failed: "border-[#edc8c5] bg-[#fcf0ef] text-[#a4463f]",
  error: "border-[#edc8c5] bg-[#fcf0ef] text-[#a4463f]",
  build_failed: "border-[#edc8c5] bg-[#fcf0ef] text-[#a4463f]",
  start_failed: "border-[#edc8c5] bg-[#fcf0ef] text-[#a4463f]",
  unhealthy: "border-[#edc8c5] bg-[#fcf0ef] text-[#a4463f]",
  crashed: "border-[#edc8c5] bg-[#fcf0ef] text-[#a4463f]",
  oom_killed: "border-[#edc8c5] bg-[#fcf0ef] text-[#a4463f]",
  stopped: "border-border bg-muted/50 text-muted-foreground",
  canceled: "border-border bg-muted/50 text-muted-foreground",
  cancelled: "border-border bg-muted/50 text-muted-foreground",
}

function readableState(state: string) {
  return state
    .toLowerCase()
    .replace(/[_-]+/g, " ")
    .replace(/\b\w/g, (letter) => letter.toUpperCase())
}

export function StatusBadge({
  state,
  className,
}: {
  state: string
  className?: string
}) {
  const normalized = state.trim().toLowerCase()
  const busy = [
    "building",
    "deploying",
    "pending",
    "queued",
    "built",
    "starting",
    "health_checking",
  ].includes(normalized)
  const failed = [
    "failed",
    "error",
    "build_failed",
    "start_failed",
    "unhealthy",
    "crashed",
    "oom_killed",
  ].includes(normalized)

  return (
    <Badge
      variant="outline"
      className={cn(
        "gap-1.5 px-2 font-medium",
        statusStyles[normalized] ?? "bg-background text-muted-foreground",
        className
      )}
    >
      {busy ? (
        <IconLoader2 className="animate-spin" />
      ) : failed ? (
        <IconX />
      ) : [
          "active",
          "running",
          "ready",
          "deployed",
          "succeeded",
          "success",
        ].includes(normalized) ? (
        <IconCircleCheckFilled />
      ) : (
        <span className="size-1.5 rounded-full bg-current opacity-60" />
      )}
      {readableState(state || "Unknown")}
    </Badge>
  )
}
