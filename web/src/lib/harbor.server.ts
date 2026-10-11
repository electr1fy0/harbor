import { Buffer } from "node:buffer"
import { request as httpRequest } from "node:http"
import { env } from "node:process"
import type {
  AppDetails,
  AppSettings,
  AppView,
  CapacitySnapshot,
  Deployment,
  DeploymentResult,
  DeploymentJob,
  GitHubBranch,
  GitHubConnection,
  GitHubRepo,
  Overview,
  ProjectInspection,
  ReleaseView,
  Service,
} from "./types"
import type {
  ConfigureAppInput,
  InspectProjectInput,
  SubmitOnboardingJobInput,
} from "./validation"

const DEFAULT_SOCKET_PATH = "/run/paas/paasd.sock"
const MAX_DAEMON_RESPONSE_BYTES = 4 * 1024 * 1024
const MAX_GITHUB_RESPONSE_BYTES = 2 * 1024 * 1024
const DEFAULT_TIMEOUT_MS = 15_000
const DEPLOY_TIMEOUT_MS = 10 * 60 * 1_000
const ONBOARDING_PREFLIGHT_TIMEOUT_MS = 150_000
const GITHUB_API_ORIGIN = "https://api.github.com"
const SAFE_DAEMON_UNAVAILABLE = "Harbor daemon is unavailable."

type JsonObject = Record<string, unknown>

interface UnixResponse {
  statusCode: number
  body: string
}

interface DaemonResponse extends JsonObject {
  URL?: unknown
  Deployment?: unknown
  Apps?: unknown
  Services?: unknown
  Releases?: unknown
  Logs?: unknown
  Error?: unknown
}

export class HarborNotFoundError extends Error {
  constructor() {
    super("Harbor resource was not found.")
    this.name = "HarborNotFoundError"
  }
}

class HarborUnavailableError extends Error {
  constructor() {
    super(SAFE_DAEMON_UNAVAILABLE)
    this.name = "HarborUnavailableError"
  }
}

class HarborTimeoutError extends Error {
  constructor() {
    super("Harbor daemon did not respond in time.")
    this.name = "HarborTimeoutError"
  }
}

class HarborOperationError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "HarborOperationError"
  }
}

class InternalTransportError extends Error {}
class InternalTimeoutError extends Error {}
class InternalResponseLimitError extends Error {}

function isObject(value: unknown): value is JsonObject {
  return typeof value === "object" && value !== null && !Array.isArray(value)
}

function safeString(value: unknown, maxLength = 512): string {
  return typeof value === "string" ? value.slice(0, maxLength) : ""
}

function safeNumber(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0
}

function safeOptionalNumber(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) && value >= 0
    ? value
    : null
}

function safeOptionalBytes(value: unknown): number | null {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0
    ? value
    : null
}

function safeOptionalSignedBytes(value: unknown): number | null {
  return typeof value === "number" && Number.isSafeInteger(value) ? value : null
}

function safeBoolean(value: unknown): boolean {
  return value === true
}

function safeTime(value: unknown): string {
  if (typeof value !== "string" || value.length > 128) return ""
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? "" : parsed.toISOString()
}

function safeNullableTime(value: unknown): string | null {
  return value === null || value === undefined ? null : safeTime(value)
}

function safeSlug(value: unknown): string {
  const candidate = safeString(value, 63)
  return /^[a-z][a-z0-9-]{0,62}$/.test(candidate) ? candidate : ""
}

function safeDeploymentError(state: string): string {
  switch (state) {
    case "BUILD_FAILED":
      return "Build failed. Review the app logs for details."
    case "START_FAILED":
      return "App failed to start. Review the app logs for details."
    case "UNHEALTHY":
      return "App did not pass its health check. Review the app logs for details."
    case "CRASHED":
      return "App stopped unexpectedly. Review the app logs for details."
    case "OOM_KILLED":
      return "App exceeded its memory limit. Review the app logs for details."
    default:
      return ""
  }
}

const deploymentStates = new Set<Deployment["State"]>([
  "QUEUED",
  "BUILDING",
  "BUILT",
  "STARTING",
  "HEALTH_CHECKING",
  "RUNNING",
  "BUILD_FAILED",
  "START_FAILED",
  "UNHEALTHY",
  "CRASHED",
  "OOM_KILLED",
  "STOPPED",
])

