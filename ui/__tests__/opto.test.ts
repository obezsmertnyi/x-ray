/**
 * Optogenetic recall effect (lib/opto.ts).
 *
 * A recall lights the recalled region with a blue fibre, fires the recalled
 * cells, and sends a pulse along their synapses. These tests build a small
 * graph by hand, with the same attributes memory-graph.tsx gives its nodes, and
 * check what the effect adds and that it cleans up after itself.
 */
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest"
import { fireOptogenetic, GRAPH_LAYER_CLASS, OPTO_LAYER_CLASS } from "../lib/opto"

const NS = "http://www.w3.org/2000/svg"

function el<K extends keyof SVGElementTagNameMap>(tag: K, attrs: Record<string, string>, parent: Element): SVGElementTagNameMap[K] {
  const e = document.createElementNS(NS, tag)
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v)
  parent.appendChild(e)
  return e
}

// Two events and a fact. event-1 and event-2 share a synapse; the fact is in
// another region.
function buildGraph() {
  const svg = document.createElementNS(NS, "svg")
  svg.setAttribute("viewBox", "0 0 800 600")
  document.body.appendChild(svg)
  el("defs", {}, svg)
  const root = el("g", {}, svg)
  const g = el("g", { class: GRAPH_LAYER_CLASS }, root)
  const nodes = [
    { id: "event-1", cat: "event", x: 520, y: 200 },
    { id: "event-2", cat: "event", x: 560, y: 260 },
    { id: "fact-1", cat: "fact", x: 200, y: 400 },
  ]
  for (const n of nodes) {
    el("path", { d: `M ${n.x - 20} ${n.y - 20} L ${n.x + 20} ${n.y - 20} L ${n.x} ${n.y + 20} Z`, "data-id": n.id, "data-category-id": n.cat }, g)
  }
  const syn = el("g", {}, g)
  el("line", { x1: "520", y1: "200", x2: "560", y2: "260" }, syn)
  for (const n of nodes) {
    el("circle", { cx: String(n.x), cy: String(n.y), r: "5", "data-dot-id": n.id, "data-dot-category-id": n.cat }, g)
  }
  return svg
}

function stubMotion(reduce: boolean) {
  window.matchMedia = ((q: string) => ({ matches: reduce && q.includes("reduce"), media: q, addEventListener() {}, removeEventListener() {} })) as unknown as typeof window.matchMedia
}

beforeEach(() => { vi.useFakeTimers(); stubMotion(false) })
afterEach(() => { vi.useRealTimers(); document.body.innerHTML = "" })

describe("fireOptogenetic", () => {
  it("lights the region with a beam, a fibre tip and a wash on every cell in it", () => {
    const svg = buildGraph()
    fireOptogenetic(svg, { categoryIds: ["event"], itemIds: [] })
    const layer = svg.querySelector(`.${OPTO_LAYER_CLASS}`)!
    expect(layer).not.toBeNull()
    expect(layer.querySelectorAll(".opto-beam")).toHaveLength(1)
    expect(layer.querySelectorAll(".opto-tip")).toHaveLength(1)
    expect(layer.querySelectorAll(".opto-wash")).toHaveLength(2) // event-1, event-2; not the fact
    expect(layer.querySelector('[data-opto-region="fact"]')).toBeNull()
  })

  it("fires the exact recalled cells harder, with rings and an afterglow", () => {
    const svg = buildGraph()
    fireOptogenetic(svg, { categoryIds: ["event"], itemIds: ["event-1"] })
    const layer = svg.querySelector(`.${OPTO_LAYER_CLASS}`)!
    expect(layer.querySelectorAll(".opto-fire")).toHaveLength(1)
    expect(layer.querySelectorAll(".opto-wash")).toHaveLength(1) // the other event cell
    expect(layer.querySelectorAll(".opto-halo")).toHaveLength(1)
    expect(layer.querySelectorAll(".opto-ring")).toHaveLength(3)
  })

  it("sends a pulse along the synapse of a recalled cell, and only those", () => {
    const svg = buildGraph()
    fireOptogenetic(svg, { categoryIds: [], itemIds: ["event-1"] })
    const pulse = svg.querySelectorAll(".opto-synapse")
    expect(pulse).toHaveLength(1)
    // It runs from the recalled cell outward.
    expect(pulse[0].getAttribute("x1")).toBe("520")
    expect(pulse[0].getAttribute("x2")).toBe("560")

    const quiet = buildGraph()
    fireOptogenetic(quiet, { categoryIds: [], itemIds: ["fact-1"] })
    expect(quiet.querySelectorAll(".opto-synapse")).toHaveLength(0)
  })

  it("finds the region from a recalled cell when the server sent no kinds", () => {
    const svg = buildGraph()
    fireOptogenetic(svg, { categoryIds: [], itemIds: ["fact-1"] })
    expect(svg.querySelector('[data-opto-region="fact"] .opto-beam')).not.toBeNull()
  })

  it("lights cells in the order the light reaches them", () => {
    const svg = buildGraph()
    fireOptogenetic(svg, { categoryIds: ["event"], itemIds: [] })
    const delays = Array.from(svg.querySelectorAll<SVGPathElement>(".opto-wash")).map(p => parseInt(p.style.animationDelay, 10))
    expect(delays).toHaveLength(2)
    expect(Math.min(...delays)).toBe(0)
    expect(Math.max(...delays)).toBeGreaterThan(0)
  })

  it("is quiet about ids the graph does not draw", () => {
    const svg = buildGraph()
    fireOptogenetic(svg, { categoryIds: ["missing"], itemIds: ["nope"] })
    const layer = svg.querySelector(`.${OPTO_LAYER_CLASS}`)
    expect(layer?.children.length ?? 0).toBe(0)
  })

  it("removes everything it added once the animations are over", () => {
    const svg = buildGraph()
    fireOptogenetic(svg, { categoryIds: ["event"], itemIds: ["event-1"] })
    expect(svg.querySelector(`.${OPTO_LAYER_CLASS}`)!.children.length).toBeGreaterThan(0)
    vi.advanceTimersByTime(8000)
    expect(svg.querySelector(`.${OPTO_LAYER_CLASS}`)!.children.length).toBe(0)
    expect(svg.querySelectorAll("defs linearGradient")).toHaveLength(0)
  })

  it("does not touch the graph's own cells and dots", () => {
    const svg = buildGraph()
    const before = svg.querySelector('[data-id="event-1"]')!.outerHTML
    fireOptogenetic(svg, { categoryIds: ["event"], itemIds: ["event-1"] })
    expect(svg.querySelector('[data-id="event-1"]')!.outerHTML).toBe(before)
  })

  it("under reduced motion is one soft wash: no rings, no synapse pulse", () => {
    stubMotion(true)
    const svg = buildGraph()
    fireOptogenetic(svg, { categoryIds: ["event"], itemIds: ["event-1"] })
    expect(svg.querySelectorAll(".opto-ring")).toHaveLength(0)
    expect(svg.querySelectorAll(".opto-synapse")).toHaveLength(0)
    for (const e of Array.from(svg.querySelectorAll(".opto-beam, .opto-wash, .opto-fire, .opto-halo"))) {
      expect(e.classList.contains("opto-still")).toBe(true)
    }
  })

  it("does nothing on a graph that has not been drawn", () => {
    const svg = document.createElementNS(NS, "svg")
    document.body.appendChild(svg)
    expect(() => fireOptogenetic(svg, { categoryIds: ["event"], itemIds: ["event-1"] })).not.toThrow()
  })
})
