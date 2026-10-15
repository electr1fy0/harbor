import { describe, expect, it } from "vitest"
import { HarborNotFoundError } from "./api.client"
import {
  beginLogCursorRequest,
  completeLogCursorRequest,
  createLatestAsyncDraftGuard,
  findTextMatches,
  mergeLogOutput,
  jsonPayloadByteLength,
  observeHistoryRequest,
  parseDotEnv,
  savedSourceErrorMessage,
  sameInspectionSnapshot,
  shouldClearBuildEnvironmentAfterAction,
  shouldClearRuntimeSecretDraft,
  type LogCursorEntry,
} from "./features"

describe("dotenv import", () => {
  it("parses comments, export syntax, empty and quoted values without interpolation", () => {
    expect(
      parseDotEnv(
        'A=\nexport B="one\\ntwo" # comment\nC=literal#hash\nD= $HOME # comment'
      )
    ).toEqual({ A: "", B: "one\ntwo", C: "literal#hash", D: "$HOME" })
  })
  it("rejects duplicate keys, invalid lines and malformed quotes", () => {
    expect(() => parseDotEnv("A=1\nA=2")).toThrow("duplicate")
    expect(() => parseDotEnv("bad-key=value")).toThrow("KEY=value")
    expect(() => parseDotEnv('A="oops')).toThrow("unmatched quote")
  })
  it("accepts prototype-like keys as plain data", () => {
    expect(parseDotEnv("__proto__=safe")["__proto__"]).toBe("safe")
  })
  it("matches the daemon's 128-character environment key limit", () => {
    const maximumKey = "A".repeat(128)
    expect(parseDotEnv(`${maximumKey}=ok`)[maximumKey]).toBe("ok")
    expect(() => parseDotEnv(`${maximumKey}A=too-long`)).toThrow(
      "expected KEY=value"
    )
  })
})

describe("observability history requests", () => {
  it("includes the selected history window in the daemon request", () => {
    expect(observeHistoryRequest("api", "hour")).toEqual({
      action: "get",
      name: "api",
      limit: 120,
      window: "hour",
    })
    expect(observeHistoryRequest("api", "day").window).toBe("day")
  })
})

describe("saved source load status", () => {
  it("treats a missing source as an empty state and storage failures as errors", () => {
    expect(savedSourceErrorMessage(new HarborNotFoundError())).toBeNull()
    expect(
      savedSourceErrorMessage(new Error("Saved settings are corrupt."))
    ).toBe("Saved settings are corrupt.")
    expect(savedSourceErrorMessage({})).toBe(
      "Saved repository settings could not be loaded."
    )
  })
})

describe("build environment draft handling", () => {
  it("clears the draft only after a successful environment replacement", () => {
    expect(
      shouldClearBuildEnvironmentAfterAction({
        action: "updateSource",
        source: { buildEnvironment: {} },
      })
    ).toBe(true)
    expect(
      shouldClearBuildEnvironmentAfterAction({
        action: "updateSource",
        source: { ref: "main" },
      })
    ).toBe(false)
    expect(
      shouldClearBuildEnvironmentAfterAction({
        action: "redeploy",
        name: "api",
      })
    ).toBe(false)
  })

  it("preserves runtime secret drafts after unrelated settings updates", () => {
    expect(
      shouldClearRuntimeSecretDraft({ environment: { API_TOKEN: "x" } })
    ).toBe(true)
    expect(shouldClearRuntimeSecretDraft({})).toBe(false)
  })
})

describe("async inspection and request bounds", () => {
  it("rejects inspection results for a stale source draft", () => {
    const request = {
      name: "api",
      repoURL: "https://github.com/example/api",
      ref: "main",
      rootDir: "",
      build: "go build ./...",
      start: "./app",
      health: "/health",
    }
    expect(sameInspectionSnapshot(request, { ...request })).toBe(true)
    expect(
      sameInspectionSnapshot(request, { ...request, ref: "release" })
    ).toBe(false)
    expect(
      sameInspectionSnapshot(request, { ...request, build: "go test ./..." })
    ).toBe(false)
  })

  it("bounds JSON-encoded dotenv values, not only their source text", () => {
    const text = `KEY="${"\\u".repeat(24_000)}"`
    expect(new TextEncoder().encode(text).length).toBeLessThan(60_000)
    expect(
      jsonPayloadByteLength({ environment: { KEY: "\\u".repeat(24_000) } })
    ).toBeGreaterThan(64 * 1024)
    expect(() => parseDotEnv(text)).toThrow("after JSON encoding")
  })

  it("ignores file reads and failures after a newer draft replaces them", () => {
    const guard = createLatestAsyncDraftGuard()
    const firstFileRead = guard.begin()
    const secondFileRead = guard.begin()
    expect(guard.isCurrent(firstFileRead)).toBe(false)
    expect(guard.isCurrent(secondFileRead)).toBe(true)

    const thirdFileRead = guard.begin()
    guard.invalidate() // the user typed a newer document while it was loading
    expect(guard.isCurrent(thirdFileRead)).toBe(false)
  })
})

