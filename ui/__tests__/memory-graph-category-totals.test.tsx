/**
 * Region counts for categories split off one kind by tag (ADR-035).
 *
 * The graph labelled each region with its kind's total, so two categories
 * sharing a kind both showed the sum. The summary now also carries
 * totalsByCategory ("kind|tag"), and a tagged category shows its own total.
 */
import { describe, it, expect, vi, beforeAll } from "vitest"
import { render, waitFor } from "@testing-library/react"
import type { MemoryItemSummary } from "../lib/api"

// Same identity on every call: the draw effect depends on the router.
const mockRouter = { push: vi.fn() }
vi.mock("next/navigation", () => ({ useRouter: () => mockRouter }))

const item = (id: string, tag?: string): MemoryItemSummary => ({
  id, kind: "policy", scope: "shared", title: id, tags: tag ? [tag] : [], status: "active",
  updatedAt: "2026-10-29T00:00:00Z", hitCount: 0,
})
const summary = {
  items: [item("a-1", "area:a"), item("a-2", "area:a"), item("b-1", "area:b"), item("c-1")],
  total: 75,
  totalsByKind: { policy: 75 },
  totalsByCategory: { "policy|area:a": 40, "policy|area:b": 30, policy: 5 },
}

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>()
  return {
    ...actual,
    fetchMemorySummary: () => Promise.resolve(summary),
    fetchMemorySearch: vi.fn().mockResolvedValue([]),
    subscribeMemoryEvents: vi.fn(() => () => {}),
  }
})

import { MemoryGraph } from "../components/memory-graph"
import { loadRuntimeCategories } from "../lib/category-config"

beforeAll(async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({
    categories: [
      { id: "a", kind: "policy", tag: "area:a", label: "A", tier: 0.6, angle: 0 },
      { id: "b", kind: "policy", tag: "area:b", label: "B", tier: 0.6, angle: 3.14 },
    ],
  }), { status: 200, headers: { "Content-Type": "application/json" } })))
  await loadRuntimeCategories()
  vi.unstubAllGlobals()
})

describe("MemoryGraph region counts", () => {
  it("labels a category split off its kind by tag with its own total", async () => {
    const { container } = render(<MemoryGraph />)
    await waitFor(() => {
      expect(container.querySelector('[data-label-category-id="a"]')?.textContent).toMatch(/\D40 items/)
    })
    expect(container.querySelector('[data-label-category-id="b"]')?.textContent).toMatch(/\D30 items/)
  })

  it("labels the untagged rest of a kind that has only tagged categories with its own total", async () => {
    // No bare category for kind policy: an untagged policy falls to the unknown
    // category (id = its kind), keyed by its kind on the server.
    const { container } = render(<MemoryGraph />)
    await waitFor(() => {
      expect(container.querySelector('[data-label-category-id="policy"]')?.textContent).toMatch(/\D5 items/)
    })
  })
})
