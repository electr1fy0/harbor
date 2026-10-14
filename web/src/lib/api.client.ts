export class HarborNotFoundError extends Error {
  constructor() {
    super("Harbor resource was not found.")
    this.name = "HarborNotFoundError"
  }
}

export class HarborAPIError extends Error {
  readonly status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = "HarborAPIError"
    this.status = status
  }
}

type RequestOptions = {
  method: "GET" | "POST"
  body?: unknown
}

async function requestJSON<T>(
  path: string,
  { method, body }: RequestOptions
): Promise<T> {
  const response = await fetch(path, {
    method,
    credentials: "same-origin",
    cache: "no-store",
    headers:
      body === undefined ? undefined : { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  })

  if (response.status === 401 && typeof window !== "undefined") {
    window.dispatchEvent(new Event("harbor:unauthorized"))
  }
  if (response.status === 404) throw new HarborNotFoundError()

  let payload: unknown
  try {
    payload = await response.json()
  } catch {
    throw new HarborAPIError(
      response.ok
        ? "Harbor returned an invalid response."
        : response.statusText,
      response.status
    )
  }

  if (!response.ok) {
    const message =
      typeof payload === "object" &&
      payload !== null &&
      "error" in payload &&
      typeof payload.error === "string"
        ? payload.error
        : typeof payload === "object" &&
            payload !== null &&
            "message" in payload &&
            typeof payload.message === "string"
          ? payload.message
          : response.statusText || "Harbor request failed."
    throw new HarborAPIError(message, response.status)
  }

  return payload as T
}

export type DashboardFunctionName =
  | "operations"
  | "automation"
  | "dataServices"
  | "observe"
  | "getOverview"
  | "getApp"
  | "getAppSettings"
  | "configureApp"
  | "getCapacity"
  | "deployApp"
  | "retryGitHubDeployment"
  | "getDeploymentJob"
  | "stopApp"
  | "rollbackApp"
  | "listGitHubRepos"
  | "getGitHubRepo"
  | "getGitHubConnection"
  | "connectGitHub"
  | "disconnectGitHub"
  | "inspectProject"
  | "submitOnboardingJob"
  | "getOnboardingJob"
  | "cancelOnboardingJob"

export function callDashboardFunction<T>(
  functionName: DashboardFunctionName,
  input: unknown = {}
): Promise<T> {
  return requestJSON<T>(`/api/dashboard/${functionName}`, {
    method: "POST",
    body: input,
  })
}

export type AuthStatus = {
  mode: "local" | "public"
  authenticated: boolean
}

export type LoginResult = { ok: boolean; message?: string }

export function getAuthStatus(): Promise<AuthStatus> {
  return requestJSON<AuthStatus>("/api/auth/status", { method: "GET" })
}

export function login(password: string): Promise<LoginResult> {
  return requestJSON<LoginResult>("/api/auth/login", {
    method: "POST",
    body: { password },
  })
}

export function logout(): Promise<{ ok: true }> {
  return requestJSON<{ ok: true }>("/api/auth/logout", {
    method: "POST",
    body: {},
  })
}
