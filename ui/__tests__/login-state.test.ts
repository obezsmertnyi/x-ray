/**
 * Login hand-off binding.
 *
 * The hole this closes: /auth/callback used to store any `?token=` it was
 * given. A link to `/auth/callback?token=<attacker's session>` therefore
 * signed the victim into the ATTACKER's account, and every note, search and
 * voice transcript they produced afterwards landed in the attacker's store.
 * A valid token is what makes the attack work, so validating the token is no
 * defence — the token has to be bound to a login this browser actually began.
 */
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest"
import { beginLogin, consumeLogin, LOGIN_STATE_KEY } from "../lib/api"
import { sameOriginPath } from "../lib/utils"

beforeEach(() => {
  vi.restoreAllMocks()
  sessionStorage.clear()
})
afterEach(() => {
  vi.useRealTimers()
  sessionStorage.clear()
})

function storedState(): string {
  return JSON.parse(sessionStorage.getItem(LOGIN_STATE_KEY)!).state
}

describe("beginLogin", () => {
  it("marks the tab and carries the same state to the provider", () => {
    const url = new URL(beginLogin("github"))
    expect(url.pathname).toBe("/v1/auth/github")
    expect(url.searchParams.get("state")).toBe(storedState())
  })

  it("mints a fresh state each time", () => {
    beginLogin("github")
    const first = storedState()
    beginLogin("github")
    expect(storedState()).not.toBe(first)
  })

  it("does not throw when storage is blocked", () => {
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("storage disabled")
    })
    expect(() => beginLogin("github")).not.toThrow()
  })
})

describe("consumeLogin", () => {
  it("rejects a token when no login started in this tab — the fixation link", () => {
    expect(consumeLogin(null)).toBe(false)
    expect(consumeLogin("anything")).toBe(false)
  })

  it("accepts a login this tab started", () => {
    const state = new URL(beginLogin("github")).searchParams.get("state")
    expect(consumeLogin(state)).toBe(true)
  })

  it("accepts when the callback echoes no state at all", () => {
    // An API that does not round-trip `state` still gets the tab binding,
    // which is what actually blocks the one-click link.
    beginLogin("github")
    expect(consumeLogin(null)).toBe(true)
  })

  it("rejects a state that does not match the one this tab sent", () => {
    beginLogin("github")
    expect(consumeLogin("some-other-state")).toBe(false)
  })

  it("is single-use, so a replayed callback URL is refused", () => {
    const state = new URL(beginLogin("github")).searchParams.get("state")
    expect(consumeLogin(state)).toBe(true)
    expect(consumeLogin(state)).toBe(false)
  })

  it("clears the marker even when validation fails", () => {
    beginLogin("github")
    consumeLogin("wrong")
    expect(sessionStorage.getItem(LOGIN_STATE_KEY)).toBeNull()
  })

  it("expires a login left unfinished for more than ten minutes", () => {
    vi.useFakeTimers()
    const state = new URL(beginLogin("github")).searchParams.get("state")
    vi.advanceTimersByTime(10 * 60_000 + 1)
    expect(consumeLogin(state)).toBe(false)
  })

  it("rejects a corrupted marker rather than throwing", () => {
    sessionStorage.setItem(LOGIN_STATE_KEY, "not json")
    expect(consumeLogin(null)).toBe(false)
  })
})

describe("sameOriginPath", () => {
  const origin = "https://xray.example.com"

  it("keeps an ordinary in-app path", () => {
    expect(sameOriginPath("/k/concerns?q=a#b", origin)).toBe("/k/concerns?q=a#b")
  })

  it("defaults to / when there is no target", () => {
    expect(sameOriginPath(null, origin)).toBe("/")
    expect(sameOriginPath("", origin)).toBe("/")
  })

  // The whole point: every one of these passes a startsWith("/") check and
  // every one leaves the origin.
  it.each([
    "//evil.com",
    "//evil.com/path",
    "/\\evil.com",
    "https://evil.com/steal",
    "javascript:alert(1)",
  ])("refuses off-origin target %s", (target) => {
    expect(sameOriginPath(target, origin)).toBe("/")
  })

  it("keeps a same-origin absolute URL, reduced to its path", () => {
    expect(sameOriginPath(`${origin}/item?id=x`, origin)).toBe("/item?id=x")
  })
})
