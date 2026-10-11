import {
  createHmac,
  randomBytes,
  scrypt,
  timingSafeEqual,
} from "node:crypto"

export const SESSION_COOKIE = "__Host-harbor_session"
export const SESSION_MAX_AGE_SECONDS = 8 * 60 * 60
export const LOGIN_WINDOW_MS = 15 * 60 * 1_000
export const LOGIN_ATTEMPT_LIMIT = 5

const PASSWORD_HASH_PATTERN =
  /^scrypt\$32768\$8\$1\$([a-f0-9]{64})\$([a-f0-9]{64})$/i

export type AuthConfiguration =
  | { mode: "local" }
  | { mode: "misconfigured" }
  | {
      mode: "public"
      origin: string
      passwordHash: string
      sessionSecret: Buffer
    }

export interface AccessRequest {
  request: Request
  pathname: string
  handlerType: "serverFn" | "router"
  serverFnName?: string
}

export type AccessDecision =
  | { type: "allow" }
  | { type: "locked" }
  | { type: "forbidden" }
  | { type: "unauthorized" }
  | { type: "redirect" }

function readTrustedOrigin(value: string): string | undefined {
  if (!value || value !== value.trim()) return undefined
  try {
    const url = new URL(value)
    if (
      url.protocol !== "https:" ||
      url.username ||
      url.password ||
      url.pathname !== "/" ||
      url.search ||
      url.hash ||
      value !== url.origin
    ) {
      return undefined
    }
    return url.origin
  } catch {
    return undefined
  }
}

export function getAuthConfiguration(
  source: NodeJS.ProcessEnv = process.env
): AuthConfiguration {
  const rawOrigin = source.PAASD_PUBLIC_ORIGIN?.trim() ?? ""
  const rawPasswordHash = source.PAASD_ADMIN_PASSWORD_HASH?.trim() ?? ""
  const rawSessionSecret = source.PAASD_SESSION_SECRET?.trim() ?? ""
  const publicModeRequested = Boolean(
    rawOrigin || rawPasswordHash || rawSessionSecret
  )

  if (!publicModeRequested) return { mode: "local" }

  const origin = readTrustedOrigin(rawOrigin)
  const sessionSecret = /^[a-f0-9]{64}$/i.test(rawSessionSecret)
    ? Buffer.from(rawSessionSecret, "hex")
    : undefined
  if (
    !origin ||
    !PASSWORD_HASH_PATTERN.test(rawPasswordHash) ||
    !sessionSecret
  ) {
    return { mode: "misconfigured" }
  }

  return {
    mode: "public",
    origin,
    passwordHash: rawPasswordHash,
    sessionSecret,
  }
}

function parseHostHeader(request: Request): URL | null {
  const host = request.headers.get("host")
  if (!host || host.length > 255 || /[\s,/@\\]/.test(host)) return null
  try {
    const parsed = new URL(`http://${host}`)
    if (parsed.username || parsed.password || parsed.pathname !== "/") {
      return null
    }
    return parsed
  } catch {
    return null
  }
}

export function requestHasTrustedHost(
  request: Request,
  configuration: AuthConfiguration
): boolean {
  const host = parseHostHeader(request)
  if (!host) return false

  if (configuration.mode === "public") {
    return host.host.toLowerCase() === new URL(configuration.origin).host
  }
  if (configuration.mode === "misconfigured") return false
  const hostname = host.hostname.toLowerCase().replace(/^\[|\]$/g, "")
  return ["localhost", "127.0.0.1", "::1"].includes(hostname)
}

export function requestHasTrustedOrigin(
  request: Request,
  configuration: AuthConfiguration
): boolean {
  const origin = request.headers.get("origin")
  if (!origin || !requestHasTrustedHost(request, configuration)) return false

  let expectedOrigin: string
  if (configuration.mode === "public") {
    expectedOrigin = configuration.origin
  } else if (configuration.mode === "local") {
    try {
      const requestUrl = new URL(request.url)
      const host = parseHostHeader(request)
      if (
        requestUrl.protocol !== "http:" ||
        !host ||
        requestUrl.host.toLowerCase() !== host.host.toLowerCase()
      ) {
        return false
      }
      expectedOrigin = requestUrl.origin
    } catch {
      return false
    }
  } else {
    return false
  }

  if (origin !== expectedOrigin) return false
  const fetchSite = request.headers.get("sec-fetch-site")
  return fetchSite === null || fetchSite === "same-origin"
}

function getSessionCookie(request: Request): string | undefined {
  const cookieHeader = request.headers.get("cookie")
  if (!cookieHeader || cookieHeader.length > 8_192) return undefined
  for (const cookie of cookieHeader.split(";")) {
    const separator = cookie.indexOf("=")
    if (separator < 0) continue
    if (cookie.slice(0, separator).trim() === SESSION_COOKIE) {
      return cookie.slice(separator + 1).trim()
    }
  }
  return undefined
}

