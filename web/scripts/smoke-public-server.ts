import { randomBytes, scryptSync } from "node:crypto"
import { spawn } from "node:child_process"
import { readFile } from "node:fs/promises"
import { createServer } from "node:net"
import { once } from "node:events"
import { setTimeout as delay } from "node:timers/promises"
import { join } from "node:path"
import { toJSONAsync } from "seroval"

const webDirectory = new URL("..", import.meta.url).pathname
const manifestPath = join(webDirectory, ".output/server/_ssr/ssr.mjs")
let manifest: string
try {
  manifest = await readFile(manifestPath, "utf8")
} catch {
  throw new Error("Run `bun run build` before the public server smoke test.")
}

function findServerFnID(name: string): string {
  const match = manifest.match(
    new RegExp(`"([a-f0-9]{64})": \\{\\s*functionName: "${name}_createServerFn_handler"`)
  )
  if (!match?.[1]) {
    throw new Error(`Could not find the built ${name} server function.`)
  }
  return match[1]
}

async function allocatePort(): Promise<number> {
  const server = createServer()
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject)
    server.listen(0, "127.0.0.1", resolve)
  })
  const address = server.address()
  if (!address || typeof address === "string") {
    throw new Error("Could not allocate a loopback port.")
  }
  await new Promise<void>((resolve, reject) => {
    server.close((error) => (error ? reject(error) : resolve()))
  })
  return address.port
}

function assert(condition: boolean, message: string): void {
  if (!condition) throw new Error(message)
  console.log(`PASS ${message}`)
}

async function stop(child: ReturnType<typeof spawn>): Promise<void> {
  if (child.exitCode !== null) return
  const exited = once(child, "exit").then(() => true)
  child.kill("SIGTERM")
  if (!(await Promise.race([exited, delay(3_000).then(() => false)]))) {
    child.kill("SIGKILL")
  }
}

const port = await allocatePort()
const password = randomBytes(24).toString("base64url")
const salt = randomBytes(32)
const passwordDigest = scryptSync(password, salt, 32, {
  N: 32_768,
  r: 8,
  p: 1,
  maxmem: 64 * 1024 * 1024,
})
const serverFunctions = {
  auth: findServerFnID("getAuthStatus"),
  overview: findServerFnID("getOverview"),
  app: findServerFnID("getApp"),
  login: findServerFnID("login"),
  logout: findServerFnID("logout"),
}
const child = spawn(process.execPath, [join(webDirectory, ".output/server/index.mjs")], {
  cwd: webDirectory,
  env: {
    ...process.env,
    HOST: "127.0.0.1",
    PORT: String(port),
    PAASD_PUBLIC_ORIGIN: "https://dashboard.example.test",
    PAASD_ADMIN_PASSWORD_HASH: `scrypt$32768$8$1$${salt.toString("hex")}$${passwordDigest.toString("hex")}`,
    PAASD_SESSION_SECRET: randomBytes(32).toString("hex"),
    PAASD_SOCKET: join(webDirectory, ".smoke-no-daemon.sock"),
  },
  stdio: ["ignore", "ignore", "pipe"],
})
let childError = ""
child.stderr.setEncoding("utf8").on("data", (chunk: string) => {
  childError += chunk
})
const origin = "https://dashboard.example.test"
const baseURL = `http://127.0.0.1:${port}`
const hostHeaders = { host: "dashboard.example.test" }

async function waitForServer(): Promise<void> {
  for (let attempt = 0; attempt < 100; attempt += 1) {
    if (child.exitCode !== null) {
      throw new Error(`The built server exited early: ${childError}`)
    }
    try {
      await fetch(`${baseURL}/login`, { headers: hostHeaders })
      return
    } catch {
      await delay(100)
    }
  }
  throw new Error(`The built server did not start: ${childError}`)
}

