import { describe, expect, it } from "vitest"
import {
  configureAppInputSchema,
  deployAppInputSchema,
  explainFailureInputSchema,
  getCapacityInputSchema,
  getAppInputSchema,
  isValidGitHubRepoURL,
  parseGitHubRepoURL,
  parseInput,
  submitOnboardingJobInputSchema,
} from "./validation"

describe("Harbor input validation", () => {
  it("rejects custom domain fields even with valid runtime changes", () => {
    expect(() =>
      parseInput(configureAppInputSchema, {
        name: "demo",
        memoryLimit: "128Mi",
        domains: ["example.com"],
      })
    ).toThrow("Invalid request data.")
  })

  it("accepts Go-compatible app names and repository defaults", () => {
    expect(
      parseInput(deployAppInputSchema, {
        name: "harbor-web",
        repoURL: "https://github.com/harbor/example.git",
      })
    ).toEqual({
      name: "harbor-web",
      repoURL: "https://github.com/harbor/example.git",
      ref: "",
      rootDir: "",
    })
  })

  it("accepts GitHub URLs with normalized host casing and explicit HTTPS port", () => {
    const repoURL = "HTTPS://GITHUB.COM:443/Harbor/Example.git"
    expect(
      parseInput(deployAppInputSchema, { name: "harbor-web", repoURL }).repoURL
    ).toBe(repoURL)
    expect(parseGitHubRepoURL(repoURL)).toEqual({
      owner: "Harbor",
      repo: "Example",
    })
  })

  it("uses the same GitHub repository-name rules in URL field feedback", () => {
    expect(isValidGitHubRepoURL("https://github.com/octo/.github")).toBe(true)
    expect(isValidGitHubRepoURL("https://github.com/octo/.." )).toBe(false)
  })

  it("rejects names that the Go daemon would reject", () => {
    expect(() => parseInput(getAppInputSchema, { name: "invalid-" })).toThrow(
      "Invalid request data."
    )
  })

  it("allows capacity requests for all apps or one validated app", () => {
    expect(parseInput(getCapacityInputSchema, {})).toEqual({})
    expect(parseInput(getCapacityInputSchema, { name: "harbor-web" })).toEqual({
      name: "harbor-web",
    })
    expect(() =>
      parseInput(getCapacityInputSchema, { name: "invalid-" })
    ).toThrow("Invalid request data.")
  })

  it("accepts a failure explanation request with a bounded deployment ID", () => {
    expect(
      parseInput(explainFailureInputSchema, {
        name: "harbor-web",
        deploymentID: "dep_123",
      })
    ).toEqual({ name: "harbor-web", deploymentID: "dep_123" })
    expect(() =>
      parseInput(explainFailureInputSchema, {
        name: "harbor-web",
        deploymentID: "../other-app",
      })
    ).toThrow("Invalid request data.")
    expect(() =>
      parseInput(explainFailureInputSchema, {
        name: "harbor-web",
        deploymentID: "dep_123",
        prompt: "unexpected input",
      })
    ).toThrow("Invalid request data.")
  })

  it("rejects repository URLs that could target another host", () => {
    expect(() =>
      parseInput(deployAppInputSchema, {
        name: "harbor-web",
        repoURL: "https://example.com/owner/repo.git",
        ref: "main",
        rootDir: "",
      })
    ).toThrow("Invalid request data.")
  })

  it("rejects ref traversal, invalid Git ref components, and root traversal", () => {
    for (const ref of ["../main", "feature/.lock", "feature/refs.LOCK"]) {
      expect(() =>
        parseInput(deployAppInputSchema, {
          name: "harbor-web",
          repoURL: "https://github.com/harbor/example",
          ref,
          rootDir: "",
        })
      ).toThrow("Invalid request data.")
    }

    expect(() =>
      parseInput(deployAppInputSchema, {
        name: "harbor-web",
        repoURL: "https://github.com/harbor/example",
        ref: "main",
        rootDir: "../outside",
      })
    ).toThrow("Invalid request data.")
  })

  it("accepts valid Unicode Git refs that the daemon supports", () => {
    expect(
      parseInput(deployAppInputSchema, {
        name: "harbor-web",
        repoURL: "https://github.com/harbor/example",
        ref: "修复/naïve",
      }).ref
    ).toBe("修复/naïve")
  })

  it("limits Git ref length in UTF-8 bytes like the daemon", () => {
    expect(
      parseInput(deployAppInputSchema, {
        name: "harbor-web",
        repoURL: "https://github.com/harbor/example",
        ref: "修".repeat(85),
      }).ref
    ).toBe("修".repeat(85))
    expect(() =>
      parseInput(deployAppInputSchema, {
        name: "harbor-web",
        repoURL: "https://github.com/harbor/example",
        ref: "修".repeat(86),
      })
    ).toThrow("Invalid request data.")
  })

  it("limits environment values in UTF-8 bytes like the daemon", () => {
    const value = "😀".repeat(20_000)
    expect(value.length).toBeLessThan(65_536)
    expect(new TextEncoder().encode(value).length).toBeGreaterThan(65_536)
    expect(
      configureAppInputSchema.safeParse({
        name: "harbor-web",
        environment: { APP_TEXT: value },
      }).success
    ).toBe(false)
    expect(
      submitOnboardingJobInputSchema.safeParse({
        name: "harbor-web",
        repoURL: "https://github.com/harbor/example",
        environment: { APP_TEXT: value },
      }).success
    ).toBe(false)
  })

  it("extracts only a validated GitHub repository path", () => {
    expect(parseGitHubRepoURL("https://github.com/Harbor/Example.git")).toEqual(
      {
        owner: "Harbor",
        repo: "Example",
      }
    )
  })

  it("accepts valid GitHub repository names beginning with punctuation", () => {
    for (const repo of [".github", "_repo", "-repo"]) {
      const repoURL = `https://github.com/harbor/${repo}`
      expect(
        parseInput(deployAppInputSchema, { name: "harbor-web", repoURL })
          .repoURL
      ).toBe(repoURL)
      expect(parseGitHubRepoURL(repoURL)).toEqual({ owner: "harbor", repo })
    }
    expect(() =>
      parseInput(deployAppInputSchema, {
        name: "harbor-web",
        repoURL: "https://github.com/harbor/..git",
      })
    ).toThrow("Invalid request data.")
  })

  it("accepts SQLite service selections and rejects obsolete PostgreSQL fields", () => {
    expect(
      parseInput(submitOnboardingJobInputSchema, {
        name: "harbor-web",
        repoURL: "https://github.com/harbor/example",
        sqliteService: "app-db",
        createSqlite: true,
      })
    ).toMatchObject({ sqliteService: "app-db", createSqlite: true })

    expect(() =>
      parseInput(submitOnboardingJobInputSchema, {
        name: "harbor-web",
        repoURL: "https://github.com/harbor/example",
        postgresService: "old-db",
      })
    ).toThrow("Invalid request data.")
  })
})
