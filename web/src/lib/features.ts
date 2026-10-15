import { callDashboardFunction, HarborNotFoundError } from "./api.client"
import type { CapacitySnapshot } from "./types"

export type FeatureArea =
  "operations" | "automation" | "dataServices" | "observe"
export function featureCall<T>(
  area: FeatureArea,
  input: Record<string, unknown>
): Promise<T> {
  return callDashboardFunction<T>(area, input)
}

/** A missing saved source is an expected empty state; other failures need retry. */
export function savedSourceErrorMessage(error: unknown): string | null {
  if (error == null || error instanceof HarborNotFoundError) return null
  if (error instanceof Error && error.message.trim()) return error.message
  return "Saved repository settings could not be loaded."
}

export type ObserveHistoryWindow = "hour" | "day"

export function observeHistoryRequest(
  name: string,
  window: ObserveHistoryWindow
) {
  return { action: "get", name, limit: 120, window }
}

export function shouldClearBuildEnvironmentAfterAction(
  input: Record<string, unknown>
): boolean {
  if (input.action !== "updateSource") return false
  const source = input.source
  return (
    typeof source === "object" &&
    source !== null &&
    "buildEnvironment" in source
  )
}

export function shouldClearRuntimeSecretDraft(input: {
  environment?: unknown
}): boolean {
  return input.environment !== undefined
}

export function sameInspectionSnapshot(
  expected: object,
  current: object
): boolean {
  const expectedValues = expected as Record<string, unknown>
  const currentValues = current as Record<string, unknown>
  const keys = Object.keys(expectedValues)
  return (
    keys.length === Object.keys(currentValues).length &&
    keys.every(
      (key) =>
        Object.hasOwn(currentValues, key) &&
        expectedValues[key] === currentValues[key]
    )
  )
}

export function jsonPayloadByteLength(value: unknown): number {
  return new TextEncoder().encode(JSON.stringify(value) ?? "").length
}

/** Return non-overlapping case-insensitive source spans for a bounded log. */
export function findTextMatches(
  text: string,
  search: string,
  limit = 2000
): Array<{ start: number; end: number }> {
  if (!search || limit <= 0) return []
  const escapedSearch = search.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")
  const expression = new RegExp(escapedSearch, "giu")
  const matches: Array<{ start: number; end: number }> = []
  let match: RegExpExecArray | null
  while (matches.length < limit && (match = expression.exec(text)) !== null) {
    matches.push({ start: match.index, end: expression.lastIndex })
  }
  return matches
}

export function downloadText(
  filename: string,
  text: string,
  type = "text/plain"
) {
  const url = URL.createObjectURL(new Blob([text], { type }))
  const anchor = document.createElement("a")
  anchor.href = url
  anchor.download = filename
  anchor.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
export function downloadBase64(
  filename: string,
  content: string,
  type = "application/octet-stream"
) {
  const bytes = Uint8Array.from(atob(content), (char) => char.charCodeAt(0))
  const url = URL.createObjectURL(new Blob([bytes], { type }))
  const anchor = document.createElement("a")
  anchor.href = url
  anchor.download = filename
  anchor.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
export interface ObserveSettings {
  name: string
  maintenance: boolean
  maintenanceMessage: string
  memoryPercent: number
  diskPercent: number
  cooldownSeconds: number
  webhookConfigured: boolean
  events: string[]
}
export interface ObserveView {
  settings: ObserveSettings
  history: Array<{ at: string; snapshot: CapacitySnapshot }>
  traffic: Array<{
    host: string
    requests: number
    bytes: number
    statusCodes: Record<string, number>
    totalLatencyMs: number
    maxLatencyMs: number
  }>
  notifications: Array<{ at: string; event: string; success: boolean }>
}
export interface LogDelta {
  text: string
  cursor: string
  reset: boolean
  source: string
}

export interface LogCursorEntry {
  value: string
  requestID: number
}

export interface LogOutput {
  identity: string
  text: string
}

/** Apply a response to the retained output for its identity. */
export function mergeLogOutput(
  previous: LogOutput,
  identity: string,
  result: Pick<LogDelta, "text" | "reset">,
  limit = 1_048_576
): LogOutput {
  const base =
    result.reset || previous.identity !== identity ? "" : previous.text
  return { identity, text: (base + result.text).slice(-limit) }
}

/** Begin a log request while preserving a cursor per deployment/source view. */
export function beginLogCursorRequest(
  cursors: Map<string, LogCursorEntry>,
  identity: string,
  requestID: number,
  reset = false
): string {
  if (reset) cursors.delete(identity)
  const value = cursors.get(identity)?.value ?? ""
  cursors.set(identity, { value, requestID })
  return value
}

/** Ignore late responses when a newer request for the same log view exists. */
export function completeLogCursorRequest(
  cursors: Map<string, LogCursorEntry>,
  identity: string,
  requestID: number,
  value: string
) {
  if (cursors.get(identity)?.requestID === requestID) {
    cursors.set(identity, { value, requestID })
  }
}

/** Guard async readers from applying results after the user changes the draft. */
export function createLatestAsyncDraftGuard() {
  let generation = 0
  return {
    begin: () => ++generation,
    invalidate: () => {
      generation++
    },
    isCurrent: (requestGeneration: number) => requestGeneration === generation,
  }
}

/** Parse a bounded dotenv document without executing or interpolating values. */
export function parseDotEnv(text: string): Record<string, string> {
  if (new TextEncoder().encode(text).length > 60_000)
    throw new Error("Environment import is limited to 60 KB.")
  const values: Record<string, string> = Object.create(null)
  const lines = text.split(/\r?\n/)
  for (let index = 0; index < lines.length; index++) {
    const line = lines[index].trim()
    if (!line || line.startsWith("#")) continue
    const match =
      /^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]{0,127})\s*=\s*(.*)$/.exec(line)
    if (!match) throw new Error(`Line ${index + 1}: expected KEY=value.`)
    let value = match[2]
    if (value.startsWith('"') || value.startsWith("'")) {
      const quote = value[0]
      const quoted =
        quote === '"'
          ? /^"((?:[^"\\]|\\.)*)"\s*(?:#.*)?$/
          : /^'([^']*)'\s*(?:#.*)?$/
      const content = quoted.exec(value)
      if (!content)
        throw new Error(
          `Line ${index + 1}: unmatched quote or unsupported multiline value.`
        )
      value =
        quote === '"'
          ? content[1].replace(
              /\\(n|r|t|"|\\)/g,
              (_, char: string) =>
                ({ n: "\n", r: "\r", t: "\t", '"': '"', "\\": "\\" })[char] ??
                char
            )
          : content[1]
    } else value = value.replace(/\s+#.*$/, "").trimEnd()
    if (value.includes("\0"))
      throw new Error(`Line ${index + 1}: NUL bytes are not allowed.`)
    if (Object.hasOwn(values, match[1]))
      throw new Error(`Line ${index + 1}: duplicate key ${match[1]}.`)
    values[match[1]] = value
    if (Object.keys(values).length > 100)
      throw new Error("Import at most 100 variables at a time.")
  }
  if (
    jsonPayloadByteLength({ name: "x".repeat(63), environment: values }) >
    64 * 1024
  ) {
    throw new Error(
      "Environment import exceeds the daemon request limit after JSON encoding."
    )
  }
  return values
}
