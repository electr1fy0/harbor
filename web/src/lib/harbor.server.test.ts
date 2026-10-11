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
                type: "postgres",
                version: "16",
                port: 5432,
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
