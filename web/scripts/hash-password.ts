import { randomBytes, scryptSync } from "node:crypto"
import { emitKeypressEvents } from "node:readline"
import { stdin, stdout } from "node:process"

function readHidden(prompt: string): Promise<string> {
  if (!stdin.isTTY || typeof stdin.setRawMode !== "function") {
    throw new Error("Run this utility in an interactive terminal.")
  }

  emitKeypressEvents(stdin)
  stdin.setRawMode(true)
  stdin.resume()
  stdout.write(prompt)

  return new Promise((resolve, reject) => {
    let value = ""
    const finish = (error?: Error) => {
      stdin.off("keypress", onKeypress)
      stdin.setRawMode(false)
      stdin.pause()
      stdout.write("\n")
      if (error) reject(error)
      else resolve(value)
    }
    const onKeypress = (character: string, key: { name?: string; ctrl?: boolean }) => {
      if (key.ctrl && key.name === "c") {
        finish(new Error("Cancelled."))
      } else if (key.name === "return" || key.name === "enter") {
        finish()
      } else if (key.name === "backspace") {
        value = [...value].slice(0, -1).join("")
      } else if (character && !key.ctrl) {
        value += character
      }
    }
    stdin.on("keypress", onKeypress)
  })
}

try {
  const password = await readHidden("New administrator password (12+ characters): ")
  const passwordBytes = Buffer.byteLength(password, "utf8")
  if (passwordBytes < 12) {
    throw new Error("Use an administrator password with at least 12 bytes.")
  }
  if (passwordBytes > 1_024) {
    throw new Error("Use an administrator password of at most 1,024 bytes.")
  }
  const confirmation = await readHidden("Confirm administrator password: ")
  if (confirmation !== password) {
    throw new Error("The passwords did not match.")
  }
  const salt = randomBytes(32)
  const derived = scryptSync(password, salt, 32, {
    N: 32_768,
    r: 8,
    p: 1,
    maxmem: 64 * 1024 * 1024,
  })
  stdout.write(
    `scrypt$32768$8$1$${salt.toString("hex")}$${derived.toString("hex")}\n`
  )
} catch (error) {
  stdout.write(`${error instanceof Error ? error.message : "Could not create password hash."}\n`)
  process.exitCode = 1
}
