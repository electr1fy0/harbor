import { createServerFn } from "@tanstack/react-start"
import {
  deployAppInputSchema,
  getAppInputSchema,
  getGitHubRepoInputSchema,
  listGitHubReposInputSchema,
  parseGitHubRepoURL,
  parseInput,
  rollbackAppInputSchema,
  stopAppInputSchema,
} from "./validation"

export const getOverview = createServerFn({ method: "GET" }).handler(
  async () => {
    const { getOverviewData } = await import("./harbor.server")
    return getOverviewData()
  }
)

export const getApp = createServerFn({ method: "GET" })
  .validator((input: unknown) => parseInput(getAppInputSchema, input))
  .handler(async ({ data }) => {
    const { getAppData } = await import("./harbor.server")
    return getAppData(data.name)
  })

export const deployApp = createServerFn({ method: "POST" })
  .validator((input: unknown) => parseInput(deployAppInputSchema, input))
  .handler(async ({ data }) => {
    const { deployAppData } = await import("./harbor.server")
    return deployAppData(data)
  })

export const stopApp = createServerFn({ method: "POST" })
  .validator((input: unknown) => parseInput(stopAppInputSchema, input))
  .handler(async ({ data }) => {
    const { stopAppData } = await import("./harbor.server")
    return stopAppData(data.name)
  })

export const rollbackApp = createServerFn({ method: "POST" })
  .validator((input: unknown) => parseInput(rollbackAppInputSchema, input))
  .handler(async ({ data }) => {
    const { rollbackAppData } = await import("./harbor.server")
    return rollbackAppData(data.name, data.releaseID)
  })

export const listGitHubRepos = createServerFn({ method: "GET" })
  .validator((input: unknown) => parseInput(listGitHubReposInputSchema, input))
  .handler(async ({ data }) => {
    const { listGitHubReposData } = await import("./harbor.server")
    return listGitHubReposData(data.owner)
  })

export const getGitHubRepo = createServerFn({ method: "GET" })
  .validator((input: unknown) => parseInput(getGitHubRepoInputSchema, input))
  .handler(async ({ data }) => {
    const { getGitHubRepoData } = await import("./harbor.server")
    const { owner, repo } = parseGitHubRepoURL(data.repoURL)
    return getGitHubRepoData(owner, repo)
  })
