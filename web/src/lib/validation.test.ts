import { describe, expect, it } from "vitest"
import {
  deployAppInputSchema,
  explainFailureInputSchema,
  getCapacityInputSchema,
  getAppInputSchema,
  parseGitHubRepoURL,
  parseInput,
  submitOnboardingJobInputSchema,
} from "./validation"

describe("Harbor input validation", () => {
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

  it("extracts only a validated GitHub repository path", () => {
    expect(parseGitHubRepoURL("https://github.com/Harbor/Example.git")).toEqual(
      {
        owner: "Harbor",
        repo: "Example",
      }
    )
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
