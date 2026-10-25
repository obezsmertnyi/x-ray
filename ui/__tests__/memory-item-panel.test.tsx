/**
 * Integration test — stubs global.fetch, NOT fetchMemoryItem.
 * A `note` is resolved scope-agnostically: the panel tries the shared
 * /v1/memory/item?id=... endpoint first (serves public notes) and falls back
 * to the ownership-checked /v1/user/notes/{id} on 404 (private notes). Both
 * scopes must therefore render.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest"
import { render, screen, fireEvent, waitFor } from "@testing-library/react"
import { MemoryItemPanel } from "../components/memory-item-panel"

const PRIVATE_NOTE = {
  id: "note1", kind: "note", scope: "private",
  title: "My Meeting Note", content: "meeting content",
  subject: "3c98b7", status: "active",
  updatedAt: "2026-05-26T00:00:00Z", createdAt: "2026-05-26T00:00:00Z",
}

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>()
  return { ...actual, fetchMemory: vi.fn().mockResolvedValue([]), subscribeMemoryEvents: vi.fn(() => () => {}) }
})

let fetchMock: ReturnType<typeof vi.fn>

beforeEach(() => {
  fetchMock = vi.fn((url: string) => {
    // Private note: 404 on the shared endpoint, served by the personal one.
    if (typeof url === "string" && url.includes("/v1/memory/item?") && url.includes("id=note1")) {
      return Promise.resolve({ ok: false, status: 404 })
    }
    if (typeof url === "string" && url.includes("/v1/user/notes/note1")) {
      return Promise.resolve({ ok: true, json: async () => PRIVATE_NOTE })
    }
    if (typeof url === "string" && url.includes("/v1/user/notes")) {
      return Promise.resolve({ ok: true, json: async () => ({ items: [PRIVATE_NOTE] }) })
    }
    return Promise.resolve({ ok: true, json: async () => ({}) })
  })
  vi.stubGlobal("fetch", fetchMock)
})

afterEach(() => { vi.unstubAllGlobals() })

const baseProps = { id: "note1", kind: "note" as const, initialX: 0, initialY: 0, onClose: vi.fn() }

describe("MemoryItemPanel — note resolves via shared-first, private fallback", () => {
  it("renders a private note via the /v1/user/notes/{id} fallback after the shared path 404s", async () => {
    render(<MemoryItemPanel {...baseProps} />)
    await waitFor(() => screen.getByText("My Meeting Note"))
    // It tried the shared endpoint first, then fell back to the personal one.
    const sharedTry = fetchMock.mock.calls.filter((c: unknown[]) => String(c[0]).includes("/v1/memory/item?") && String(c[0]).includes("id=note1"))
    const personalTry = fetchMock.mock.calls.filter((c: unknown[]) => String(c[0]).includes("/v1/user/notes/note1"))
    expect(sharedTry.length).toBeGreaterThan(0)
    expect(personalTry.length).toBeGreaterThan(0)
  })

  it("renders a public note directly from /v1/memory/item?id=... with no personal fallback", async () => {
    const PUBLIC_NOTE = {
      id: "pub1", kind: "note", scope: "public",
      title: "Knowledge: a theme", content: "synthesis",
      subject: "xray", status: "active",
      updatedAt: "2026-05-26T00:00:00Z", createdAt: "2026-05-26T00:00:00Z",
    }
    fetchMock = vi.fn((url: string) => {
      if (typeof url === "string" && url.includes("/v1/memory/item?") && url.includes("id=pub1")) {
        return Promise.resolve({ ok: true, json: async () => PUBLIC_NOTE })
      }
      return Promise.resolve({ ok: true, json: async () => ({}) })
    })
    vi.stubGlobal("fetch", fetchMock)

    render(<MemoryItemPanel {...baseProps} id="pub1" />)
    await waitFor(() => screen.getByText("Knowledge: a theme"))
    const personalTry = fetchMock.mock.calls.filter((c: unknown[]) => String(c[0]).includes("/v1/user/notes/pub1"))
    expect(personalTry).toHaveLength(0)
  })
})