function mapDeployment(value: unknown): Deployment | null {
  if (value === null || value === undefined) return null
  if (!isObject(value)) {
    throw new HarborOperationError("Harbor returned invalid deployment data.")
  }

  const stateValue = safeString(value.State, 32).toUpperCase()
  const state = deploymentStates.has(stateValue as Deployment["State"])
    ? (stateValue as Deployment["State"])
    : "UNKNOWN"

  return {
    ID: safeString(value.ID, 128),
    AppID: safeString(value.AppID, 128),
    AppName: safeSlug(value.AppName),
    ReleaseID: safeString(value.ReleaseID, 128),
    Port: safeNumber(value.Port),
    State: state,
    Error: safeDeploymentError(state),
    MemoryLimit: safeString(value.MemoryLimit, 64),
    CPUQuota: safeNumber(value.CPUQuota),
    TasksLimit: safeNumber(value.TasksLimit),
    ReadinessKnown: safeBoolean(value.ReadinessKnown),
    HealthPath: safeString(value.HealthPath, 256),
    PromotedAt: safeNullableTime(value.PromotedAt),
    CreatedAt: safeTime(value.CreatedAt),
    StartedAt: safeNullableTime(value.StartedAt),
    StoppedAt: safeNullableTime(value.StoppedAt),
  }
}

function mapAppView(value: unknown): AppView {
  if (!isObject(value) || !isObject(value.Application)) {
    throw new HarborOperationError("Harbor returned invalid application data.")
  }

  const application = value.Application
  return {
    Application: {
      ID: safeString(application.ID, 128),
      Name: safeSlug(application.Name),
      CreatedAt: safeTime(application.CreatedAt),
    },
    Deployment: mapDeployment(value.Deployment),
  }
}

function mapService(value: unknown): Service {
  if (!isObject(value)) {
    throw new HarborOperationError("Harbor returned invalid service data.")
  }
  return {
    id: safeString(value.id, 128),
    name: safeString(value.name, 128),
    type: safeString(value.type, 64),
    version: safeString(value.version, 64),
    port: safeNumber(value.port),
    state: safeString(value.state, 32),
    createdAt: safeTime(value.created_at),
  }
}

function mapRelease(value: unknown): ReleaseView {
  if (!isObject(value)) {
    throw new HarborOperationError("Harbor returned invalid release data.")
  }
  return {
    id: safeString(value.id, 128),
    appId: safeString(value.app_id, 128),
    appName: safeSlug(value.app_name),
    sourceHash: safeString(value.source_hash, 128),
    runtime: safeString(value.runtime, 64),
    createdAt: safeTime(value.created_at),
    isActive: safeBoolean(value.is_active),
  }
}

