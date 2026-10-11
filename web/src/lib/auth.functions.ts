import { deleteCookie, getRequest, setCookie } from "@tanstack/react-start/server"
import { createServerFn } from "@tanstack/react-start"
import { z } from "zod"
import {
  createSessionToken,
  getAuthConfiguration,
  hasValidSession,
  loginAttemptLimiter,
  SESSION_COOKIE,
  SESSION_MAX_AGE_SECONDS,
  verifyPassword,
} from "./auth.server"
import { parseInput } from "./validation"

const loginInputSchema = z
  .object({ password: z.string().min(1).max(1_024) })
  .strict()

export const getAuthStatus = createServerFn({ method: "GET" }).handler(() => {
  const configuration = getAuthConfiguration()
  if (configuration.mode === "local") {
    return { mode: "local" as const, authenticated: true }
  }
  return {
    mode: "public" as const,
    authenticated: hasValidSession(getRequest(), configuration),
  }
})

export const login = createServerFn({ method: "POST" })
  .validator((input: unknown) => parseInput(loginInputSchema, input))
  .handler(async ({ data }) => {
    const configuration = getAuthConfiguration()
    if (configuration.mode !== "public") {
      return { ok: false as const, message: "Sign-in is unavailable." }
    }

    const attempt = loginAttemptLimiter.consume()
    if (!attempt.allowed) {
      return {
        ok: false as const,
        message: `Too many sign-in attempts. Try again in ${Math.ceil(attempt.retryAfterSeconds / 60)} minutes.`,
      }
    }

    if (!(await verifyPassword(data.password, configuration.passwordHash))) {
      return { ok: false as const, message: "The password is incorrect." }
    }

    loginAttemptLimiter.reset()
    setCookie(
      SESSION_COOKIE,
      createSessionToken(configuration.sessionSecret),
      {
        path: "/",
        httpOnly: true,
        secure: true,
        sameSite: "lax",
        maxAge: SESSION_MAX_AGE_SECONDS,
      }
    )
    return { ok: true as const }
  })

export const logout = createServerFn({ method: "POST" }).handler(() => {
  deleteCookie(SESSION_COOKIE, {
    path: "/",
    httpOnly: true,
    secure: true,
    sameSite: "lax",
    maxAge: 0,
  })
  return { ok: true }
})
