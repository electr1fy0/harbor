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
}

export interface Overview {
  connected: boolean
  apps: AppView[]
  services: Service[]
  error?: string
}

export interface AppDetails {
  deployment: Deployment | null
  releases: ReleaseView[]
  logs: string
  url: string
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