try {
  await waitForServer()

  let response = await fetch(`${baseURL}/login`, {
    headers: hostHeaders,
    redirect: "manual",
  })
  assert(
    response.status === 200,
    "unauthenticated /login loads from the production build"
  )
  assert(
    response.headers.get("cache-control")?.includes("no-store") === true,
    "public HTML responses disable caching"
  )

  response = await fetch(`${baseURL}/_serverFn/${serverFunctions.auth}`, {
    headers: { ...hostHeaders, "x-tsr-serverFn": "true" },
  })
  assert(
    response.status === 200,
    "getAuthStatus exact generated GET URL remains public after bundling"
  )

  response = await fetch(`${baseURL}/_serverFn/${serverFunctions.overview}`, {
    headers: { ...hostHeaders, "x-tsr-serverFn": "true" },
  })
  assert(response.status === 401, "getOverview server function requires login")
  assert(
    response.headers.get("cache-control")?.includes("no-store") === true,
    "unauthorized server function responses disable caching"
  )
  response = await fetch(`${baseURL}/_serverFn/${serverFunctions.app}`, {
    headers: { ...hostHeaders, "x-tsr-serverFn": "true" },
  })
  assert(response.status === 401, "getApp server function also requires login")

  response = await fetch(baseURL, { headers: hostHeaders, redirect: "manual" })
  assert(
    response.status === 302 && response.headers.get("location") === "/login",
    "unauthenticated SSR redirects to login"
  )

  const loginPayload = JSON.stringify(
    await toJSONAsync({ data: { password } })
  )
  const postHeaders = {
    ...hostHeaders,
    "content-type": "application/json",
    "x-tsr-serverFn": "true",
    origin,
    "sec-fetch-site": "same-origin",
  }
  response = await fetch(`${baseURL}/_serverFn/${serverFunctions.login}`, {
    method: "POST",
    headers: {
      ...postHeaders,
      origin: "https://attacker.example",
      "sec-fetch-site": "cross-site",
    },
    body: loginPayload,
  })
  assert(response.status === 403, "login rejects cross-origin POST requests")

  response = await fetch(`${baseURL}/_serverFn/${serverFunctions.login}`, {
    method: "POST",
    headers: postHeaders,
    body: loginPayload,
  })
  assert(response.status === 200, "login server function works after bundling")
  const setCookie = response.headers.get("set-cookie") ?? ""
  assert(
    setCookie.startsWith("__Host-harbor_session=") &&
      /HttpOnly/i.test(setCookie) &&
      /Secure/i.test(setCookie),
    "login sets a secure HTTP-only signed session cookie"
  )
  const cookie = setCookie.split(";", 1)[0] ?? ""

  response = await fetch(baseURL, {
    headers: { ...hostHeaders, cookie },
    redirect: "manual",
  })
  assert(
    response.status === 200 &&
      response.headers.get("cache-control")?.includes("no-store") === true,
    "authenticated SSR works and cannot be cached"
  )

  response = await fetch(`${baseURL}/_serverFn/${serverFunctions.logout}`, {
    method: "POST",
    headers: {
      ...hostHeaders,
      cookie,
      "x-tsr-serverFn": "true",
      origin,
      "sec-fetch-site": "same-origin",
    },
  })
  assert(
    response.status === 200 &&
      /Max-Age=0|Expires=/i.test(response.headers.get("set-cookie") ?? ""),
    "logout expires the session cookie"
  )

  response = await fetch(`${baseURL}/_serverFn/${serverFunctions.overview}`, {
    headers: { ...hostHeaders, "x-tsr-serverFn": "true" },
  })
  assert(response.status === 401, "requests without the cleared cookie are anonymous")

  response = await fetch(baseURL, {
    headers: {
      host: "spoof.example.test",
      "x-forwarded-host": "dashboard.example.test",
    },
    redirect: "manual",
  })
  assert(response.status === 403, "public access rejects spoofed Host headers")

  response = await fetch(`${baseURL}/_serverFn/${serverFunctions.auth}`, {
    method: "POST",
    headers: {
      ...postHeaders,
      "content-type": "application/json",
    },
    body: loginPayload,
  })
  assert(
    response.status === 401,
    "auth exemption does not allow a method mismatch"
  )
} finally {
  await stop(child)
}