function safeDomain(value: unknown): string {
  const candidate = safeString(value, 253).toLowerCase()
  return /^(?=.{1,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(
    candidate
  )
    ? candidate
    : ""
}

function mapSettings(value: unknown): AppSettings {
  if (!isObject(value)) {
    throw new HarborOperationError("Harbor returned invalid app settings.")
  }
  const settings = isObject(value.settings)
    ? value.settings
    : isObject(value.Settings)
      ? value.Settings
      : value
  const domains = arrayValue(settings.domains ?? settings.Domains)
    .map(safeDomain)
    .filter(Boolean)
  const environmentKeys = arrayValue(
    settings.environmentKeys ?? settings.EnvironmentKeys
  )
    .map((key) => safeString(key, 128))
    .filter((key) => /^[A-Za-z_][A-Za-z0-9_]{0,127}$/.test(key))
  return {
    domains: [...new Set(domains)],
    environmentKeys: [...new Set(environmentKeys)],
    memoryLimit: safeString(settings.memoryLimit ?? settings.MemoryLimit, 64),
    cpuQuota: safeNumber(settings.cpuQuota ?? settings.CPUQuota),
    tasksLimit: safeNumber(settings.tasksLimit ?? settings.TasksLimit),
  }
}

const emptySettings: AppSettings = {
  domains: [],
  environmentKeys: [],
  memoryLimit: "",
  cpuQuota: 0,
  tasksLimit: 0,
}

function mapCapacity(value: unknown): CapacitySnapshot {
  if (!isObject(value)) {
    throw new HarborOperationError("Harbor returned invalid capacity data.")
  }
  const platform = isObject(value.platform) ? value.platform : {}
  const host = isObject(value.host) ? value.host : {}
  const disk = isObject(value.disk) ? value.disk : {}
  const builds = isObject(value.builds) ? value.builds : {}
  const headroom = isObject(value.headroom) ? value.headroom : {}
  const apps = arrayValue(value.apps).map((item) => {
    if (!isObject(item)) {
      throw new HarborOperationError("Harbor returned invalid capacity data.")
    }
    return {
      deploymentId: safeString(item.deploymentId, 128),
      name: safeSlug(item.name),
      state: safeString(item.state, 32),
      memoryLimitBytes: safeOptionalBytes(item.memoryLimitBytes),
      memoryCurrentBytes: safeOptionalBytes(item.memoryCurrentBytes),
      memoryPeakBytes: safeOptionalBytes(item.memoryPeakBytes),
      cpuQuotaCores: safeOptionalNumber(item.cpuQuotaCores),
      cpuUsageCores: safeOptionalNumber(item.cpuUsageCores),
      tasksLimit: safeOptionalBytes(item.tasksLimit),
      tasksCurrent: safeOptionalBytes(item.tasksCurrent),
      metricsSource: safeString(item.metricsSource, 32),
    }
  })
  const caveats = Array.isArray(value.caveats)
    ? value.caveats.flatMap((item) =>
        typeof item === "string" ? [item.slice(0, 256)] : []
      )
    : []

  return {
    sampledAt: safeTime(value.sampledAt),
    platform: {
      supported: safeBoolean(platform.supported),
      name: safeString(platform.name, 32),
      reason: safeString(platform.reason, 256),
    },
    host: {
      memoryTotalBytes: safeOptionalBytes(host.memoryTotalBytes),
      memoryAvailableBytes: safeOptionalBytes(host.memoryAvailableBytes),
      memoryUsedBytes: safeOptionalBytes(host.memoryUsedBytes),
      cpuUsagePercent: safeOptionalNumber(host.cpuUsagePercent),
      logicalCPUs: safeOptionalBytes(host.logicalCPUs),
    },
    disk: {
      filesystemTotalBytes: safeOptionalBytes(disk.filesystemTotalBytes),
      filesystemAvailableBytes: safeOptionalBytes(
        disk.filesystemAvailableBytes
      ),
      filesystemUsedBytes: safeOptionalBytes(disk.filesystemUsedBytes),
      harborDataBytes: safeOptionalBytes(disk.harborDataBytes),
      harborDataComplete: safeBoolean(disk.harborDataComplete),
      harborDataChangeBytes: safeOptionalSignedBytes(
        disk.harborDataChangeBytes
      ),
      releasesChangeBytes: safeOptionalSignedBytes(disk.releasesChangeBytes),
      buildCacheChangeBytes: safeOptionalSignedBytes(
        disk.buildCacheChangeBytes
      ),
      logsChangeBytes: safeOptionalSignedBytes(disk.logsChangeBytes),
      changeIntervalSeconds: safeOptionalNumber(disk.changeIntervalSeconds),
      releasesBytes: safeOptionalBytes(disk.releasesBytes),
      buildCacheBytes: safeOptionalBytes(disk.buildCacheBytes),
      logsBytes: safeOptionalBytes(disk.logsBytes),
    },
    apps,
    builds: {
      systemdManagedActiveCount: safeOptionalBytes(
        builds.systemdManagedActiveCount
      ),
      memoryLimitBytes: safeOptionalBytes(builds.memoryLimitBytes),
      memoryCurrentBytes: safeOptionalBytes(builds.memoryCurrentBytes),
      memoryPeakBytes: safeOptionalBytes(builds.memoryPeakBytes),
      cpuQuotaCores: safeOptionalNumber(builds.cpuQuotaCores),
      cpuUsageCores: safeOptionalNumber(builds.cpuUsageCores),
    },
    headroom: {
      memoryReservePercent:
        safeOptionalNumber(headroom.memoryReservePercent) ?? 0,
      memoryReserveBytes: safeOptionalBytes(headroom.memoryReserveBytes),
      estimatedMemoryAfterReserveBytes: safeOptionalBytes(
        headroom.estimatedMemoryAfterReserveBytes
      ),
    },
    caveats,
  }
}

function sanitizeLogs(value: unknown): string {
  if (typeof value !== "string") return ""
  return value
    .slice(-250_000)
    .replace(/\bgh[pousr]_[A-Za-z0-9_]{12,}\b/g, "[redacted token]")
    .replace(/\bgithub_pat_[A-Za-z0-9_]{12,}\b/g, "[redacted token]")
    .replace(/(\bBearer\s+)[A-Za-z0-9._~+/-]+=*/gi, "$1[redacted]")
    .replace(/([?&](?:access_token|token|auth|key)=)[^&\s]+/gi, "$1[redacted]")
    .replace(
      /\b(authorization|password|passwd|secret|token|api[_-]?key)\s*[:=]\s*([^\s,;]+)/gi,
      "$1=[redacted]"
    )
    .replace(/(https?:\/\/)[^\s/@:]+:[^\s/@]+@/gi, "$1[redacted]@")
    .replace(
      /(?:\/Users\/|\/home\/|\/private\/tmp\/|\/tmp\/|\/var\/tmp\/|\/run\/paas\/|\/opt\/paas\/|\/var\/lib\/paas\/|\/srv\/paas\/)[^\s'"`),;]*/g,
      "[redacted path]"
    )
}

function getSocketPath(): string {
  return env.PAASD_SOCKET?.trim() || DEFAULT_SOCKET_PATH
}

function requestUnix(
  method: "GET" | "POST" | "DELETE",
  endpoint: string,
  body: string | undefined,
  timeoutMs: number
): Promise<UnixResponse> {
  const socketPath = getSocketPath()

  return new Promise((resolve, reject) => {
    let settled = false
    let bytes = 0
    const chunks: Buffer[] = []
    const finish = (callback: () => void) => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      callback()
    }
    const headers: Record<string, string> = {}
    if (body !== undefined) {
      headers["content-type"] = "application/json"
      headers["content-length"] = String(Buffer.byteLength(body))
    }

    const request = httpRequest(
      { socketPath, path: endpoint, method, headers },
      (response) => {
        response.on("data", (chunk: Buffer | string) => {
          if (settled) return
          const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk)
          bytes += buffer.byteLength
          if (bytes > MAX_DAEMON_RESPONSE_BYTES) {
            finish(() => reject(new InternalResponseLimitError()))
            request.destroy()
            response.destroy()
            return
          }
          chunks.push(buffer)
        })
        response.on("end", () => {
          finish(() =>
            resolve({
              statusCode: response.statusCode ?? 500,
              body: Buffer.concat(chunks).toString("utf8"),
            })
          )
        })
        response.on("error", () => {
          finish(() => reject(new InternalTransportError()))
        })
      }
    )

    const timer = setTimeout(() => {
      finish(() => reject(new InternalTimeoutError()))
      request.destroy()
    }, timeoutMs)

    request.on("error", () => {
      finish(() => reject(new InternalTransportError()))
    })
    request.end(body)
  })
}

function translateTransportError(error: unknown): never {
  if (error instanceof InternalTimeoutError) throw new HarborTimeoutError()
  if (error instanceof InternalResponseLimitError) {
    throw new HarborOperationError("Harbor returned too much data.")
  }
  throw new HarborUnavailableError()
}

function parseDaemonJson(response: UnixResponse): DaemonResponse {
  let parsed: unknown
  try {
    parsed = JSON.parse(response.body)
  } catch {
    throw new HarborOperationError("Harbor returned an invalid response.")
  }
  if (!isObject(parsed)) {
    throw new HarborOperationError("Harbor returned an invalid response.")
  }
  return parsed
}

async function daemonPing(): Promise<void> {
  let response: UnixResponse
  try {
    response = await requestUnix("GET", "/ping", undefined, DEFAULT_TIMEOUT_MS)
  } catch (error) {
    translateTransportError(error)
  }
  if (response.statusCode !== 200 || response.body.trim() !== "pong") {
    throw new HarborUnavailableError()
  }
}

async function daemonCall(
  endpoint: string,
  input: JsonObject,
  timeoutMs: number,
  safeErrorMessage: string,
  allowDeploymentError = false
): Promise<DaemonResponse> {
  let response: UnixResponse
  try {
    response = await requestUnix(
      "POST",
      endpoint,
      JSON.stringify(input),
      timeoutMs
    )
  } catch (error) {
    translateTransportError(error)
  }

  if (response.statusCode === 404) throw new HarborNotFoundError()
  const payload = parseDaemonJson(response)
  const hasOperationError =
    response.statusCode < 200 ||
    response.statusCode >= 300 ||
    (typeof payload.Error === "string" && payload.Error.length > 0)

  if (
    hasOperationError &&
    !(allowDeploymentError && isObject(payload.Deployment))
  ) {
    throw new HarborOperationError(safeErrorMessage)
  }
  return payload
}

async function onboardingDaemonCall(
  method: "GET" | "POST" | "DELETE",
  endpoint: string,
  input: JsonObject | undefined,
  timeoutMs: number
): Promise<JsonObject> {
  let response: UnixResponse
  try {
    response = await requestUnix(
      method,
      endpoint,
      input === undefined ? undefined : JSON.stringify(input),
      timeoutMs
    )
  } catch (error) {
    translateTransportError(error)
  }
  const payload = parseDaemonJson(response)
  if (response.statusCode < 200 || response.statusCode >= 300) {
    const message = safeString(payload.error, 512)
    throw new HarborOperationError(
      message || "Harbor could not complete onboarding."
    )
  }
  return payload
}

function safeRouteUrl(
  value: unknown,
  deployment: Deployment | null,
  domains: string[] = []
): string {
  if (!deployment || deployment.State !== "RUNNING" || !deployment.AppName) {
    return ""
  }
  if (typeof value !== "string" || value.length > 256) return ""
  try {
    const parsed = new URL(value)
    if (
      parsed.username ||
      parsed.password ||
      parsed.pathname !== "/" ||
      parsed.search ||
      parsed.hash
    ) {
      return ""
    }

    if (
      parsed.protocol === "https:" &&
      domains.includes(parsed.hostname.toLowerCase()) &&
      (!parsed.port || parsed.port === "443")
    ) {
      return parsed.origin
    }
    if (
      parsed.protocol !== "http:" ||
      parsed.hostname !== `${deployment.AppName}.localhost` ||
      !parsed.port
    ) {
      return ""
    }

    const port = Number(parsed.port)
    if (!Number.isInteger(port) || port < 1 || port > 65535) return ""
    return parsed.origin
  } catch {
    return ""
  }
}

function configuredHTTPSUrl(
  deployment: Deployment | null,
  domains: string[]
): string {
  if (!deployment || deployment.State !== "RUNNING") return ""
  const domain = domains[0]
  return domain ? `https://${domain}` : ""
}

export async function getAppSettingsData(name: string): Promise<AppSettings> {
  const response = await daemonCall(
    "/settings",
    { Name: name },
    DEFAULT_TIMEOUT_MS,
    "Could not load app settings."
  )
  return mapSettings(response)
}

export async function configureAppData(
  input: ConfigureAppInput
): Promise<AppSettings> {
  const response = await daemonCall(
    "/configure",
    {
      Name: input.name,
      ...(input.environment !== undefined
        ? { Environment: input.environment }
        : {}),
      ...(input.removeEnvironment !== undefined
        ? { RemoveEnvironment: input.removeEnvironment }
        : {}),
      ...(input.domains !== undefined ? { Domains: input.domains } : {}),
      ...(input.memoryLimit !== undefined
        ? { MemoryLimit: input.memoryLimit }
        : {}),
      ...(input.cpuQuota !== undefined ? { CPUQuota: input.cpuQuota } : {}),
      ...(input.tasksLimit !== undefined
        ? { TasksLimit: input.tasksLimit }
        : {}),
    },
    DEFAULT_TIMEOUT_MS,
    "Could not update app settings."
  )
  return mapSettings(response)
}

async function getActiveRouteUrl(name: string): Promise<string> {
  try {
    const response = await daemonCall(
      "/inspect",
      { Name: name },
      DEFAULT_TIMEOUT_MS,
      "Could not inspect the active app route."
    )
    const deployment = mapDeployment(response.Deployment)
    const settings = await getAppSettingsData(name).catch(() => emptySettings)
    return (
      configuredHTTPSUrl(deployment, settings.domains) ||
      safeRouteUrl(response.URL, deployment, settings.domains)
    )
  } catch {
    return ""
  }
}

function arrayValue(value: unknown): unknown[] {
  if (value === undefined || value === null) return []
  if (!Array.isArray(value)) {
    throw new HarborOperationError("Harbor returned invalid list data.")
  }
  return value
}

function isTransportFailure(error: unknown): boolean {
  return (
    error instanceof HarborUnavailableError ||
    error instanceof HarborTimeoutError
  )
}

export async function getOverviewData(): Promise<Overview> {
  try {
    await daemonPing()
  } catch {
    return {
      connected: false,
      apps: [],
      services: [],
      error: SAFE_DAEMON_UNAVAILABLE,
    }
  }

  const [appsResult, servicesResult] = await Promise.allSettled([
    daemonCall("/apps", {}, DEFAULT_TIMEOUT_MS, "Could not load applications."),
    daemonCall("/services", {}, DEFAULT_TIMEOUT_MS, "Could not load services."),
  ])

  if (
    (appsResult.status === "rejected" &&
      isTransportFailure(appsResult.reason)) ||
    (servicesResult.status === "rejected" &&
      isTransportFailure(servicesResult.reason))
  ) {
    return {
      connected: false,
      apps: [],
      services: [],
      error: SAFE_DAEMON_UNAVAILABLE,
    }
  }

  let apps: AppView[] = []
  let services: Service[] = []
  let error: string | undefined

  if (appsResult.status === "fulfilled") {
    try {
      apps = arrayValue(appsResult.value.Apps).map(mapAppView)
    } catch {
      error = "Could not load applications."
    }
  } else {
    error = "Could not load applications."
  }

  if (servicesResult.status === "fulfilled") {
    try {
      services = arrayValue(servicesResult.value.Services).map(mapService)
    } catch {
      error = error
        ? `${error} Service inventory is unavailable.`
        : "Service inventory is unavailable."
    }
  } else {
    error = error
      ? `${error} Service inventory is unavailable.`
      : "Service inventory is unavailable."
  }

  return {
    connected: true,
    apps,
    services,
    ...(error ? { error } : {}),
  }
}

export async function getAppData(name: string): Promise<AppDetails> {
  const [inspectResult, releasesResult, logsResult, settingsResult] =
    await Promise.allSettled([
      daemonCall(
        "/inspect",
        { Name: name },
        DEFAULT_TIMEOUT_MS,
        "Could not load app status."
      ),
      daemonCall(
        "/releases",
        { Name: name },
        DEFAULT_TIMEOUT_MS,
        "Could not load app releases."
      ),
      daemonCall(
        "/logs",
        { Name: name },
        DEFAULT_TIMEOUT_MS,
        "Could not load app logs."
      ),
      getAppSettingsData(name),
    ])

  if (
    [inspectResult, releasesResult, logsResult, settingsResult].some(
      (result) =>
        result.status === "rejected" && isTransportFailure(result.reason)
    )
  ) {
    throw new HarborUnavailableError()
  }

  if (
    inspectResult.status === "rejected" &&
    !(inspectResult.reason instanceof HarborNotFoundError)
  ) {
    throw new HarborOperationError("Could not load app status.")
  }
  if (
    releasesResult.status === "rejected" &&
    !(releasesResult.reason instanceof HarborNotFoundError)
  ) {
    throw new HarborOperationError("Could not load app releases.")
  }
  if (
    logsResult.status === "rejected" &&
    !(logsResult.reason instanceof HarborNotFoundError)
  ) {
    throw new HarborOperationError("Could not load app logs.")
  }
  if (
    settingsResult.status === "rejected" &&
    !(settingsResult.reason instanceof HarborNotFoundError)
  ) {
    throw new HarborOperationError("Could not load app settings.")
  }

  const inspect =
    inspectResult.status === "fulfilled" ? inspectResult.value : null
  const releases =
    releasesResult.status === "fulfilled" ? releasesResult.value : null
  const logs = logsResult.status === "fulfilled" ? logsResult.value : null
  const settings =
    settingsResult.status === "fulfilled" ? settingsResult.value : emptySettings
  const deployment = inspect ? mapDeployment(inspect.Deployment) : null

  return {
    deployment,
    releases: arrayValue(releases?.Releases).map(mapRelease),
    logs: sanitizeLogs(logs?.Logs),
    url:
      configuredHTTPSUrl(deployment, settings.domains) ||
      safeRouteUrl(inspect?.URL, deployment, settings.domains),
    settings,
  }
}

export async function getCapacityData(
  name?: string
): Promise<CapacitySnapshot> {
  const response = await daemonCall(
    "/capacity",
    name ? { Name: name } : {},
    DEFAULT_TIMEOUT_MS,
    "Could not load capacity metrics."
  )
  return mapCapacity(response.Capacity)
}

export async function deployAppData(input: {
  name: string
  repoURL: string
  ref: string
  rootDir: string
}): Promise<DeploymentResult> {
  const response = await daemonCall(
    "/deploy",
    {
      Name: input.name,
      RepoURL: input.repoURL,
      Ref: input.ref,
      RootDir: input.rootDir,
    },
    DEPLOY_TIMEOUT_MS,
    "Could not deploy this app. Review the repository settings and app logs.",
    true
  )
  const deployment = mapDeployment(response.Deployment)
  if (!deployment) {
    throw new HarborOperationError("Harbor did not return a deployment result.")
  }
  return {
    deployment,
    url: await getPublicOrLocalRouteUrl(input.name, response.URL, deployment),
  }
}

function mapGitHubConnection(value: unknown): GitHubConnection {
  if (!isObject(value))
    throw new HarborOperationError(
      "Harbor returned invalid GitHub connection data."
    )
  return {
    connected: value.connected === true,
    ...(typeof value.login === "string"
      ? { login: safeString(value.login, 64) }
      : {}),
    ...(value.connectedAt ? { connectedAt: safeTime(value.connectedAt) } : {}),
  }
}

function mapOnboardingConfig(value: unknown): ProjectInspection["config"] {
  if (!isObject(value))
    throw new HarborOperationError(
      "Harbor returned invalid project configuration."
    )
  const runtime =
    value.runtime === "go" || value.runtime === "node" ? value.runtime : null
  if (!runtime)
    throw new HarborOperationError(
      "Harbor returned an unsupported project runtime."
    )
  return {
    runtime,
    buildCommand: safeString(value.buildCommand, 4096),
    startCommand: safeString(value.startCommand, 4096),
    healthPath: safeString(value.healthPath, 256),
    memoryLimit: safeString(value.memoryLimit, 32),
    cpuQuota: safeNumber(value.cpuQuota),
    tasksLimit: safeNumber(value.tasksLimit),
  }
}

function mapProjectInspection(value: unknown): ProjectInspection {
  if (!isObject(value))
    throw new HarborOperationError(
      "Harbor returned invalid project inspection data."
    )
  return {
    existingConfig: value.existingConfig === true,
    config: mapOnboardingConfig(value.config),
    environmentKeys: arrayValue(value.environmentKeys)
      .map((key) => safeString(key, 128))
      .filter((key) => /^[A-Za-z_][A-Za-z0-9_]{0,127}$/.test(key)),
    warnings: arrayValue(value.warnings)
      .map((warning) => safeString(warning, 512))
      .filter(Boolean),
  }
}

const jobStates = new Set([
  "QUEUED",
  "RUNNING",
  "SUCCEEDED",
  "FAILED",
  "CANCELLING",
  "CANCELED",
])
function mapDeploymentJob(value: unknown): DeploymentJob {
  if (!isObject(value))
    throw new HarborOperationError(
      "Harbor returned invalid deployment job data."
    )
  const stateValue = safeString(value.state, 24).toUpperCase()
  const state = jobStates.has(stateValue)
    ? (stateValue as DeploymentJob["state"])
    : "FAILED"
  const logs = arrayValue(value.logs)
    .map((line) => sanitizeLogs(line))
    .filter(Boolean)
  return {
    id: safeString(value.id, 64),
    name: safeSlug(value.name),
    state,
    stage: safeString(value.stage, 32),
    createdAt: safeTime(value.createdAt),
    updatedAt: safeTime(value.updatedAt),
    deployment: mapDeployment(value.deployment),
    error: safeString(value.error, 512),
    logs,
  }
}

async function onboardingJobCall(
  method: "GET" | "POST" | "DELETE",
  endpoint: string,
  input?: JsonObject,
  timeoutMs = ONBOARDING_PREFLIGHT_TIMEOUT_MS
): Promise<DeploymentJob> {
  const response = await onboardingDaemonCall(
    method,
    endpoint,
    input,
    timeoutMs
  )
  return mapDeploymentJob(response.job)
}

export async function getGitHubConnectionData(): Promise<GitHubConnection> {
  const response = await onboardingDaemonCall(
    "GET",
    "/github/connection",
    undefined,
    DEFAULT_TIMEOUT_MS
  )
  return mapGitHubConnection(response.connection)
}

export async function connectGitHubData(
  token: string
): Promise<GitHubConnection> {
  const response = await onboardingDaemonCall(
    "POST",
    "/github/connection",
    { token },
    DEFAULT_TIMEOUT_MS
  )
  return mapGitHubConnection(response.connection)
}

export async function disconnectGitHubData(): Promise<GitHubConnection> {
  const response = await onboardingDaemonCall(
    "DELETE",
    "/github/connection",
    undefined,
    DEFAULT_TIMEOUT_MS
  )
  return mapGitHubConnection(response.connection)
}

export async function inspectProjectData(
  input: InspectProjectInput
): Promise<ProjectInspection> {
  const response = await onboardingDaemonCall(
    "POST",
    "/projects/inspect",
    input,
    ONBOARDING_PREFLIGHT_TIMEOUT_MS
  )
  return mapProjectInspection(response.inspection)
}

export async function submitOnboardingJobData(
  input: SubmitOnboardingJobInput
): Promise<DeploymentJob> {
  return onboardingJobCall(
    "POST",
    "/jobs/deploy",
    input,
    ONBOARDING_PREFLIGHT_TIMEOUT_MS
  )
}

export async function getOnboardingJobData(id: string): Promise<DeploymentJob> {
  return onboardingJobCall(
    "GET",
    `/jobs/${encodeURIComponent(id)}`,
    undefined,
    DEFAULT_TIMEOUT_MS
  )
}

export async function cancelOnboardingJobData(
  id: string
): Promise<DeploymentJob> {
  return onboardingJobCall(
    "DELETE",
    `/jobs/${encodeURIComponent(id)}`,
    undefined,
    DEFAULT_TIMEOUT_MS
  )
}

export async function stopAppData(name: string): Promise<DeploymentResult> {
  const response = await daemonCall(
    "/stop",
    { Name: name },
    DEFAULT_TIMEOUT_MS,
    "Could not stop this app."
  )
  const deployment = mapDeployment(response.Deployment)
  if (!deployment) {
    throw new HarborOperationError("Harbor did not return a deployment result.")
  }
  return {
    deployment,
    url: await getPublicOrLocalRouteUrl(name, response.URL, deployment),
  }
}

export async function rollbackAppData(
  name: string,
  releaseID: string
): Promise<DeploymentResult> {
  const response = await daemonCall(
    "/rollback",
    { Name: name, ReleaseID: releaseID },
    DEPLOY_TIMEOUT_MS,
    "Could not roll back this app. Review the app logs.",
    true
  )
  const deployment = mapDeployment(response.Deployment)
  if (!deployment) {
    throw new HarborOperationError("Harbor did not return a deployment result.")
  }
  return {
    deployment,
    url: await getPublicOrLocalRouteUrl(name, response.URL, deployment),
  }
}

async function getPublicOrLocalRouteUrl(
  name: string,
  value: unknown,
  deployment: Deployment | null
): Promise<string> {
  const settings = await getAppSettingsData(name).catch(() => emptySettings)
  return (
    configuredHTTPSUrl(deployment, settings.domains) ||
    safeRouteUrl(value, deployment, settings.domains) ||
    (await getActiveRouteUrl(name))
  )
}

async function readLimitedJson(response: Response): Promise<unknown> {
  const reader = response.body?.getReader()
  if (!reader) throw new Error("GitHub returned an invalid response.")
  const chunks: Uint8Array[] = []
  let bytes = 0

  try {
    let result = await reader.read()
    while (!result.done) {
      const value = result.value
      bytes += value.byteLength
      if (bytes > MAX_GITHUB_RESPONSE_BYTES) {
        await reader.cancel()
        throw new Error("GitHub returned too much data.")
      }
      chunks.push(value)
      result = await reader.read()
    }
  } finally {
    reader.releaseLock()
  }

  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8")) as unknown
  } catch {
    throw new Error("GitHub returned an invalid response.")
  }
}

