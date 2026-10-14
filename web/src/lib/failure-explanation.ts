import type { Deployment, DeploymentState } from "./types"

const failedStates = new Set<DeploymentState>([
  "BUILD_FAILED",
  "START_FAILED",
  "UNHEALTHY",
  "CRASHED",
  "OOM_KILLED",
])

export function isExplainableFailureState(state: DeploymentState): boolean {
  return failedStates.has(state)
}

/**
 * Create a compact cache identity from the evidence visible for this attempt.
 * Hashing keeps raw diagnostic text out of the React Query key while ensuring
 * a refreshed excerpt or deployment status cannot reuse an older explanation.
 */
export function failureEvidenceFingerprint({
  summary,
  excerpt,
  latestAttempt,
  production,
}: {
  summary: string
  excerpt: string
  latestAttempt: Deployment
  production: Deployment | null
}): string {
  const evidence = JSON.stringify({
    summary,
    excerpt,
    latestAttempt: {
      id: latestAttempt.ID,
      state: latestAttempt.State,
      error: latestAttempt.Error,
      releaseID: latestAttempt.ReleaseID,
      createdAt: latestAttempt.CreatedAt,
      startedAt: latestAttempt.StartedAt,
      stoppedAt: latestAttempt.StoppedAt,
      port: latestAttempt.Port,
      memoryLimit: latestAttempt.MemoryLimit,
      cpuQuota: latestAttempt.CPUQuota,
      tasksLimit: latestAttempt.TasksLimit,
      readinessKnown: latestAttempt.ReadinessKnown,
      healthPath: latestAttempt.HealthPath,
    },
    production: production
      ? {
          id: production.ID,
          state: production.State,
          error: production.Error,
        }
      : null,
  })

  let hash = 0xcbf29ce484222325n
  for (let index = 0; index < evidence.length; index++) {
    hash ^= BigInt(evidence.charCodeAt(index))
    hash = BigInt.asUintN(64, hash * 0x100000001b3n)
  }
  return hash.toString(16).padStart(16, "0")
}
