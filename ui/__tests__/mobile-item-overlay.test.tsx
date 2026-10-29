/**
 * Mobile item overlay transitions.
 *
 * The overlay kept its swipe-to-close drag in React state, so every touchmove
 * re-rendered the item, markdown included, and it vanished at once when a
 * swipe passed the threshold. It now slides in on open, follows the drag
 * through its style once per animation frame, and slides out before closing,
 * from the swipe or the back button.
 */
import { describe, it, expect, vi, beforeEach } from "vitest"
import { render, fireEvent, waitFor, screen } from "@testing-library/react"

const renders = { markdown: 0 }
const item = {
  id: "cell-1", kind: "stable_fact", scope: "shared", title: "Leave policy", content: "Some **text**.",
  tags: ["lang:en"], status: "active", updatedAt: "2026-10-29T00:00:00Z",
}
const loaded = { item, loading: false }
vi.mock("@/hooks/use-memory", () => ({ useMemoryItem: () => loaded }))
vi.mock("react-markdown", () => ({
  default: ({ children }: { children: string }) => { renders.markdown++; return <div>{children}</div> },
}))

import { MobileItemOverlay } from "../components/mobile-item-overlay"

const SLID_IN = "translateX(0px)"
const SLID_OUT = "translateX(100%)"

function setup() {
  const onClose = vi.fn()
  const view = render(<MobileItemOverlay id="cell-1" onClose={onClose} />)
  const root = view.container.firstElementChild as HTMLDivElement
  renders.markdown = 0
  return { root, onClose }
}

function drag(el: HTMLElement, dx: number) {
  fireEvent.touchStart(el, { touches: [{ clientX: 50, clientY: 400 }] })
  fireEvent.touchMove(el, { touches: [{ clientX: 50 + Math.sign(dx) * 20, clientY: 400 }] })
  fireEvent.touchMove(el, { touches: [{ clientX: 50 + dx, clientY: 400 }] })
}

beforeEach(() => { renders.markdown = 0 })

describe("MobileItemOverlay transitions", () => {
  it("slides in when it opens", () => {
    const { root } = setup()
    expect(root.style.transform).toBe(SLID_IN)
    expect(root.style.transition).not.toBe("none")
  })

  it("follows a drag without re-rendering the item, and snaps back after a short one", async () => {
    const { root, onClose } = setup()
    drag(root, 40)
    await waitFor(() => expect(root.style.transform).toBe("translateX(40px)"))
    expect(root.style.transition).toBe("none")
    expect(renders.markdown).toBe(0)
    fireEvent.touchEnd(root)
    expect(root.style.transform).toBe(SLID_IN)
    expect(onClose).not.toHaveBeenCalled()
  })

  it("slides out, then closes, after a swipe past the threshold", async () => {
    const { root, onClose } = setup()
    drag(root, 120)
    fireEvent.touchEnd(root)
    expect(root.style.transform).toBe(SLID_OUT)
    expect(onClose).not.toHaveBeenCalled()
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1))
  })

  it("slides out, then closes, from the back button", async () => {
    const { root, onClose } = setup()
    fireEvent.click(screen.getByRole("button"))
    expect(root.style.transform).toBe(SLID_OUT)
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1))
  })

  it("snaps back without closing when the gesture is cancelled", async () => {
    const { root, onClose } = setup()
    drag(root, 120)
    await waitFor(() => expect(root.style.transform).toBe("translateX(120px)"))
    vi.useFakeTimers()
    try {
      fireEvent.touchCancel(root)
      expect(root.style.transform).toBe(SLID_IN)
      vi.advanceTimersByTime(1000)
      expect(onClose).not.toHaveBeenCalled()
    } finally {
      vi.useRealTimers()
    }
  })

  it("keeps sliding out when touched again during the slide", async () => {
    const { root, onClose } = setup()
    drag(root, 120)
    fireEvent.touchEnd(root)
    expect(root.style.transform).toBe(SLID_OUT)
    drag(root, 20)
    fireEvent.touchEnd(root)
    expect(root.style.transform).toBe(SLID_OUT)
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1))
  })
})