async function githubRequest(path: string): Promise<unknown> {
  const url = new URL(path, GITHUB_API_ORIGIN)
  if (url.origin !== GITHUB_API_ORIGIN) {
    throw new Error("GitHub request was rejected.")
  }

  const token = env.GITHUB_TOKEN?.trim()
  let response: Response
  try {
    response = await fetch(url, {
      headers: {
        Accept: "application/vnd.github+json",
        "User-Agent": "Harbor",
        "X-GitHub-Api-Version": "2026-03-10",
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
      },
      redirect: "error",
      signal: AbortSignal.timeout(DEFAULT_TIMEOUT_MS),
    })
  } catch {
    throw new Error("GitHub could not be reached.")
  }

  if (response.status === 401) {
    throw new Error(
      token
        ? "GitHub authorization failed. Check the server-side token."
        : "GitHub rejected the request."
    )
  }
  if (response.status === 403) {
    throw new Error(
      response.headers.get("x-ratelimit-remaining") === "0"
        ? "GitHub rate limit reached. Try again later."
        : "GitHub access is restricted. Check repository visibility and server token permissions."
    )
  }
  if (response.status === 404) {
    throw new Error("GitHub repository was not found or is inaccessible.")
  }
  if (!response.ok) throw new Error("GitHub could not complete the request.")
  return readLimitedJson(response)
}

