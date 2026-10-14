import { callDashboardFunction } from "./api.client"
import type {
  AppDetails,
  AppSettings,
  CapacitySnapshot,
  DeploymentJob,
  DeploymentResult,
  GitHubConnection,
  GitHubRepo,
  Overview,
  ProjectInspection,
} from "./types"
import {
  configureAppInputSchema,
  connectGitHubInputSchema,
  deployAppInputSchema,
  getAppInputSchema,
  getAppSettingsInputSchema,
  getCapacityInputSchema,
  getDeploymentJobInputSchema,
  getGitHubRepoInputSchema,
  inspectProjectInputSchema,
  listGitHubReposInputSchema,
  onboardingJobIDSchema,
  parseInput,
  rollbackAppInputSchema,
  stopAppInputSchema,
  submitOnboardingJobInputSchema,
} from "./validation"
import type {
  ConfigureAppInput,
  ConnectGitHubInput,
  DeployAppInput,
  GetAppInput,
  GetAppSettingsInput,
  GetCapacityInput,
  GetDeploymentJobInput,
  GetGitHubRepoInput,
  InspectProjectInput,
  ListGitHubReposInput,
  RollbackAppInput,
  StopAppInput,
  SubmitOnboardingJobInput,
  OnboardingJobIDInput,
} from "./validation"

type FunctionInput<T> = { data: T }

export function getOverview(): Promise<Overview> {
  return callDashboardFunction("getOverview")
}

export function getApp(input: FunctionInput<GetAppInput>): Promise<AppDetails> {
  return callDashboardFunction(
    "getApp",
    parseInput(getAppInputSchema, input.data)
  )
}

export function getAppSettings(
  input: FunctionInput<GetAppSettingsInput>
): Promise<AppSettings> {
  return callDashboardFunction(
    "getAppSettings",
    parseInput(getAppSettingsInputSchema, input.data)
  )
}

export function configureApp(
  input: FunctionInput<ConfigureAppInput>
): Promise<AppSettings> {
  return callDashboardFunction(
    "configureApp",
    parseInput(configureAppInputSchema, input.data)
  )
}

export function getCapacity(
  input: FunctionInput<GetCapacityInput>
): Promise<CapacitySnapshot> {
  return callDashboardFunction(
    "getCapacity",
    parseInput(getCapacityInputSchema, input.data)
  )
}

export function deployApp(
  input: FunctionInput<DeployAppInput>
): Promise<DeploymentResult> {
  return callDashboardFunction(
    "deployApp",
    parseInput(deployAppInputSchema, input.data)
  )
}

export function retryGitHubDeployment(
  input: FunctionInput<GetAppInput>
): Promise<DeploymentJob> {
  return callDashboardFunction(
    "retryGitHubDeployment",
    parseInput(getAppInputSchema, input.data)
  )
}

export function getDeploymentJob(
  input: FunctionInput<GetDeploymentJobInput>
): Promise<DeploymentJob> {
  return callDashboardFunction(
    "getDeploymentJob",
    parseInput(getDeploymentJobInputSchema, input.data)
  )
}

export function stopApp(
  input: FunctionInput<StopAppInput>
): Promise<DeploymentResult> {
  return callDashboardFunction(
    "stopApp",
    parseInput(stopAppInputSchema, input.data)
  )
}

export function rollbackApp(
  input: FunctionInput<RollbackAppInput>
): Promise<DeploymentResult> {
  return callDashboardFunction(
    "rollbackApp",
    parseInput(rollbackAppInputSchema, input.data)
  )
}

export function listGitHubRepos(
  input: FunctionInput<ListGitHubReposInput>
): Promise<GitHubRepo[]> {
  return callDashboardFunction(
    "listGitHubRepos",
    parseInput(listGitHubReposInputSchema, input.data)
  )
}

export function getGitHubRepo(
  input: FunctionInput<GetGitHubRepoInput>
): Promise<GitHubRepo> {
  return callDashboardFunction(
    "getGitHubRepo",
    parseInput(getGitHubRepoInputSchema, input.data)
  )
}

export function getGitHubConnection(): Promise<GitHubConnection> {
  return callDashboardFunction("getGitHubConnection")
}

export function connectGitHub(
  input: FunctionInput<ConnectGitHubInput>
): Promise<GitHubConnection> {
  return callDashboardFunction(
    "connectGitHub",
    parseInput(connectGitHubInputSchema, input.data)
  )
}

export function disconnectGitHub(): Promise<GitHubConnection> {
  return callDashboardFunction("disconnectGitHub")
}

export function inspectProject(
  input: FunctionInput<InspectProjectInput>
): Promise<ProjectInspection> {
  return callDashboardFunction(
    "inspectProject",
    parseInput(inspectProjectInputSchema, input.data)
  )
}

export function submitOnboardingJob(
  input: FunctionInput<SubmitOnboardingJobInput>
): Promise<DeploymentJob> {
  return callDashboardFunction(
    "submitOnboardingJob",
    parseInput(submitOnboardingJobInputSchema, input.data)
  )
}

export function getOnboardingJob(
  input: FunctionInput<OnboardingJobIDInput>
): Promise<DeploymentJob> {
  return callDashboardFunction(
    "getOnboardingJob",
    parseInput(onboardingJobIDSchema, input.data)
  )
}

export function cancelOnboardingJob(
  input: FunctionInput<OnboardingJobIDInput>
): Promise<DeploymentJob> {
  return callDashboardFunction(
    "cancelOnboardingJob",
    parseInput(onboardingJobIDSchema, input.data)
  )
}