export function createSessionToken(
  secret: Uint8Array,
  nowMs = Date.now()
): string {
  const issuedAt = Math.floor(nowMs / 1_000)
  const payload = Buffer.from(
    JSON.stringify({
      version: 1,
      issuedAt,
      expiresAt: issuedAt + SESSION_MAX_AGE_SECONDS,
      nonce: randomBytes(16).toString("base64url"),
    })
  ).toString("base64url")
  const signature = createHmac("sha256", secret)
    .update(payload)
    .digest("base64url")
  return `${payload}.${signature}`
}

export function verifySessionToken(
  token: string | undefined,
  secret: Uint8Array,
  nowMs = Date.now()
): boolean {
  if (!token || token.length > 1_024) return false
  const pieces = token.split(".")
  if (pieces.length !== 2 || !pieces[0] || !pieces[1]) return false
  const [payload, receivedSignature] = pieces
  if (!payload || !receivedSignature) return false

  const expectedSignature = createHmac("sha256", secret)
    .update(payload)
    .digest()
  let actualSignature: Buffer
  try {
    actualSignature = Buffer.from(receivedSignature, "base64url")
  } catch {
    return false
  }
  if (
    actualSignature.length !== expectedSignature.length ||
    !timingSafeEqual(actualSignature, expectedSignature)
  ) {
    return false
  }

  try {
    const session: unknown = JSON.parse(
      Buffer.from(payload, "base64url").toString("utf8")
    )
    if (typeof session !== "object" || session === null) return false
    const value = session as Record<string, unknown>
    const now = Math.floor(nowMs / 1_000)
    return (
      value.version === 1 &&
      typeof value.issuedAt === "number" &&
      typeof value.expiresAt === "number" &&
      typeof value.nonce === "string" &&
      value.nonce.length >= 16 &&
      value.issuedAt <= now + 60 &&
      value.expiresAt > now &&
      value.expiresAt > value.issuedAt &&
      value.expiresAt - value.issuedAt <= SESSION_MAX_AGE_SECONDS
    )
  } catch {
    return false
  }
}

export function hasValidSession(
  request: Request,
  configuration: AuthConfiguration,
  nowMs = Date.now()
): boolean {
  return (
    configuration.mode === "public" &&
    verifySessionToken(
      getSessionCookie(request),
      configuration.sessionSecret,
      nowMs
    )
  )
}

const PUBLIC_AUTH_FUNCTIONS = new Set(["getAuthStatus", "login"])

export function decideRequestAccess(
  access: AccessRequest,
  configuration: AuthConfiguration,
  nowMs = Date.now()
): AccessDecision {
  const { request, pathname, handlerType, serverFnName } = access
  if (configuration.mode === "misconfigured") return { type: "locked" }
  if (!requestHasTrustedHost(request, configuration)) {
    return { type: "forbidden" }
  }

  if (!/^(GET|HEAD|OPTIONS)$/i.test(request.method)) {
    if (!requestHasTrustedOrigin(request, configuration)) {
      return { type: "forbidden" }
    }
  }

  if (configuration.mode === "local") return { type: "allow" }

  const authenticated = hasValidSession(request, configuration, nowMs)
  if (handlerType === "serverFn") {
    if (
      !authenticated &&
      !(serverFnName && PUBLIC_AUTH_FUNCTIONS.has(serverFnName))
    ) {
      return { type: "unauthorized" }
    }
    return { type: "allow" }
  }

  if (pathname === "/login") return { type: "allow" }
  return authenticated ? { type: "allow" } : { type: "redirect" }
}

export async function verifyPassword(
  password: string,
  encodedHash: string
): Promise<boolean> {
  const match = PASSWORD_HASH_PATTERN.exec(encodedHash)
  if (!match || Buffer.byteLength(password, "utf8") > 1_024) return false
  const [, saltHex, hashHex] = match
  if (!saltHex || !hashHex) return false
  const expected = Buffer.from(hashHex, "hex")
  const actual = await new Promise<Buffer>((resolve, reject) => {
    scrypt(
      password,
      Buffer.from(saltHex, "hex"),
      32,
      { N: 32_768, r: 8, p: 1, maxmem: 64 * 1024 * 1024 },
      (error, derivedKey) => {
        if (error) reject(error)
        else resolve(derivedKey)
      }
    )
  })
  return actual.length === expected.length && timingSafeEqual(actual, expected)
}

export class LoginAttemptLimiter {
  private attempts = 0
  private resetAt = 0

  consume(nowMs = Date.now()): { allowed: boolean; retryAfterSeconds: number } {
    if (nowMs >= this.resetAt) {
      this.attempts = 0
      this.resetAt = nowMs + LOGIN_WINDOW_MS
    }
    if (this.attempts >= LOGIN_ATTEMPT_LIMIT) {
      return {
        allowed: false,
        retryAfterSeconds: Math.max(1, Math.ceil((this.resetAt - nowMs) / 1_000)),
      }
    }
    this.attempts += 1
    return { allowed: true, retryAfterSeconds: 0 }
  }

  reset(): void {
    this.attempts = 0
    this.resetAt = 0
  }
}

export const loginAttemptLimiter = new LoginAttemptLimiter()
