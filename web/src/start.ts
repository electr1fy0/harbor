import {
  createCsrfMiddleware,
  createMiddleware,
  createStart,
} from "@tanstack/react-start"

const localHostGuard = createMiddleware().server(async ({ next, request }) => {
  const host = request.headers.get("host")
  if (!host) return new Response("Forbidden", { status: 403 })

  let hostname: string
  try {
    hostname = new URL(`http://${host}`).hostname.toLowerCase()
  } catch {
    return new Response("Forbidden", { status: 403 })
  }

  if (!["localhost", "127.0.0.1", "[::1]"].includes(hostname)) {
    return new Response("Forbidden", { status: 403 })
  }
  return next()
})

const sameOriginServerFunctionMiddleware = createCsrfMiddleware({
  filter: ({ handlerType }) => handlerType === "serverFn",
})

export const startInstance = createStart(() => ({
  requestMiddleware: [localHostGuard, sameOriginServerFunctionMiddleware],
}))