describe("advanced log cursors", () => {
  it("isolates deployment/source identities and ignores late responses", () => {
    const cursors = new Map<string, LogCursorEntry>()
    const firstDeploymentRequest = beginLogCursorRequest(
      cursors,
      "api:1:all",
      1
    )
    const secondDeploymentRequest = beginLogCursorRequest(
      cursors,
      "api:2:all",
      2
    )
    expect(firstDeploymentRequest).toBe("")
    expect(secondDeploymentRequest).toBe("")

    completeLogCursorRequest(cursors, "api:1:all", 1, "cursor-one")
    completeLogCursorRequest(cursors, "api:2:all", 2, "cursor-two")
    const newerFirstRequest = beginLogCursorRequest(cursors, "api:1:all", 3)
    expect(newerFirstRequest).toBe("cursor-one")

    completeLogCursorRequest(cursors, "api:1:all", 1, "stale-cursor")
    completeLogCursorRequest(cursors, "api:1:all", 3, "cursor-one-new")
    expect(cursors.get("api:1:all")?.value).toBe("cursor-one-new")
    expect(cursors.get("api:2:all")?.value).toBe("cursor-two")
  })

  it("restarts a cursor when returning to a view whose output was discarded", () => {
    const cursors = new Map<string, LogCursorEntry>()
    let output = mergeLogOutput({ identity: "", text: "" }, "api:1:all", {
      text: "A: first line\n",
      reset: true,
    })
    expect(beginLogCursorRequest(cursors, "api:1:all", 1)).toBe("")
    completeLogCursorRequest(cursors, "api:1:all", 1, "cursor-a")
    expect(beginLogCursorRequest(cursors, "api:2:all", 2)).toBe("")
    output = mergeLogOutput(output, "api:2:all", {
      text: "B output\n",
      reset: true,
    })
    completeLogCursorRequest(cursors, "api:2:all", 2, "cursor-b")

    // The component only retains one view's output. Returning to A must fetch
    // the whole history because its previous output was discarded on B.
    expect(beginLogCursorRequest(cursors, "api:1:all", 3, true)).toBe("")
    // The late response for the original A request cannot replace the fresh
    // cursor for the return request.
    completeLogCursorRequest(cursors, "api:1:all", 1, "late-cursor-a")
    expect(cursors.get("api:1:all")?.requestID).toBe(3)
    output = mergeLogOutput(output, "api:1:all", {
      text: "A: first line\nA: second line\n",
      reset: true,
    })
    completeLogCursorRequest(cursors, "api:1:all", 3, "cursor-a-full")
    expect(cursors.get("api:1:all")?.value).toBe("cursor-a-full")
    expect(output.text).toBe("A: first line\nA: second line\n")
  })
})

describe("log search", () => {
  it("returns bounded, non-overlapping, case-insensitive source spans", () => {
    expect(findTextMatches("Ababa ABABA", "aba")).toEqual([
      { start: 0, end: 3 },
      { start: 6, end: 9 },
    ])
    expect(findTextMatches("aaaa", "aa", 1)).toEqual([{ start: 0, end: 2 }])
    expect(findTextMatches("anything", "")).toEqual([])
  })

  it("keeps source offsets correct when lowercase conversion expands Unicode", () => {
    const text = "İx before İy"
    const matches = findTextMatches(text, "İ")
    let cursor = 0
    const reconstructed = matches
      .map(({ start, end }) => {
        expect(text.slice(start, end)).toBe("İ")
        const segment = text.slice(cursor, end)
        cursor = end
        return segment
      })
      .join("")
    expect(matches.map((match) => [match.start, match.end])).toEqual([
      [0, 1],
      [10, 11],
    ])
    expect(reconstructed + text.slice(cursor)).toBe(text)
  })
})
