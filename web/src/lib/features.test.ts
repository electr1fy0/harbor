import { describe, expect, it } from "vitest"
import { parseDotEnv } from "./features"

describe("dotenv import", () => {
  it("parses comments, export syntax, empty and quoted values without interpolation", () => {
    expect(
      parseDotEnv(
        'A=\nexport B="one\\ntwo" # comment\nC=literal#hash\nD= $HOME # comment'
      )
    ).toEqual({ A: "", B: "one\ntwo", C: "literal#hash", D: "$HOME" })
  })
  it("rejects duplicate keys, invalid lines and malformed quotes", () => {
    expect(() => parseDotEnv("A=1\nA=2")).toThrow("duplicate")
    expect(() => parseDotEnv("bad-key=value")).toThrow("KEY=value")
    expect(() => parseDotEnv('A="oops')).toThrow("unmatched quote")
  })
  it("accepts prototype-like keys as plain data", () => {
    expect(parseDotEnv("__proto__=safe")["__proto__"]).toBe("safe")
  })
})
