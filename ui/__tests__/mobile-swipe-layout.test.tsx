/**
 * Mobile swipe regression tests.
 *
 * The drag offset used to live in React state: every touchmove re-rendered
 * every tab panel (the Home graph and every list), so swipes stuttered on
 * phones. The drag is now written to the panels' style once per animation
 * frame, and the panels are memoised so layout state they do not read (voice
 * transcript, quota, the user menu) does not re-render them either.
 *
 * Render counts are taken from the hooks and components each panel calls on
 * every render: useMemoryList (the Concerns, Events and Facts lists) and
 * MemoryGraph (Home).
 */
import { describe, it, expect, vi, beforeEach } from "vitest"
import { render, fireEvent, waitFor, act, screen } from "@testing-library/react"

const renders = { lists: 0, graph: 0 }

const emptyList = { items: [], loading: false, error: null }
const noHits = { hits: null }
const noResults = { results: [], loading: false }
vi.mock("@/hooks/use-memory", () => ({
  useMemoryList: () => { renders.lists++; return emptyList },
  useMemoryHits: () => noHits,
  useMetrics: () => null,
  useMemorySearch: () => noResults,
}))
vi.mock("@/lib/quota-store", () => ({ useLiveQuota: () => null }))
const idleVoice = { status: "idle", bubbles: [] }
vi.mock("@/hooks/use-voice-session", () => ({ useVoiceSession: () => idleVoice }))
const auth = { username: "alice", userID: "u-alice" }
vi.mock("@/hooks/use-auth", () => ({ useAuth: () => auth }))
vi.mock("@/components/memory-graph", () => ({
  MemoryGraph: () => { renders.graph++; return null },
}))
vi.mock("@/components/mobile-item-overlay", () => ({ MobileItemOverlay: () => null }))
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>()
  return {
    ...actual,
    fetchUserNotes: vi.fn().mockResolvedValue([]),
    subscribeMemoryEvents: vi.fn(() => () => {}),
  }
})

import { MobileSwipeLayout } from "../components/mobile-swipe-layout"

// Default mobile tabs: concerns, events, [home], facts, notes.
const HOME = 2

async function setup() {
  const view = render(<MobileSwipeLayout />)
  await act(async () => {})  // let the notes fetches settle
  const panels = Array.from(view.container.querySelectorAll<HTMLDivElement>("div.absolute.inset-0"))
  const area = panels[0].parentElement as HTMLElement
  renders.lists = 0
  renders.graph = 0
  return { panels, area }
}

function drag(area: HTMLElement, dx: number) {
  fireEvent.touchStart(area, { touches: [{ clientX: 200, clientY: 300 }] })
  fireEvent.touchMove(area, { touches: [{ clientX: 200 + Math.sign(dx) * 20, clientY: 300 }] })
  fireEvent.touchMove(area, { touches: [{ clientX: 200 + dx, clientY: 300 }] })
}

const at = (idx: number, center: number, delta = 0) =>
  `translateX(calc(${(idx - center) * 100}% + ${delta}px))`

beforeEach(() => {
  renders.lists = 0
  renders.graph = 0
})

describe("MobileSwipeLayout swipe", () => {
  it("moves the panels during a drag without re-rendering them", async () => {
    const { panels, area } = await setup()
    drag(area, -60)
    await waitFor(() => expect(panels[HOME].style.transform).toBe(at(HOME, HOME, -60)))
    expect(panels[HOME + 1].style.transform).toBe(at(HOME + 1, HOME, -60))
    expect(panels[HOME].style.transition).toBe("none")
    expect(panels[HOME].style.willChange).toBe("transform")
    expect(renders).toEqual({ lists: 0, graph: 0 })
  })

  it("snaps back to the current tab after a short drag", async () => {
    const { panels, area } = await setup()
    drag(area, -30)
    await waitFor(() => expect(panels[HOME].style.transform).toBe(at(HOME, HOME, -30)))
    fireEvent.touchEnd(area)
    panels.forEach((el, idx) => expect(el.style.transform).toBe(at(idx, HOME)))
    expect(panels[HOME].style.transition).not.toBe("none")
    expect(screen.getByPlaceholderText("Search all memories…")).toBeInTheDocument()
  })

  it("switches to the next tab after a swipe past the threshold", async () => {
    const { panels, area } = await setup()
    drag(area, -100)
    fireEvent.touchEnd(area)
    panels.forEach((el, idx) => expect(el.style.transform).toBe(at(idx, HOME + 1)))
    expect(screen.getByPlaceholderText("Search Facts…")).toBeInTheDocument()
  })

  it("stays on the first tab when swiping past it", async () => {
    const { panels, area } = await setup()
    for (let i = 0; i < HOME; i++) {
      drag(area, 100)
      fireEvent.touchEnd(area)
    }
    expect(screen.getByPlaceholderText("Search Concerns…")).toBeInTheDocument()
    drag(area, 100)
    await waitFor(() => expect(panels[0].style.transform).toBe(at(0, 0, 100)))
    fireEvent.touchEnd(area)
    panels.forEach((el, idx) => expect(el.style.transform).toBe(at(idx, 0)))
    expect(screen.getByPlaceholderText("Search Concerns…")).toBeInTheDocument()
  })

  it("snaps back without switching when the gesture is cancelled", async () => {
    const { panels, area } = await setup()
    drag(area, -100)
    await waitFor(() => expect(panels[HOME].style.transform).toBe(at(HOME, HOME, -100)))
    fireEvent.touchCancel(area)
    panels.forEach((el, idx) => expect(el.style.transform).toBe(at(idx, HOME)))
    expect(screen.getByPlaceholderText("Search all memories…")).toBeInTheDocument()
  })

  it("does not re-render the panels for layout state they do not read", async () => {
    await setup()
    fireEvent.click(screen.getByRole("button", { name: "a" }))
    expect(screen.getByText("Sign out")).toBeInTheDocument()
    expect(renders).toEqual({ lists: 0, graph: 0 })
  })
})
