export function formatDate(value?: string | number | Date | null) {
  if (!value) return "—"
  const date = value instanceof Date ? value : new Date(value)
  if (Number.isNaN(date.getTime())) return "—"
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date)
}

export function formatRelative(value?: string | number | Date | null) {
  if (!value) return "—"
  const date = value instanceof Date ? value : new Date(value)
  if (Number.isNaN(date.getTime())) return "—"
  const seconds = Math.round((date.getTime() - Date.now()) / 1000)
  const units: [Intl.RelativeTimeFormatUnit, number][] = [
    ["year", 31_536_000],
    ["month", 2_592_000],
    ["week", 604_800],
    ["day", 86_400],
    ["hour", 3_600],
    ["minute", 60],
    ["second", 1],
  ]
  const [unit, unitSeconds] =
    units.find(
      ([, candidateSeconds]) => Math.abs(seconds) >= candidateSeconds
    ) ?? units.at(-1)!
  return new Intl.RelativeTimeFormat(undefined, { numeric: "auto" }).format(
    Math.round(seconds / unitSeconds),
    unit
  )
}

export function shortHash(value?: string | null) {
  if (!value) return "—"
  const prefixedID = /^(dep_|rel_)([a-f\d-]+)/i.exec(value)
  if (prefixedID) {
    return `${prefixedID[1]}${prefixedID[2].replaceAll("-", "").slice(0, 8)}`
  }
  return value.slice(0, 7)
}

export function displayName(value?: string | null) {
  return value?.trim() || "Untitled project"
}
