import { IconAlertTriangle } from "@tabler/icons-react"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"

export function PageLoading({ rows = 3 }: { rows?: number }) {
  return (
    <div aria-label="Loading" className="space-y-5">
      <Skeleton className="h-8 w-48" />
      <Card>
        <CardContent className="space-y-4 pt-4">
          {Array.from({ length: rows }, (_, index) => (
            <div
              key={index}
              className="flex items-center gap-3 border-b border-border/60 pb-4 last:border-0 last:pb-0"
            >
              <Skeleton className="size-9 rounded-lg" />
              <div className="flex-1">
                <Skeleton className="h-3 w-36" />
              </div>
              <Skeleton className="h-5 w-16 rounded-full" />
            </div>
          ))}
        </CardContent>
      </Card>
    </div>
  )
}

export function PageError({
  message,
  onRetry,
}: {
  message: string
  onRetry?: () => void
}) {
  return (
    <Card className="border-[#edc8c5] bg-[#fffdfd]">
      <CardContent className="flex flex-col items-start gap-3 py-6 sm:flex-row sm:items-center">
        <span className="grid size-9 shrink-0 place-items-center rounded-lg bg-[#fcf0ef] text-[#a4463f]">
          <IconAlertTriangle size={18} />
        </span>
        <div className="flex-1">
          <p className="text-sm font-semibold">Couldn’t load this view</p>
          <p className="mt-1 text-sm text-muted-foreground">{message}</p>
        </div>
        {onRetry ? (
          <Button variant="outline" size="sm" onClick={onRetry}>
            Try again
          </Button>
        ) : null}
      </CardContent>
    </Card>
  )
}