function mapGitHubRepo(value: unknown): GitHubRepo {
  if (!isObject(value))
    throw new Error("GitHub returned invalid repository data.")

  const name = safeString(value.name, 100)
  const owner = isObject(value.owner) ? safeString(value.owner.login, 39) : ""
  if (
    !/^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/.test(name) ||
    !/^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$/.test(owner)
  ) {
    throw new Error("GitHub returned invalid repository data.")
  }

  const description = value.description
  const language = value.language
  return {
    fullName: `${owner}/${name}`,
    name,
    description:
      typeof description === "string" ? description.slice(0, 1_000) : null,
    url: `https://github.com/${owner}/${name}`,
    defaultBranch: safeString(value.default_branch, 255),
    language: typeof language === "string" ? language.slice(0, 100) : null,
    private: safeBoolean(value.private),
    updatedAt: safeTime(value.updated_at),
  }
}

export async function listGitHubReposData(owner = ""): Promise<GitHubRepo[]> {
  const normalizedOwner = owner.trim()
  let connected = false
  try {
    connected = (await getGitHubConnectionData()).connected
  } catch {
    /* Public listing remains available. */
  }
  if (connected) {
    const response = await onboardingDaemonCall(
      "GET",
      "/github/repos",
      undefined,
      DEFAULT_TIMEOUT_MS
    )
    const repos = arrayValue(response.repositories).map((value) => {
      if (!isObject(value))
        throw new HarborOperationError(
          "GitHub returned invalid repository data."
        )
      const [repoOwner, repoName, ...rest] = safeString(
        value.fullName,
        200
      ).split("/")
      if (rest.length || !repoOwner || !repoName)
        throw new HarborOperationError(
          "GitHub returned invalid repository data."
        )
      return mapGitHubRepo({
        owner: { login: repoOwner },
        name: repoName,
        default_branch: value.defaultBranch,
        description: value.description,
        language: value.language,
        private: value.private,
        updated_at: value.updatedAt,
      })
    })
    return normalizedOwner
      ? repos.filter(
          (repo) =>
            repo.fullName.split("/")[0]?.toLowerCase() ===
            normalizedOwner.toLowerCase()
        )
      : repos
  }
  if (
    !/^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$/.test(normalizedOwner)
  ) {
    throw new HarborOperationError(
      "Connect GitHub or enter an owner to browse public repositories."
    )
  }
  const repos: GitHubRepo[] = []
  for (let page = 1; page <= 10; page++) {
    const response = await githubRequest(
      `/users/${encodeURIComponent(normalizedOwner)}/repos?type=public&sort=updated&per_page=100&page=${page}`
    )
    if (!Array.isArray(response))
      throw new HarborOperationError("GitHub returned invalid repository data.")
    for (const item of response) repos.push(mapGitHubRepo(item))
    if (response.length < 100) break
  }
  return repos
}

export async function getGitHubRepoData(
  owner: string,
  repo: string
): Promise<GitHubRepo> {
  const encodedOwner = encodeURIComponent(owner)
  const encodedRepo = encodeURIComponent(repo)
  const detail = mapGitHubRepo(
    await githubRequest(`/repos/${encodedOwner}/${encodedRepo}`)
  )

  try {
    const branches = await githubRequest(
      `/repos/${encodedOwner}/${encodedRepo}/branches?per_page=100`
    )
    if (Array.isArray(branches)) {
      const mappedBranches: GitHubBranch[] = branches.flatMap((value) => {
        if (!isObject(value) || !isObject(value.commit)) return []
        const name = safeString(value.name, 255)
        const commitSha = safeString(value.commit.sha, 64)
        return name ? [{ name, commitSha }] : []
      })
      return { ...detail, branches: mappedBranches }
    }
  } catch {
    // Repository metadata is useful even when GitHub's optional branch list is
    // unavailable or rate limited.
  }
  return detail
}
