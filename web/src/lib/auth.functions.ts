import { z } from "zod"
import {
  getAuthStatus as fetchAuthStatus,
  login as submitLogin,
  logout as submitLogout,
} from "./api.client"
import { parseInput } from "./validation"

const loginInputSchema = z
  .object({ password: z.string().min(1).max(1_024) })
  .strict()

export function getAuthStatus() {
  return fetchAuthStatus()
}

export function login(input: { data: { password: string } }) {
  const data = parseInput(loginInputSchema, input.data)
  return submitLogin(data.password)
}

export function logout() {
  return submitLogout()
}
