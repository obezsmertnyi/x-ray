import { describe, it, expect, vi, beforeEach } from "vitest"
import { fetchMemoryItem, fetchUserNotes } from "../lib/api"

const note = {
  id: "abc123", kind: "note", scope: "private",
  title: "My Note", content: "hello", subject: "3c98b7",
  status: "active", updatedAt: "2026-05-26T00:00:00Z", createdAt: "2026-05-26T00:00:00Z",
}
const publicNote = {
  id: "pub789", kind: "note", scope: "public",
  title: "Knowledge: a theme", content: "synthesis", subject: "xray",
  status: "active", updatedAt: "2026-05-26T00:00:00Z", createdAt: "2026-05-26T00:00:00Z",
}
const sharedItem = {
  id: "def456", kind: "stable_fact", scope: "public",
  title: "A Fact", content: "facts", subject: "",
  status: "active", updatedAt: "2026-05-26T00:00:00Z", createdAt: "2026-05-26T00:00:00Z",
}

beforeEach(() => { vi.restoreAllMocks() })

describe("fetchMemoryItem", () => {
  it("serves a public note from /v1/memory/item?id=... on the first try (no private fallback)", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => publicNote })
    vi.stubGlobal("fetch", fetchMock)

    const result = await fetchMemoryItem("pub789", "note")

    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock.mock.calls[0][0]).toContain("/v1/memory/item?")
    expect(fetchMock.mock.calls[0][0]).toContain("id=pub789")
    expect(result.id).toBe("pub789")
  })

  it("falls back to /v1/user/notes/{id} for a private note (404 on the shared path)", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce({ ok: false, status: 404 })
      .mockResolvedValueOnce({ ok: true, json: async () => note })
    vi.stubGlobal("fetch", fetchMock)

    const result = await fetchMemoryItem("abc123", "note")

    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(fetchMock.mock.calls[0][0]).toContain("/v1/memory/item?")
    expect(fetchMock.mock.calls[0][0]).toContain("id=abc123")
    expect(fetchMock.mock.calls[1][0]).toContain("/v1/user/notes/abc123")
    expect(result.id).toBe("abc123")
  })

  it("calls /v1/memory/item?id=... for shared kinds", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => sharedItem })
    vi.stubGlobal("fetch", fetchMock)

    await fetchMemoryItem("def456", "stable_fact")

    expect(fetchMock.mock.calls[0][0]).toContain("/v1/memory/item?")
    expect(fetchMock.mock.calls[0][0]).toContain("id=def456")
  })

  it("falls back to /v1/user/notes/{id} when /v1/memory/item?id=... returns 404 and no kind given", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce({ ok: false, status: 404 })
      .mockResolvedValueOnce({ ok: true, json: async () => note })
    vi.stubGlobal("fetch", fetchMock)

    const result = await fetchMemoryItem("abc123")

    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(fetchMock.mock.calls[0][0]).toContain("/v1/memory/item?")
    expect(fetchMock.mock.calls[0][0]).toContain("id=abc123")
    expect(fetchMock.mock.calls[1][0]).toContain("/v1/user/notes/abc123")
    expect(result.id).toBe("abc123")
  })

  it("throws when both endpoints fail", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce({ ok: false, status: 404 })
      .mockResolvedValueOnce({ ok: false, status: 404 })
    vi.stubGlobal("fetch", fetchMock)

    await expect(fetchMemoryItem("abc123")).rejects.toThrow("HTTP 404")
  })

  // The lookup is a query parameter, not a {id} path segment, so an id
  // containing '/' — e.g. a claim/incident cell id that embeds a source path
  // like `inc-claim|gcp|events/gcp-status/compute-engine` — round-trips
  // correctly instead of splitting into extra path segments and 404ing.
  // URLSearchParams handles '|' and spaces/non-ASCII the same way.
  it("encodes an id containing '/', '|', spaces, and non-ASCII as a single query value", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => publicNote })
    vi.stubGlobal("fetch", fetchMock)

    const id = "inc-claim|gcp|events/gcp-status/compute-engine öst"
    await fetchMemoryItem(id)

    const url = fetchMock.mock.calls[0][0] as string
    expect(url).toContain("/v1/memory/item?")
    // Decode the query string back and confirm the id round-trips exactly —
    // asserting on decoded content rather than a literal escaped substring,
    // since the exact percent-encoding of non-ASCII bytes is an
    // implementation detail of URLSearchParams.
    const params = new URLSearchParams(url.split("?")[1])
    expect(params.get("id")).toBe(id)
  })
})

describe("fetchUserNotes", () => {
  it("returns items from /v1/user/notes", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ items: [note] }),
    })
    vi.stubGlobal("fetch", fetchMock)

    const items = await fetchUserNotes()

    expect(fetchMock.mock.calls[0][0]).toContain("/v1/user/notes")
    expect(items).toHaveLength(1)
    expect(items[0].id).toBe("abc123")
  })

  it("returns empty array on error", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 401 }))
    const items = await fetchUserNotes()
    expect(items).toEqual([])
  })
})
