import { callDashboardFunction } from "./api.client"
import type { CapacitySnapshot } from "./types"

export type FeatureArea =
  "operations" | "automation" | "dataServices" | "observe"
export function featureCall<T>(
  area: FeatureArea,
  input: Record<string, unknown>
): Promise<T> {
  return callDashboardFunction<T>(area, input)
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
  primaryDomain: string
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

/** Parse a bounded dotenv document without executing or interpolating values. */
export function parseDotEnv(text: string): Record<string, string> {
  if (new TextEncoder().encode(text).length > 60_000)
    throw new Error("Environment import is limited to 60 KB.")
  const values: Record<string, string> = Object.create(null)
  const lines = text.split(/\r?\n/)
  for (let index = 0; index < lines.length; index++) {
    const line = lines[index].trim()
    if (!line || line.startsWith("#")) continue
    const match = /^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$/.exec(
      line
    )
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
  return values
}
