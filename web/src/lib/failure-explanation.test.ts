import { describe, expect, it } from "vitest"
import {
  failureEvidenceFingerprint,
  isExplainableFailureState,
} from "./failure-explanation"
import type { Deployment } from "./types"

const attempt: Deployment = {
  ID: "dep_123",
  AppID: "app_1",
  AppName: "demo",
  ReleaseID: "rel_1",
  Port: 8080,
  State: "BUILD_FAILED",
  Error: "Build failed.",
  MemoryLimit: "512Mi",
  CPUQuota: 1,
  TasksLimit: 32,
  ReadinessKnown: true,
  HealthPath: "/health",
  PromotedAt: null,
  CreatedAt: "2026-10-14T00:00:00Z",
  StartedAt: null,
  StoppedAt: "2026-10-14T00:01:00Z",
}

describe("failure explanation identity", () => {
  it("allows only the supported failed deployment states", () => {
    for (const state of [
      "BUILD_FAILED",
      "START_FAILED",
      "UNHEALTHY",
      "CRASHED",
      "OOM_KILLED",
    ] as const) {
      expect(isExplainableFailureState(state)).toBe(true)
    }

    expect(isExplainableFailureState("RUNNING")).toBe(false)
    expect(isExplainableFailureState("STOPPED")).toBe(false)
  })

  it("changes cache identity when diagnostic evidence or production status changes", () => {
    const fingerprint = (
      overrides: {
        summary?: string
        excerpt?: string
        latestAttempt?: Deployment
        production?: Deployment | null
      } = {}
    ) =>
      failureEvidenceFingerprint({
        summary: overrides.summary ?? "The build failed.",
        excerpt: overrides.excerpt ?? "package not found",
        latestAttempt: overrides.latestAttempt ?? attempt,
        production: overrides.production ?? null,
      })

    const original = fingerprint()
    expect(fingerprint({ summary: "A different summary." })).not.toBe(original)
    expect(fingerprint({ excerpt: "a different log line" })).not.toBe(original)
    expect(
      fingerprint({ latestAttempt: { ...attempt, MemoryLimit: "1Gi" } })
    ).not.toBe(original)
    expect(
      fingerprint({ latestAttempt: { ...attempt, CPUQuota: 2 } })
    ).not.toBe(original)
    expect(
      fingerprint({ latestAttempt: { ...attempt, ReadinessKnown: false } })
    ).not.toBe(original)
    expect(
      fingerprint({
        production: { ...attempt, ID: "dep_live", State: "RUNNING" },
      })
    ).not.toBe(original)
  })
})
