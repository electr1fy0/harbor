import { createServer } from "node:http"
import type {
  IncomingMessage,
  RequestListener,
  ServerResponse,
} from "node:http"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { env } from "node:process"
import { afterEach, describe, expect, it } from "vitest"
import {
  configureAppData,
  deployAppData,
  getAppData,
  getCapacityData,
  getOverviewData,
} from "./harbor.server"

const temporaryDirectories: string[] = []
const previousSocketPath = env.PAASD_SOCKET

afterEach(async () => {
  if (previousSocketPath === undefined) delete env.PAASD_SOCKET
  else env.PAASD_SOCKET = previousSocketPath

  await Promise.all(
    temporaryDirectories
      .splice(0)
      .map((directory) => rm(directory, { recursive: true, force: true }))
  )
})

async function withDaemon(
  handler: RequestListener,
  action: () => Promise<void>
): Promise<void> {
  const directory = await mkdtemp(join(tmpdir(), "harbor-web-test-"))
  temporaryDirectories.push(directory)
  const socketPath = join(directory, "paasd.sock")
  const server = createServer(handler)
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject)
    server.listen(socketPath, resolve)
  })
  env.PAASD_SOCKET = socketPath

  try {
    await action()
  } finally {
    await new Promise<void>((resolve, reject) => {
      server.close((error) => (error ? reject(error) : resolve()))
    })
  }
}

function jsonResponse(
  response: ServerResponse<IncomingMessage>,
  status: number,
  value: unknown
) {
  response.statusCode = status
  response.setHeader("content-type", "application/json")
  response.end(JSON.stringify(value))
}

