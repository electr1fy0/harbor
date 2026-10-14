import { useQuery } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import { explainFailure } from "@/lib/harbor.functions"
import {
  failureEvidenceFingerprint,
  isExplainableFailureState,
} from "@/lib/failure-explanation"
import type { Deployment } from "@/lib/types"

export function FailureExplanation({
  name,
  deployment,
  summary,
  excerpt,
  production,
}: {
  name: string
  deployment: Deployment
  summary: string
  excerpt: string
  production: Deployment | null
}) {
  if (!isExplainableFailureState(deployment.State)) return null

  return (
    <FailureExplanationForAttempt
      key={`${name}:${deployment.ID}`}
      name={name}
      deployment={deployment}
      summary={summary}
      excerpt={excerpt}
      production={production}
    />
  )
}

function FailureExplanationForAttempt({
  name,
  deployment,
  summary,
  excerpt,
  production,
}: {
  name: string
  deployment: Deployment
  summary: string
  excerpt: string
  production: Deployment | null
}) {
  const fingerprint = failureEvidenceFingerprint({
    summary,
    excerpt,
    latestAttempt: deployment,
    production,
  })
  const explanationQuery = useQuery({
    queryKey: [
      "harbor",
      "failure-explanation",
      name,
      deployment.ID,
      fingerprint,
    ],
    queryFn: async () => {
      const explanation = await explainFailure({
        data: { name, deploymentID: deployment.ID },
      })
      if (explanation.deploymentID !== deployment.ID) {
        throw new Error("Harbor returned an explanation for another attempt.")
      }
      return explanation
    },
    enabled: false,
    retry: false,
    staleTime: Infinity,
    gcTime: 30 * 60 * 1000,
  })

  const requestExplanation = () => {
    if (!explanationQuery.isFetching && !explanationQuery.data) {
      void explanationQuery.refetch()
    }
  }

  return (
    <section className="rounded-lg border border-border/80 bg-[#fbfcfb] p-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="text-xs text-muted-foreground">
          Explain failure sends redacted logs and deployment details to OpenAI.
        </p>
        {!explanationQuery.data ? (
          <Button
            variant="outline"
            size="sm"
            disabled={explanationQuery.isFetching}
            onClick={requestExplanation}
          >
            {explanationQuery.isFetching
              ? "Explaining…"
              : explanationQuery.isError
                ? "Try again"
                : "Explain failure"}
          </Button>
        ) : null}
      </div>

      {explanationQuery.isError ? (
        <p role="alert" className="mt-2 text-xs text-[#a4463f]">
          {explanationQuery.error.message ||
            "Harbor could not explain this failure. Try again."}
        </p>
      ) : null}

      {explanationQuery.data ? (
        <div
          aria-live="polite"
          className="mt-3 space-y-3 border-t border-border/70 pt-3"
        >
          <div>
            <p className="text-[10px] font-semibold tracking-[0.1em] text-muted-foreground uppercase">
              Explanation
            </p>
            <p className="mt-1 text-sm leading-6 text-foreground">
              {explanationQuery.data.explanation}
            </p>
          </div>
          {explanationQuery.data.evidence.length > 0 ? (
            <div>
              <p className="text-[10px] font-semibold tracking-[0.1em] text-muted-foreground uppercase">
                Evidence
              </p>
              <ul className="mt-1 max-w-full list-inside list-disc space-y-1 text-xs leading-5 text-muted-foreground">
                {explanationQuery.data.evidence.map((item, index) => (
                  <li
                    key={`${index}-${item}`}
                    className="max-w-full [overflow-wrap:anywhere] break-words whitespace-pre-wrap"
                  >
                    {item}
                  </li>
                ))}
              </ul>
            </div>
          ) : null}
          <div>
            <p className="text-[10px] font-semibold tracking-[0.1em] text-muted-foreground uppercase">
              Next step
            </p>
            <p className="mt-1 text-sm leading-6 text-foreground">
              {explanationQuery.data.nextStep}
            </p>
          </div>
        </div>
      ) : null}
    </section>
  )
}
