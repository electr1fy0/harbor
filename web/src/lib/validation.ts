import { z } from "zod"

const appName = z
  .string()
  .regex(/^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$/, "Invalid request data.")

const environmentKey = z
  .string()
  .regex(/^[A-Za-z_][A-Za-z0-9_]{0,127}$/, "Invalid request data.")

const domainName = z
  .string()
  .trim()
  .toLowerCase()
  .max(253)
  .regex(
    /^(?=.{1,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/,
    "Invalid request data."
  )

const githubOwner = z
  .string()
  .trim()
  .regex(
    /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$/,
    "Invalid request data."
  )

const githubRepoUrl = z
  .string()
  .trim()
  .regex(
    /^https:\/\/github\.com\/[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?\/[A-Za-z0-9][A-Za-z0-9._-]{0,99}(?:\.git)?$/,
    "Invalid request data."
  )

const gitRef = z
  .string()
  .max(255)
  .refine((value) => {
    if (value === "") return true
    if (!/^[A-Za-z0-9][A-Za-z0-9._/-]*$/.test(value)) return false
    const parts = value.split("/")
    return (
      !value.includes("..") &&
      !value.includes("//") &&
      !value.endsWith("/") &&
      !value.endsWith(".") &&
      parts.every(
        (part) =>
          part.length > 0 &&
          !part.startsWith(".") &&
          !part.toLowerCase().endsWith(".lock")
      )
    )
  }, "Invalid request data.")

const repositoryRoot = z
  .string()
  .max(256)
  .refine((value) => {
    if (value === "" || value === ".") return true
    if (
      value.startsWith("/") ||
      value.includes("\\") ||
      /^[A-Za-z]:/.test(value) ||
      // eslint-disable-next-line no-control-regex
      /[\u0000-\u001f\u007f]/.test(value)
    ) {
      return false
    }

    return value
      .split("/")
      .every((part) => part.length > 0 && part !== "." && part !== "..")
  }, "Invalid request data.")

export const getAppInputSchema = z.object({ name: appName }).strict()
export const getCapacityInputSchema = z
  .object({ name: appName.optional() })
  .strict()
export const getDeploymentJobInputSchema = z
  .object({ id: z.string().regex(/^[A-Za-z0-9_-]{1,128}$/) })
  .strict()
export const stopAppInputSchema = z.object({ name: appName }).strict()
export const rollbackAppInputSchema = z
  .object({
    name: appName,
    releaseID: z.string().regex(/^(?:[A-Za-z0-9_-]{1,128})?$/),
  })
  .strict()
export const deployAppInputSchema = z
  .object({
    name: appName,
    repoURL: githubRepoUrl,
    ref: gitRef.optional().default(""),
    rootDir: repositoryRoot.optional().default(""),
  })
  .strict()
export const listGitHubReposInputSchema = z
  .object({
    owner: githubOwner.optional().default(""),
  })
  .strict()
export const getGitHubRepoInputSchema = z
  .object({ repoURL: githubRepoUrl })
  .strict()
const onboardingConfigSchema = z
  .object({
    runtime: z.enum(["go", "node"]).optional(),
    buildCommand: z.string().max(4096).optional(),
    startCommand: z.string().max(4096).optional(),
    healthPath: z.string().max(256).optional(),
    memoryLimit: z.string().max(32).optional(),
    cpuQuota: z.number().finite().positive().max(256).optional(),
    tasksLimit: z.number().int().positive().max(65_536).optional(),
  })
  .strict()
export const inspectProjectInputSchema = z
  .object({
    name: appName.optional(),
    repoURL: githubRepoUrl,
    ref: gitRef.optional().default(""),
    rootDir: repositoryRoot.optional().default(""),
    config: onboardingConfigSchema.optional(),
  })
  .strict()
export const submitOnboardingJobInputSchema = z
  .object({
    name: appName,
    repoURL: githubRepoUrl,
    ref: gitRef.optional().default(""),
    rootDir: repositoryRoot.optional().default(""),
    config: onboardingConfigSchema.optional(),
    environment: z.record(environmentKey, z.string().max(65_536)).optional(),
    postgresService: appName.optional(),
    createPostgres: z.boolean().optional(),
  })
  .strict()
export const onboardingJobIDSchema = z
  .object({ id: z.string().regex(/^job_[a-f0-9]{32}$/) })
  .strict()
export const connectGitHubInputSchema = z
  .object({ token: z.string().min(10).max(4096) })
  .strict()
export const getAppSettingsInputSchema = z.object({ name: appName }).strict()
export const configureAppInputSchema = z
  .object({
    name: appName,
    environment: z.record(environmentKey, z.string().max(65_536)).optional(),
    removeEnvironment: z.array(environmentKey).max(100).optional(),
    domains: z.array(domainName).max(50).optional(),
    memoryLimit: z
      .string()
      .trim()
      .regex(/^[1-9][0-9]*(?:Ki|Mi|Gi|Ti|Pi|Ei|K|M|G|T|P|E|B)?$/)
      .optional(),
    cpuQuota: z.number().finite().positive().max(256).optional(),
    tasksLimit: z.number().int().positive().max(65_536).optional(),
  })
  .strict()
  .refine(
    (input) =>
      input.environment !== undefined ||
      input.removeEnvironment !== undefined ||
      input.domains !== undefined ||
      input.memoryLimit !== undefined ||
      input.cpuQuota !== undefined ||
      input.tasksLimit !== undefined,
    "Invalid request data."
  )
  .refine(
    (input) =>
      !input.domains || new Set(input.domains).size === input.domains.length,
    "Invalid request data."
  )

export type GetAppInput = z.infer<typeof getAppInputSchema>
export type GetCapacityInput = z.infer<typeof getCapacityInputSchema>
export type GetDeploymentJobInput = z.infer<typeof getDeploymentJobInputSchema>
export type DeployAppInput = z.infer<typeof deployAppInputSchema>
export type StopAppInput = z.infer<typeof stopAppInputSchema>
export type RollbackAppInput = z.infer<typeof rollbackAppInputSchema>
export type ListGitHubReposInput = z.infer<typeof listGitHubReposInputSchema>
export type GetGitHubRepoInput = z.infer<typeof getGitHubRepoInputSchema>
export type InspectProjectInput = z.infer<typeof inspectProjectInputSchema>
export type SubmitOnboardingJobInput = z.infer<
  typeof submitOnboardingJobInputSchema
>
export type OnboardingJobIDInput = z.infer<typeof onboardingJobIDSchema>
export type ConnectGitHubInput = z.infer<typeof connectGitHubInputSchema>
export type GetAppSettingsInput = z.infer<typeof getAppSettingsInputSchema>
export type ConfigureAppInput = z.infer<typeof configureAppInputSchema>

/**
 * Normalize validation failures so Zod's diagnostic object never crosses the
 * server-function boundary.
 */
export function parseInput<TSchema extends z.ZodType>(
  schema: TSchema,
  input: unknown
): z.infer<TSchema> {
  const result = schema.safeParse(input)
  if (!result.success) throw new Error("Invalid request data.")
  return result.data
}

export function parseGitHubRepoURL(repoURL: string): {
  owner: string
  repo: string
} {
  const parsed = parseInput(githubRepoUrl, repoURL)
  const [, owner, rawRepo] =
    parsed.match(/^https:\/\/github\.com\/([^/]+)\/([^/]+)$/) ?? []
  if (!owner || !rawRepo) throw new Error("Invalid request data.")

  return {
    owner,
    repo: rawRepo.endsWith(".git") ? rawRepo.slice(0, -4) : rawRepo,
  }
}
