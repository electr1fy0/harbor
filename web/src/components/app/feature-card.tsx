import type { ReactNode } from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { featureCall, type FeatureArea } from "@/lib/features"

export function FeatureCard({
  title,
  description,
  children,
}: {
  title: string
  description?: string
  children: ReactNode
}) {
  return (
    <Card className="border-border/80 bg-white">
      <CardHeader>
        <CardTitle className="text-sm">{title}</CardTitle>
        {description ? (
          <p className="text-xs text-muted-foreground">{description}</p>
        ) : null}
      </CardHeader>
      <CardContent className="space-y-4">{children}</CardContent>
    </Card>
  )
}
export function useFeatureAction<T = Record<string, unknown>>(
  area: FeatureArea,
  onSuccess?: (data: T, input: Record<string, unknown>) => void
) {
  const client = useQueryClient()
  return useMutation({
    mutationFn: (input: Record<string, unknown>) => featureCall<T>(area, input),
    onSuccess: (data, input) => {
      onSuccess?.(data, input)
      toast.success("Changes applied")
    },
    onError: (error) => toast.error(error.message),
    onSettled: () => client.invalidateQueries({ queryKey: ["harbor"] }),
  })
}
export const fieldClass = "grid gap-1.5 text-xs font-medium"
