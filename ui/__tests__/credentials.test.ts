import { describe, it, expect, vi, beforeEach, afterEach } from "vitest"
import { apiFetch, storedToken, SESSION_TOKEN_KEY } from "../lib/api"

beforeEach(() => {
  vi.restoreAllMocks()
  sessionStorage.clear()
})
afterEach(() => sessionStorage.clear())

function stubFetch() {
  const m = vi.fn().mockResolvedValue({ ok: true, json: async () => ({}) })
  vi.stubGlobal("fetch", m)
  return m
}

describe("storedToken", () => {
  it("returns null when nothing is stored — the ordinary same-origin cookie case", () => {
    expect(storedToken()).toBeNull()
  })

  it("returns the stored token", () => {
    sessionStorage.setItem(SESSION_TOKEN_KEY, "tok123")
    expect(storedToken()).toBe("tok123")
  })

  it("returns null rather than throwing when storage is blocked", () => {
    // Private mode and some embedded webviews throw on access rather than
    // returning null. A throw here would take down every API call, so the
    // failure has to degrade to 'no token'.
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("storage disabled")
    })
    expect(storedToken()).toBeNull()
  })
})

describe("apiFetch", () => {
  it("always sends credentials so the same-origin cookie is used", async () => {
    const f = stubFetch()
    await apiFetch("/v1/memory")
    expect(f.mock.calls[0][1].credentials).toBe("include")
  })

  it("sends no Authorization header when no token is stored", async () => {
    const f = stubFetch()
    await apiFetch("/v1/memory")
    expect(new Headers(f.mock.calls[0][1].headers).get("Authorization")).toBeNull()
  })

  it("attaches the stored token as a bearer header", async () => {
    sessionStorage.setItem(SESSION_TOKEN_KEY, "tok123")
    const f = stubFetch()
    await apiFetch("/v1/memory")
    expect(new Headers(f.mock.calls[0][1].headers).get("Authorization")).toBe("Bearer tok123")
  })

  it("keeps caller-supplied headers alongside the credential", async () => {
    sessionStorage.setItem(SESSION_TOKEN_KEY, "tok123")
    const f = stubFetch()
    await apiFetch("/v1/memory", { headers: { "Content-Type": "application/json" } })
    const h = new Headers(f.mock.calls[0][1].headers)
    expect(h.get("Content-Type")).toBe("application/json")
    expect(h.get("Authorization")).toBe("Bearer tok123")
  })

  it("passes through the method and body", async () => {
    const f = stubFetch()
    await apiFetch("/v1/memory", { method: "POST", body: "{}" })
    expect(f.mock.calls[0][1].method).toBe("POST")
    expect(f.mock.calls[0][1].body).toBe("{}")
  })

  // The token is read per call, not captured once at module load, so a token
  // stored by the /auth/callback hand-off applies to requests made afterwards
  // without a reload.
  it("reads the token at call time", async () => {
    const f = stubFetch()
    await apiFetch("/v1/memory")
    expect(new Headers(f.mock.calls[0][1].headers).get("Authorization")).toBeNull()

    sessionStorage.setItem(SESSION_TOKEN_KEY, "later")
    await apiFetch("/v1/memory")
    expect(new Headers(f.mock.calls[1][1].headers).get("Authorization")).toBe("Bearer later")
  })
})
