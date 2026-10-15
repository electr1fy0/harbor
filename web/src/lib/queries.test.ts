import { describe, expect, it } from "vitest"
import { deploymentJobRefetchInterval, deploymentJobStatus } from "./queries"

describe("deployment job polling", () => {
  it("keeps polling after transient errors with a slower interval", () => {
    expect(
      deploymentJobRefetchInterval({ data: { state: "BUILDING" }, error: {} })
    ).toBe(5_000)
    expect(
      deploymentJobRefetchInterval({ data: { state: "BUILDING" }, error: null })
    ).toBe(1_500)
  })

  it("stops polling once a terminal job result is available", () => {
    expect(
      deploymentJobRefetchInterval({
        data: { state: "FAILED" },
        error: null,
      })
    ).toBe(false)
  })

  it("keeps cached status visible while a refresh is failing", () => {
    expect(
      deploymentJobStatus({ data: { state: "BUILDING" }, error: new Error() })
    ).toBe("BUILDING")
    expect(deploymentJobStatus({ error: new Error() })).toBe(
      "Status unavailable"
    )
  })
})
