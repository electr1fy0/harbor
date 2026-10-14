import { afterEach, describe, expect, it, vi } from "vitest"
import { HarborNotFoundError } from "./api.client"
import type { HarborAPIError } from "./api.client"
import { getAuthStatus, login, logout } from "./auth.functions"
import { configureApp, explainFailure, getOverview } from "./harbor.functions"

const fetchMock = vi.fn<typeof fetch>()

afterEach(() => {
  fetchMock.mockReset()
  vi.unstubAllGlobals()
})

describe("browser API transport", () => {
  it("posts function inputs directly to the dashboard API", async () => {
    fetchMock.mockResolvedValue(
      Response.json({ connected: true, apps: [], services: [] })
    )
    vi.stubGlobal("fetch", fetchMock)

    await getOverview()

    expect(fetchMock).toHaveBeenCalledWith(
      "/api/dashboard/getOverview",
      expect.objectContaining({
        method: "POST",
        credentials: "same-origin",
        cache: "no-store",
        body: "{}",
      })
    )
  })

  it("keeps validation normalization and sends only the input object", async () => {
    fetchMock.mockResolvedValue(Response.json({ domains: ["example.com"] }))
    vi.stubGlobal("fetch", fetchMock)

    await configureApp({
      data: { name: "demo", domains: [" EXAMPLE.COM "], cpuQuota: 1 },
    })

    expect(fetchMock).toHaveBeenCalledWith(
      "/api/dashboard/configureApp",
      expect.objectContaining({
        body: JSON.stringify({
          name: "demo",
          domains: ["example.com"],
          cpuQuota: 1,
        }),
      })
    )
  })

  it("posts only the validated app and deployment identity for failure explanations", async () => {
    fetchMock.mockResolvedValue(
      Response.json({
        deploymentID: "dep_123",
        model: "gpt-5.4-mini",
        explanation: "The build could not resolve a package.",
        evidence: ["package not found"],
        nextStep: "Check the package name in the manifest.",
        cached: false,
      })
    )
    vi.stubGlobal("fetch", fetchMock)

    await explainFailure({
      data: { name: "demo", deploymentID: "dep_123" },
    })

    expect(fetchMock).toHaveBeenCalledWith(
      "/api/dashboard/explainFailure",
      expect.objectContaining({
        method: "POST",
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify({ name: "demo", deploymentID: "dep_123" }),
      })
    )
  })

  it("maps a 404 response to HarborNotFoundError", async () => {
    fetchMock.mockResolvedValue(
      Response.json({ error: "missing" }, { status: 404 })
    )
    vi.stubGlobal("fetch", fetchMock)

    await expect(getOverview()).rejects.toBeInstanceOf(HarborNotFoundError)
  })

  it("surfaces JSON API error messages and status codes", async () => {
    fetchMock.mockResolvedValue(
      Response.json({ error: "Harbor daemon is unavailable." }, { status: 503 })
    )
    vi.stubGlobal("fetch", fetchMock)

    await expect(getOverview()).rejects.toMatchObject<Partial<HarborAPIError>>({
      message: "Harbor daemon is unavailable.",
      status: 503,
    })
  })

  it("surfaces login throttling messages from auth responses", async () => {
    fetchMock.mockResolvedValue(
      Response.json(
        { ok: false, message: "Sign-in is busy. Try again shortly." },
        { status: 429 }
      )
    )
    vi.stubGlobal("fetch", fetchMock)

    await expect(login({ data: { password: "secret" } })).rejects.toMatchObject(
      {
        message: "Sign-in is busy. Try again shortly.",
        status: 429,
      }
    )
  })

  it("uses the auth endpoints with same-origin credentials", async () => {
    fetchMock
      .mockResolvedValueOnce(
        Response.json({ mode: "public", authenticated: false })
      )
      .mockResolvedValueOnce(Response.json({ ok: true }))
      .mockResolvedValueOnce(Response.json({ ok: true }))
    vi.stubGlobal("fetch", fetchMock)

    await getAuthStatus()
    await login({ data: { password: "secret" } })
    await logout()

    expect(fetchMock.mock.calls).toEqual([
      [
        "/api/auth/status",
        expect.objectContaining({ method: "GET", credentials: "same-origin" }),
      ],
      [
        "/api/auth/login",
        expect.objectContaining({
          method: "POST",
          credentials: "same-origin",
          body: JSON.stringify({ password: "secret" }),
        }),
      ],
      [
        "/api/auth/logout",
        expect.objectContaining({
          method: "POST",
          credentials: "same-origin",
          body: "{}",
        }),
      ],
    ])
  })
})
