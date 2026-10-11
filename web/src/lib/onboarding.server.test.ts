import {
  createServer,
  type IncomingMessage,
  type ServerResponse,
} from "node:http"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { once } from "node:events"
import { afterEach, describe, expect, it, vi } from "vitest"
import { listGitHubReposData } from "./harbor.server"

const savedSocket = process.env.PAASD_SOCKET
const savedToken = process.env.GITHUB_TOKEN

async function withDaemon(
  respond: (request: IncomingMessage, response: ServerResponse) => void,
  callback: () => Promise<void>
) {
  const dir = await mkdtemp(join(tmpdir(), "harbor-onboarding-web-"))
  const socketPath = join(dir, "daemon.sock")
  process.env.PAASD_SOCKET = socketPath
  const server = createServer(respond)
  server.listen(socketPath)
  await once(server, "listening")
  try {
    await callback()
  } finally {
    await new Promise<void>((resolve, reject) =>
      server.close((error) => (error ? reject(error) : resolve()))
    )
    await rm(dir, { recursive: true, force: true })
    if (savedSocket === undefined) delete process.env.PAASD_SOCKET
    else process.env.PAASD_SOCKET = savedSocket
  }
}

function sendJSON(response: ServerResponse, value: unknown) {
  response.writeHead(200, { "content-type": "application/json" })
  response.end(JSON.stringify(value))
}

afterEach(() => {
  vi.unstubAllGlobals()
  if (savedToken === undefined) delete process.env.GITHUB_TOKEN
  else process.env.GITHUB_TOKEN = savedToken
})

describe("GitHub repository listing transport", () => {
  it("maps connected private repositories from the daemon and filters by owner", async () => {
    await withDaemon(
      (request, response) => {
        if (request.url === "/github/connection") {
          sendJSON(response, {
            connection: { connected: true, login: "harbor-user" },
          })
          return
        }
        if (request.url === "/github/repos") {
          sendJSON(response, {
            repositories: [
              {
                fullName: "team/private-api",
                name: "private-api",
                url: "https://github.com/team/private-api",
                defaultBranch: "main",
                description: "private source",
                language: "Go",
                private: true,
                updatedAt: "2026-10-01T00:00:00Z",
              },
              {
                fullName: "other/web",
                name: "web",
                url: "https://github.com/other/web",
                defaultBranch: "main",
                private: false,
              },
            ],
          })
          return
        }
        response.writeHead(404).end()
      },
      async () => {
        const all = await listGitHubReposData()
        expect(all.map((repo) => repo.fullName)).toEqual([
          "team/private-api",
          "other/web",
        ])
        expect(all[0]).toMatchObject({
          private: true,
          url: "https://github.com/team/private-api",
          defaultBranch: "main",
        })
        const filtered = await listGitHubReposData("team")
        expect(filtered.map((repo) => repo.fullName)).toEqual([
          "team/private-api",
        ])
      }
    )
  })

  it("keeps public owner browsing available without a connected daemon token", async () => {
    process.env.GITHUB_TOKEN = ""
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input))
      expect(url.pathname).toBe("/users/octocat/repos")
      expect(url.searchParams.get("type")).toBe("public")
      return new Response(
        JSON.stringify([
          {
            name: "hello-world",
            owner: { login: "octocat" },
            default_branch: "main",
            description: "sample",
            language: "Go",
            private: false,
            updated_at: "2026-10-01T00:00:00Z",
          },
        ]),
        { status: 200, headers: { "content-type": "application/json" } }
      )
    })
    vi.stubGlobal("fetch", fetchMock)
    await withDaemon(
      (_request, response) =>
        sendJSON(response, { connection: { connected: false } }),
      async () => {
        const repos = await listGitHubReposData("octocat")
        expect(repos.map((repo) => repo.fullName)).toEqual([
          "octocat/hello-world",
        ])
        expect(fetchMock).toHaveBeenCalledOnce()
      }
    )
  })
})
