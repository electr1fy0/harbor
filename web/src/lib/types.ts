/** Public deployment states returned by the Harbor daemon. */
export type DeploymentState =
  | "QUEUED"
  | "BUILDING"
  | "BUILT"
  | "STARTING"
  | "HEALTH_CHECKING"
  | "RUNNING"
  | "BUILD_FAILED"
  | "START_FAILED"
  | "UNHEALTHY"
  | "CRASHED"
  | "OOM_KILLED"
  | "STOPPED"
  | "UNKNOWN"

/** Safe subset of model.Application. */
export interface Application {
  ID: string
  Name: string
  CreatedAt: string
}

/**
 * Safe subset of model.Deployment. Host paths, process identifiers, sandbox
 * identifiers, and systemd details are deliberately excluded.
 */
export interface Deployment {
  ID: string
  AppID: string
  AppName: string
  ReleaseID: string
  Port: number
  State: DeploymentState
  /** A frontend-safe summary; the daemon's raw error is never returned. */
  Error: string
  MemoryLimit: string
  CPUQuota: number
  TasksLimit: number
  ReadinessKnown: boolean
  HealthPath: string
  PromotedAt: string | null
  CreatedAt: string
  StartedAt: string | null
  StoppedAt: string | null
}

/** Shape of daemon.AppView using Go's default PascalCase JSON field names. */
export interface AppView {
  Application: Application
  Deployment: Deployment | null
  /** Deployment currently mapped to production, or null when no route serves. */
  Production: Deployment | null
  /** Most recent deployment attempt, even if an older production stays live. */
  LatestAttempt: Deployment | null
}

export type DiagnosticActionID =
  | "view_attempt_logs"
  | "configure_settings"
  | "retry_github_deploy"
  | "rollback_release"

export interface DiagnosticAction {
  id: DiagnosticActionID
  label: string
  available: boolean
  reason: string
  releaseID?: string
}

export interface AppDiagnostics {
  production: Deployment | null
  latestAttempt: Deployment | null
  summary: string
  excerpt: string
  actions: DiagnosticAction[]
}

export interface FailureExplanation {
  deploymentID: string
  model: "gpt-5.4-mini"
  explanation: string
  evidence: string[]
  nextStep: string
  cached: boolean
}

export type DeploymentJobState =
  | "QUEUED"
  | "RUNNING"
  | "SUCCEEDED"
  | "FAILED"
  | "CANCELLING"
  | "CANCELED"
  | "UNKNOWN"

export interface DeploymentJob {
  id: string
  name: string
  state: DeploymentJobState
  stage: string
  createdAt: string
  updatedAt: string
  deployment: Deployment | null
  error: string
  logs: string[]
}

export interface Service {
  id: string
  name: string
  type: string
  version: string
  port: number
  state: string
  createdAt: string
}

/** Safe subset of model.ReleaseView; ArtifactDir is intentionally omitted. */
export interface ReleaseView {
  id: string
  appId: string
  appName: string
  sourceHash: string
  runtime: string
  createdAt: string
  isActive: boolean
  artifactAvailable: boolean
}

export interface Overview {
  connected: boolean
  apps: AppView[]
  services: Service[]
  error?: string
}

export interface AppDetails {
  deployment: Deployment | null
  diagnostics: AppDiagnostics | null
  releases: ReleaseView[]
  logs: string
  url: string
  settings: AppSettings
}

/** Safe project settings metadata. Secret values are never returned. */
export interface AppSettings {
  domains: string[]
  environmentKeys: string[]
  memoryLimit: string
  cpuQuota: number
  tasksLimit: number
}

export interface DeploymentResult {
  deployment: Deployment | null
  url: string
}

export interface GitHubBranch {
  name: string
  commitSha: string
}

export interface GitHubRepo {
  fullName: string
  name: string
  description: string | null
  url: string
  defaultBranch: string
  language: string | null
  private: boolean
  updatedAt: string
  branches?: GitHubBranch[]
}

export interface CapacitySnapshot {
  sampledAt: string
  platform: { supported: boolean; name: string; reason: string }
  host: {
    memoryTotalBytes: number | null
    memoryAvailableBytes: number | null
    memoryUsedBytes: number | null
    cpuUsagePercent: number | null
    logicalCPUs: number | null
  }
  disk: {
    filesystemTotalBytes: number | null
    filesystemAvailableBytes: number | null
    filesystemUsedBytes: number | null
    harborDataBytes: number | null
    harborDataComplete: boolean
    harborDataChangeBytes: number | null
    releasesChangeBytes: number | null
    buildCacheChangeBytes: number | null
    logsChangeBytes: number | null
    changeIntervalSeconds: number | null
    releasesBytes: number | null
    buildCacheBytes: number | null
    logsBytes: number | null
  }
  apps: Array<{
    deploymentId: string
    name: string
    state: string
    memoryLimitBytes: number | null
    memoryCurrentBytes: number | null
    memoryPeakBytes: number | null
    cpuQuotaCores: number | null
    cpuUsageCores: number | null
    tasksLimit: number | null
    tasksCurrent: number | null
    metricsSource: string
  }>
  builds: {
    systemdManagedActiveCount: number | null
    memoryLimitBytes: number | null
    memoryCurrentBytes: number | null
    memoryPeakBytes: number | null
    cpuQuotaCores: number | null
    cpuUsageCores: number | null
  }
  headroom: {
    memoryReservePercent: number
    memoryReserveBytes: number | null
    estimatedMemoryAfterReserveBytes: number | null
  }
  caveats: string[]
}

export interface GitHubConnection {
  connected: boolean
  login?: string
  connectedAt?: string
}

export interface OnboardingConfig {
  runtime: "go" | "node"
  buildCommand: string
  startCommand: string
  healthPath: string
  memoryLimit: string
  cpuQuota: number
  tasksLimit: number
}

export interface ProjectInspection {
  existingConfig: boolean
  config: OnboardingConfig
  environmentKeys: string[]
  warnings: string[]
}
