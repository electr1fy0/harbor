import { describe, expect, it } from "vitest"
import {
  createSessionToken,
  decideRequestAccess,
  getAuthConfiguration,
  hasValidSession,
  LOGIN_ATTEMPT_LIMIT,
  LOGIN_WINDOW_MS,
  LoginAttemptLimiter,
  requestHasTrustedHost,
  requestHasTrustedOrigin,
  SESSION_COOKIE,
  SESSION_MAX_AGE_SECONDS,
  verifySessionToken,
} from "./auth.server"

const sessionSecret = Buffer.alloc(32, 7)
const passwordHash = `scrypt$32768$8$1$${"a".repeat(64)}$${"b".repeat(64)}`
const publicConfiguration = {
  mode: "public" as const,
  origin: "https://dashboard.example.test",
  passwordHash,
  sessionSecret,
}

describe("public access controls", () => {
  it("fails closed when any public auth setting is present but configuration is incomplete", () => {
    const configuration = getAuthConfiguration({
      PAASD_PUBLIC_ORIGIN: "https://dashboard.example.test",
      PAASD_ADMIN_PASSWORD_HASH: passwordHash,
    })

    expect(configuration.mode).toBe("misconfigured")
    expect(
      decideRequestAccess(
        {
          request: new Request("https://dashboard.example.test/"),
          pathname: "/",
          handlerType: "router",
        },
        configuration
      )
    ).toEqual({ type: "locked" })
  })

  it("keeps local mode loopback-only and ignores forwarded host headers", () => {
    const local = getAuthConfiguration({})
    const spoofed = new Request("http://attacker.example/", {
      headers: {
        host: "attacker.example",
        "x-forwarded-host": "localhost:3000",
        "x-forwarded-proto": "http",
      },
    })

    expect(requestHasTrustedHost(spoofed, local)).toBe(false)
    expect(
      decideRequestAccess(
        { request: spoofed, pathname: "/", handlerType: "router" },
        local
      )
    ).toEqual({ type: "forbidden" })
    expect(
      requestHasTrustedHost(
        new Request("http://127.0.0.1:3000/", {
          headers: { host: "127.0.0.1:3000" },
        }),
        local
      )
    ).toBe(true)
  })

  it("requires the configured public Host and does not trust forwarded host values", () => {
    const wrongHost = new Request("https://attacker.example/", {
      headers: {
        host: "attacker.example",
        "x-forwarded-host": "dashboard.example.test",
        "x-forwarded-proto": "https",
      },
    })
    expect(requestHasTrustedHost(wrongHost, publicConfiguration)).toBe(false)

    const exactHost = new Request("https://dashboard.example.test/", {
      headers: {
        host: "dashboard.example.test",
        "x-forwarded-host": "attacker.example",
      },
    })
    expect(requestHasTrustedHost(exactHost, publicConfiguration)).toBe(true)
  })

  it("rejects cross-origin and origin-less state-changing requests", () => {
    const crossOrigin = new Request("https://dashboard.example.test/auth", {
      method: "POST",
      headers: {
        host: "dashboard.example.test",
        origin: "https://attacker.example",
        "sec-fetch-site": "cross-site",
      },
    })
    const noOrigin = new Request("https://dashboard.example.test/auth", {
      method: "POST",
      headers: { host: "dashboard.example.test" },
    })

    expect(requestHasTrustedOrigin(crossOrigin, publicConfiguration)).toBe(false)
    expect(requestHasTrustedOrigin(noOrigin, publicConfiguration)).toBe(false)
    expect(
      decideRequestAccess(
        {
          request: crossOrigin,
          pathname: "/auth",
          handlerType: "serverFn",
          serverFnName: "login",
        },
        publicConfiguration
      )
    ).toEqual({ type: "forbidden" })
  })

  it("blocks protected server functions and allows only public auth functions before login", () => {
    const request = new Request("https://dashboard.example.test/", {
      headers: { host: "dashboard.example.test" },
    })

    expect(
      decideRequestAccess(
        {
          request,
          pathname: "/",
          handlerType: "serverFn",
          serverFnName: "getOverview",
        },
        publicConfiguration
      )
    ).toEqual({ type: "unauthorized" })
    expect(
      decideRequestAccess(
        {
          request,
          pathname: "/",
          handlerType: "serverFn",
          serverFnName: "getAuthStatus",
        },
        publicConfiguration
      )
    ).toEqual({ type: "allow" })
    expect(
      decideRequestAccess(
        { request, pathname: "/projects", handlerType: "router" },
        publicConfiguration
      )
    ).toEqual({ type: "redirect" })
  })

  it("accepts only signed, untampered, unexpired session cookies", () => {
    const now = Date.UTC(2026, 9, 11, 9, 0, 0)
    const token = createSessionToken(sessionSecret, now)
    const request = new Request("https://dashboard.example.test/", {
      headers: {
        host: "dashboard.example.test",
        cookie: `${SESSION_COOKIE}=${token}`,
      },
    })

    expect(verifySessionToken(token, sessionSecret, now)).toBe(true)
    expect(hasValidSession(request, publicConfiguration, now)).toBe(true)
    expect(verifySessionToken(`${token}x`, sessionSecret, now)).toBe(false)
    expect(
      hasValidSession(
        new Request("https://dashboard.example.test/", {
          headers: {
            host: "dashboard.example.test",
            cookie: `${SESSION_COOKIE}=${token}`,
          },
        }),
        publicConfiguration,
        now + (SESSION_MAX_AGE_SECONDS + 1) * 1_000
      )
    ).toBe(false)
  })

  it("bounds login attempts in a rolling process-wide window", () => {
    const limiter = new LoginAttemptLimiter()
    const now = 1_000_000
    for (let attempt = 0; attempt < LOGIN_ATTEMPT_LIMIT; attempt += 1) {
      expect(limiter.consume(now).allowed).toBe(true)
    }
    expect(limiter.consume(now).allowed).toBe(false)
    expect(limiter.consume(now + LOGIN_WINDOW_MS + 1).allowed).toBe(true)
  })
})
