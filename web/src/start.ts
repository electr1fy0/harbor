import { createMiddleware, createStart } from "@tanstack/react-start"
import { getAuthStatus, login } from "./lib/auth.functions"
import { decideRequestAccess, getAuthConfiguration } from "./lib/auth.server"

const authFunctionForRequest = new Map<string, { name: string; method: string }>([
  [getAuthStatus.url, { name: "getAuthStatus", method: "GET" }],
  [login.url, { name: "login", method: "POST" }],
])

const accessControl = createMiddleware().server(
  async ({ next, request, pathname, handlerType }) => {
    const authFunction = authFunctionForRequest.get(pathname)
    const exactAuthFunctionName =
      authFunction?.method === request.method.toUpperCase()
        ? authFunction.name
        : undefined
    const decision = decideRequestAccess(
      {
        request,
        pathname,
        handlerType,
        // Request middleware runs before Start resolves a server function, so
        // use the generated RPC paths and their exact methods here.
        serverFnName: exactAuthFunctionName,
      },
      getAuthConfiguration()
    )

    if (decision.type === "allow") {
      const result = await next()
      if (getAuthConfiguration().mode !== "public") return result

      const response = result.response
      const headers = new Headers(response.headers)
      const setCookies = response.headers.getSetCookie()
      if (setCookies.length > 0) {
        headers.delete("set-cookie")
        for (const cookie of setCookies) headers.append("set-cookie", cookie)
      }
      headers.set("cache-control", "private, no-store, max-age=0")
      headers.set("x-content-type-options", "nosniff")
      return {
        ...result,
        response: new Response(response.body, {
          status: response.status,
          statusText: response.statusText,
          headers,
        }),
      }
    }

    if (decision.type === "locked") {
      return new Response("Harbor public access is not configured safely.", {
        status: 503,
        headers: { "cache-control": "no-store" },
      })
    }
    if (decision.type === "forbidden") {
      return new Response("Forbidden", {
        status: 403,
        headers: { "cache-control": "no-store" },
      })
    }
    if (decision.type === "unauthorized") {
      return new Response("Unauthorized", {
        status: 401,
        headers: { "cache-control": "no-store" },
      })
    }

    return new Response(null, {
      status: 302,
      headers: {
        location: "/login",
        "cache-control": "no-store",
      },
    })
  }
)

export const startInstance = createStart(() => ({
  requestMiddleware: [accessControl],
}))