describe("Harbor daemon transport", () => {
  it("returns only settings metadata and links to a configured HTTPS domain", async () => {
    let configureBody: Record<string, unknown> | undefined
    await withDaemon(
      (request, response) => {
        if (request.url === "/inspect") {
          jsonResponse(response, 200, {
            URL: "https://app.example.com",
            Deployment: {
              ID: "dep_1",
              AppID: "app_1",
              AppName: "harbor-web",
              ReleaseID: "rel_1",
              Port: 8080,
              State: "RUNNING",
              CreatedAt: "2026-01-01T00:00:00Z",
            },
          })
        } else if (request.url === "/releases") {
          jsonResponse(response, 200, { Releases: [] })
        } else if (request.url === "/logs") {
          jsonResponse(response, 200, { Logs: "" })
        } else if (request.url === "/settings") {
          jsonResponse(response, 200, {
            settings: {
              domains: ["app.example.com", "bad/host.example"],
              environmentKeys: ["API_TOKEN"],
              memoryLimit: "256Mi",
              cpuQuota: 1.5,
              tasksLimit: 48,
              environment: { API_TOKEN: "must-not-be-returned" },
            },
          })
        } else if (request.url === "/configure") {
          const chunks: Buffer[] = []
          request.on("data", (chunk: Buffer | string) => {
            chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk))
          })
          request.on("end", () => {
            configureBody = JSON.parse(Buffer.concat(chunks).toString("utf8"))
            jsonResponse(response, 200, {
              settings: {
                domains: ["app.example.com"],
                environmentKeys: ["API_TOKEN"],
                memoryLimit: "256Mi",
                cpuQuota: 1.5,
                tasksLimit: 48,
                environment: { API_TOKEN: "must-not-be-returned" },
              },
            })
          })
        } else {
          response.statusCode = 404
          response.end("not found")
        }
      },
      async () => {
        const app = await getAppData("harbor-web")
        expect(app.url).toBe("https://app.example.com")
        expect(app.diagnostics).toBeNull()
        expect(app.settings).toEqual({
          domains: ["app.example.com"],
          environmentKeys: ["API_TOKEN"],
          memoryLimit: "256Mi",
          cpuQuota: 1.5,
          tasksLimit: 48,
        })
        expect(JSON.stringify(app)).not.toContain("must-not-be-returned")

        await configureAppData({
          name: "harbor-web",
          environment: { API_TOKEN: "new-private-value" },
          removeEnvironment: ["OLD_TOKEN"],
          domains: ["app.example.com"],
          memoryLimit: "512Mi",
          cpuQuota: 2,
          tasksLimit: 64,
        })
        expect(configureBody).toEqual({
          Name: "harbor-web",
          Environment: { API_TOKEN: "new-private-value" },
          RemoveEnvironment: ["OLD_TOKEN"],
          Domains: ["app.example.com"],
          MemoryLimit: "512Mi",
          CPUQuota: 2,
          TasksLimit: 64,
        })
      }
    )
  })

  it("loads a filtered capacity sample and preserves unavailable metrics", async () => {
    await withDaemon(
      (request, response) => {
        if (request.url !== "/capacity") {
          response.statusCode = 404
          response.end("not found")
          return
        }
        let body = ""
        request.setEncoding("utf8")
        request.on("data", (chunk: string) => (body += chunk))
        request.on("end", () => {
          expect(JSON.parse(body)).toEqual({ Name: "harbor-web" })
          jsonResponse(response, 200, {
            Capacity: {
              sampledAt: "2026-10-11T01:00:00Z",
              platform: { supported: true, name: "linux", reason: "" },
              host: {
                memoryTotalBytes: 2_147_483_648,
                memoryAvailableBytes: 1_073_741_824,
                memoryUsedBytes: 1_073_741_824,
                cpuUsagePercent: 18.5,
                logicalCPUs: 2,
              },
              disk: {
                filesystemTotalBytes: 20_000,
                filesystemAvailableBytes: 12_000,
                filesystemUsedBytes: 8_000,
                harborDataBytes: 1_000,
                harborDataComplete: true,
                harborDataChangeBytes: -4,
                releasesChangeBytes: 2,
                buildCacheChangeBytes: 0,
                logsChangeBytes: -6,
                changeIntervalSeconds: 15.5,
                releasesBytes: 500,
                buildCacheBytes: 300,
                logsBytes: 200,
              },
              apps: [
                {
                  deploymentId: "dep_42",
                  name: "harbor-web",
                  state: "RUNNING",
                  memoryLimitBytes: 134_217_728,
                  memoryCurrentBytes: 32_768,
                  memoryPeakBytes: 65_536,
                  cpuQuotaCores: 0.5,
                  cpuUsageCores: null,
                  tasksLimit: 64,
                  tasksCurrent: 2,
                  metricsSource: "cgroup v2",
                  logPath: "/var/lib/paas/private.log",
                },
              ],
              builds: {
                systemdManagedActiveCount: 0,
                memoryLimitBytes: 0,
                memoryCurrentBytes: 0,
                memoryPeakBytes: 0,
                cpuQuotaCores: 0,
                cpuUsageCores: 0,
              },
              headroom: {
                memoryReservePercent: 10,
                memoryReserveBytes: 214_748_364,
                estimatedMemoryAfterReserveBytes: 858_993_460,
              },
              caveats: ["Configured limits are ceilings, not reservations."],
              dataDir: "/var/lib/paas",
            },
          })
        })
      },
      async () => {
        const capacity = await getCapacityData("harbor-web")
        expect(capacity.platform.supported).toBe(true)
        expect(capacity.host.memoryTotalBytes).toBe(2_147_483_648)
        expect(capacity.apps[0]?.cpuUsageCores).toBeNull()
        expect(capacity.apps[0]?.memoryLimitBytes).toBe(134_217_728)
        expect(capacity.apps[0]?.deploymentId).toBe("dep_42")
        expect(capacity.disk.harborDataChangeBytes).toBe(-4)
        expect(capacity.disk.releasesChangeBytes).toBe(2)
        expect(capacity.disk.changeIntervalSeconds).toBe(15.5)
        expect(capacity.builds.systemdManagedActiveCount).toBe(0)
        expect(JSON.stringify(capacity)).not.toContain("/var/lib/paas")
      }
    )
  })

  it("keeps unsupported host and runtime values unavailable", async () => {
    await withDaemon(
      (_request, response) => {
        jsonResponse(response, 200, {
          Capacity: {
            sampledAt: "2026-10-11T01:00:00Z",
            platform: {
              supported: false,
              name: "darwin",
              reason: "Linux host metrics are unavailable.",
            },
            host: {},
            disk: {},
            apps: [
              {
                name: "configured-app",
                state: "RUNNING",
                memoryLimitBytes: 67_108_864,
                memoryCurrentBytes: null,
                cpuQuotaCores: 0.25,
                cpuUsageCores: null,
                metricsSource: "unavailable",
              },
            ],
            builds: {},
            headroom: { memoryReservePercent: 10 },
            caveats: [],
          },
        })
      },
      async () => {
        const capacity = await getCapacityData()
        expect(capacity.platform.supported).toBe(false)
        expect(capacity.host.memoryTotalBytes).toBeNull()
        expect(capacity.apps[0]?.memoryCurrentBytes).toBeNull()
        expect(capacity.apps[0]?.cpuUsageCores).toBeNull()
      }
    )
  })

  it("uses the Unix socket and returns only path-free overview fields", async () => {
    await withDaemon(
      (request, response) => {
        if (request.url === "/ping") {
          response.end("pong\n")
          return
        }
        if (request.url === "/apps") {
          jsonResponse(response, 200, {
            Apps: [
              {
                Application: {
                  ID: "app_1",
                  Name: "harbor-web",
                  CreatedAt: "2026-01-01T00:00:00Z",
                },
                Deployment: {
                  ID: "dep_1",
                  AppID: "app_1",
                  AppName: "harbor-web",
                  ReleaseID: "rel_1",
                  Port: 8080,
                  State: "RUNNING",
                  Error: "/run/paas/private-error",
                  PID: 501,
                  LogPath: "/run/paas/private.log",
                  SystemdUnit: "private.service",
                  SandboxID: "private-sandbox",
                  MemoryLimit: "512MiB",
                  CPUQuota: 1,
                  TasksLimit: 64,
                  ReadinessKnown: true,
                  HealthPath: "/health",
                  CreatedAt: "2026-01-01T00:00:00Z",
                  StartedAt: "2026-01-01T00:01:00Z",
                },
              },
            ],
          })
          return
        }
        if (request.url === "/services") {
          jsonResponse(response, 200, {
            Services: [
              {
                id: "svc_1",
                name: "primary-db",
                type: "sqlite",
                version: "",
                port: 0,
                state: "READY",
                volume_dir: "/var/lib/paas/services/private-db",
                created_at: "2026-01-01T00:00:00Z",
              },
            ],
          })
          return
        }
        response.statusCode = 404
        response.end("not found")
      },
      async () => {
        const overview = await getOverviewData()
        expect(overview.connected).toBe(true)
        expect(overview.apps[0]?.Application.Name).toBe("harbor-web")
        expect(overview.apps[0]?.Deployment?.State).toBe("RUNNING")
        expect(overview.services[0]?.name).toBe("primary-db")
        expect(JSON.stringify(overview)).not.toContain("/run/paas")
        expect(JSON.stringify(overview)).not.toContain("/var/lib/paas")
        expect(JSON.stringify(overview)).not.toContain("private-sandbox")
      }
    )
  })

  it("replaces daemon error details with a safe inventory message", async () => {
    await withDaemon(
      (request, response) => {
        if (request.url === "/ping") {
          response.end("pong\n")
        } else if (request.url === "/apps") {
          jsonResponse(response, 400, {
            Error:
              "failed at /run/paas/secret.sock with ghp_abcdefghijk123456789",
          })
        } else if (request.url === "/services") {
          jsonResponse(response, 200, { Services: [] })
        } else {
          response.statusCode = 404
          response.end("not found")
        }
      },
      async () => {
        const overview = await getOverviewData()
        expect(overview.connected).toBe(true)
        expect(overview.error).toBe("Could not load applications.")
        expect(JSON.stringify(overview)).not.toContain("ghp_")
        expect(JSON.stringify(overview)).not.toContain("/run/paas")
      }
    )
  })

  it("keeps failed deployment diagnostics useful while redacting secrets and paths", async () => {
    await withDaemon(
      (request, response) => {
        if (request.url === "/inspect") {
          jsonResponse(response, 200, {
            URL: "http://harbor-web.localhost:8080",
            Deployment: {
              ID: "dep_failed",
              AppID: "app_1",
              AppName: "harbor-web",
              ReleaseID: "",
              Port: 0,
              State: "BUILD_FAILED",
              Error: "git failed in /home/runner/project with token=private",
              LogPath: "/run/paas/private.log",
              CreatedAt: "2026-01-01T00:00:00Z",
            },
          })
        } else if (request.url === "/releases") {
          jsonResponse(response, 200, { Releases: [] })
        } else if (request.url === "/logs") {
          jsonResponse(response, 200, {
            Logs: "clone https://user:secret@github.com/owner/repo ghp_abcdefghijk123456789 /run/paas/build.log /home/runner/project",
          })
        } else {
          response.statusCode = 404
          response.end("not found")
        }
      },
      async () => {
        const app = await getAppData("harbor-web")
        expect(app.deployment?.State).toBe("BUILD_FAILED")
        expect(app.deployment?.Error).toBe(
          "Build failed. Review the app logs for details."
        )
        expect(app.url).toBe("")
        expect(app.logs).toContain("[redacted token]")
        expect(app.logs).toContain("[redacted path]")
        expect(app.logs).not.toContain("ghp_")
        expect(app.logs).not.toContain("/run/paas")
        expect(app.logs).not.toContain("/home/runner")
        expect(app.logs).not.toContain("user:secret")
      }
    )
  })

  it("maps production separately from a failed attempt and keeps diagnostics actions safe", async () => {
    let attemptLogsRequest: Record<string, unknown> | undefined
    await withDaemon(
      (request, response) => {
        if (request.url === "/inspect") {
          jsonResponse(response, 200, {
            URL: "https://app.example.com",
            Deployment: {
              ID: "dep_prod",
              AppID: "app_1",
              AppName: "harbor-web",
              ReleaseID: "rel_prod",
              Port: 8080,
              State: "RUNNING",
              CreatedAt: "2026-01-01T00:00:00Z",
            },
          })
        } else if (request.url === "/releases") {
          jsonResponse(response, 200, { Releases: [] })
        } else if (request.url === "/logs") {
          const chunks: Buffer[] = []
          request.on("data", (chunk: Buffer | string) => {
            chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk))
          })
          request.on("end", () => {
            attemptLogsRequest = JSON.parse(
              Buffer.concat(chunks).toString("utf8")
            )
            jsonResponse(response, 200, { Logs: "candidate build output" })
          })
        } else if (request.url === "/settings") {
          jsonResponse(response, 200, { settings: {} })
        } else if (request.url === "/diagnostics") {
          jsonResponse(response, 200, {
            diagnostics: {
              production: {
                ID: "dep_prod",
                AppID: "app_1",
                AppName: "harbor-web",
                ReleaseID: "rel_prod",
                Port: 8080,
                State: "RUNNING",
                CreatedAt: "2026-01-01T00:00:00Z",
              },
              latestAttempt: {
                ID: "dep_failed",
                AppID: "app_1",
                AppName: "harbor-web",
                ReleaseID: "rel_failed",
                Port: 0,
                State: "BUILD_FAILED",
                Error: "compiler said token=private",
                CreatedAt: "2026-01-02T00:00:00Z",
              },
              summary:
                "Production remains healthy and is serving traffic. Latest attempt: build failed.",
              excerpt: "token=private\ncompiler output",
              actions: [
                {
                  id: "view_attempt_logs",
                  available: true,
                },
                {
                  id: "rollback_release",
                  available: true,
                  releaseID: "rel_previous",
                },
                {
                  id: "unknown_action",
                  available: true,
                },
              ],
            },
          })
        } else {
          response.statusCode = 404
          response.end("not found")
        }
      },
      async () => {
        const app = await getAppData("harbor-web")
        expect(app.deployment?.ID).toBe("dep_prod")
        expect(app.diagnostics?.production?.ID).toBe("dep_prod")
        expect(app.diagnostics?.production?.State).toBe("RUNNING")
        expect(app.diagnostics?.latestAttempt?.ID).toBe("dep_failed")
        expect(app.diagnostics?.latestAttempt?.State).toBe("BUILD_FAILED")
        expect(app.diagnostics?.excerpt).toContain("compiler output")
        expect(app.diagnostics?.excerpt).not.toContain("private")
        expect(attemptLogsRequest).toEqual({
          Name: "harbor-web",
          DeploymentID: "dep_failed",
        })
        expect(app.diagnostics?.actions).toEqual([
          {
            id: "view_attempt_logs",
            label: "View failed attempt logs",
            available: true,
            reason: "",
            releaseID: "",
          },
          {
            id: "rollback_release",
            label: "Roll back last good release",
            available: true,
            reason: "",
            releaseID: "rel_previous",
          },
        ])
        expect(JSON.stringify(app)).not.toContain("unknown_action")
        expect(JSON.stringify(app)).not.toContain("token=private")
      }
    )
  })

  it("preserves an explicit null production route and maps a later successful attempt", async () => {
    let diagnostics: unknown = {
      production: null,
      latestAttempt: {
        ID: "dep_starting",
        AppID: "app_1",
        AppName: "harbor-web",
        ReleaseID: "rel_starting",
        Port: 8080,
        State: "RUNNING",
        CreatedAt: "2026-01-02T00:00:00Z",
      },
      summary:
        "The latest deployment reports RUNNING, but no production route is active.",
      actions: [],
    }

    await withDaemon(
      (request, response) => {
        if (request.url === "/inspect") {
          jsonResponse(response, 200, {
            Deployment: {
              ID: "dep_starting",
              AppID: "app_1",
              AppName: "harbor-web",
              ReleaseID: "rel_starting",
              Port: 8080,
              State: "RUNNING",
              CreatedAt: "2026-01-02T00:00:00Z",
            },
          })
        } else if (request.url === "/releases") {
          jsonResponse(response, 200, { Releases: [] })
        } else if (request.url === "/logs") {
          jsonResponse(response, 200, { Logs: "" })
        } else if (request.url === "/diagnostics") {
          jsonResponse(response, 200, { diagnostics })
        } else {
          response.statusCode = 404
          response.end("not found")
        }
      },
      async () => {
        const recovering = await getAppData("harbor-web")
        expect(recovering.deployment?.State).toBe("RUNNING")
        expect(recovering.diagnostics?.production).toBeNull()
        expect(recovering.diagnostics?.latestAttempt?.ID).toBe("dep_starting")

        const successfulDeployment = {
          ID: "dep_success",
          AppID: "app_1",
          AppName: "harbor-web",
          ReleaseID: "rel_success",
          Port: 8080,
          State: "RUNNING",
          CreatedAt: "2026-01-03T00:00:00Z",
        }
        diagnostics = {
          production: successfulDeployment,
          latestAttempt: successfulDeployment,
          summary:
            "The latest deployment is healthy and serving production traffic.",
          actions: [
            {
              id: "view_attempt_logs",
              available: false,
              reason: "No failed deployment attempt is recorded.",
            },
          ],
        }
        const successful = await getAppData("harbor-web")
        expect(successful.diagnostics?.production?.ID).toBe("dep_success")
        expect(successful.diagnostics?.latestAttempt?.ID).toBe("dep_success")
        expect(successful.diagnostics?.summary).not.toContain("failed")
        expect(successful.diagnostics?.actions[0]?.available).toBe(false)
      }
    )
  })

  it("returns the active route after a failed deployment candidate", async () => {
    await withDaemon(
      (request, response) => {
        if (request.url === "/deploy") {
          jsonResponse(response, 400, {
            URL: "http://harbor-web.localhost:9999",
            Error: "build failed at /run/paas/private-path",
            Deployment: {
              ID: "dep_failed",
              AppID: "app_1",
              AppName: "harbor-web",
              ReleaseID: "",
              Port: 0,
              State: "BUILD_FAILED",
              Error: "build failed at /run/paas/private-path",
              CreatedAt: "2026-01-01T00:00:00Z",
            },
          })
        } else if (request.url === "/inspect") {
          jsonResponse(response, 200, {
            URL: "http://harbor-web.localhost:8080",
            Deployment: {
              ID: "dep_active",
              AppID: "app_1",
              AppName: "harbor-web",
              ReleaseID: "rel_1",
              Port: 3000,
              State: "RUNNING",
              CreatedAt: "2026-01-01T00:00:00Z",
            },
          })
        } else {
          response.statusCode = 404
          response.end("not found")
        }
      },
      async () => {
        const result = await deployAppData({
          name: "harbor-web",
          repoURL: "https://github.com/harbor/example",
          ref: "main",
          rootDir: "",
        })
        expect(result.deployment?.State).toBe("BUILD_FAILED")
        expect(result.deployment?.Error).toContain("Review the app logs")
        expect(result.url).toBe("http://harbor-web.localhost:8080")
        expect(JSON.stringify(result)).not.toContain("/run/paas")
      }
    )
  })
})
