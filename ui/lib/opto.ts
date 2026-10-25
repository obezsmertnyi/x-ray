// Optogenetic recall effect for the memory graph.
//
// In optogenetics a neuron that has been made to express a light-gated channel
// (channelrhodopsin) fires when blue light from an optical fibre reaches it. A
// memory recall is drawn the same way: a blue fibre lights the region the
// system recalled from, the cells nearest the fibre fire first because tissue
// scatters the light, the exact recalled cells (the engram) fire hardest with
// spike rings and an afterglow, and the signal travels along the synapses to the
// memories they connect to.
//
// It is layered on top of the category-coloured flash memory-graph.tsx already
// applies, never in place of it: everything here is a transient overlay in its
// own group, removed when its animation ends, so it cannot leave the graph in a
// different state than it found it.
//
// Motion is deliberately bounded. A region is a large area, and a flicker above
// three flashes a second across a large area is a photosensitivity hazard
// (WCAG 2.3.1), so the region wash is one fast attack and a smooth decay with a
// single softer rebound, about 2 Hz at most. The fast things, rings and a double
// pulse, are small and stay on the dots. Under prefers-reduced-motion the
// effect is one slow wash with no rings, beam travel or synapse pulse.

import * as d3 from "d3"

// ChR2 is driven at about 470 nm: a saturated blue, white at the fibre tip.
export const OPTO_BLUE = "#3fa9ff"
export const OPTO_CYAN = "#8fdcff"
export const OPTO_WHITE = "#eaf8ff"

export const OPTO_LAYER_CLASS = "opto-layer"
export const GRAPH_LAYER_CLASS = "graph-layer"

// Longest an effect element lives, so a burst of hits cannot pile up.
const MAX_LIVE_MS = 7000
const MAX_LAYER_CHILDREN = 900

export interface OptoTarget {
  // Categories whose region was recalled from (data-category-id on the cells).
  categoryIds: Iterable<string>
  // The exact items that were recalled (data-id on the cells).
  itemIds: Iterable<string>
}

function reducedMotion(): boolean {
  return typeof window !== "undefined" && typeof window.matchMedia === "function" &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches
}

interface Dot { id: string; cat: string; x: number; y: number }

function readDots(svg: d3.Selection<SVGSVGElement, unknown, null, undefined>): Dot[] {
  const dots: Dot[] = []
  svg.selectAll<SVGCircleElement, unknown>("circle[data-dot-id]").each(function () {
    dots.push({
      id: this.getAttribute("data-dot-id") ?? "",
      cat: this.getAttribute("data-dot-category-id") ?? "",
      x: parseFloat(this.getAttribute("cx") ?? "0"),
      y: parseFloat(this.getAttribute("cy") ?? "0"),
    })
  })
  return dots
}

function viewSize(svgEl: SVGSVGElement): { w: number; h: number } {
  const vb = svgEl.getAttribute("viewBox")?.split(/[\s,]+/).map(Number)
  if (vb && vb.length === 4 && vb[2] > 0 && vb[3] > 0) return { w: vb[2], h: vb[3] }
  return { w: svgEl.clientWidth || 800, h: svgEl.clientHeight || 600 }
}

// The layer all effect elements live in: inside the graph's own masked group, so
// the radial edge fade that hides the graph's rim hides the effect's too.
function layerOf(svg: d3.Selection<SVGSVGElement, unknown, null, undefined>): d3.Selection<SVGGElement, unknown, null, undefined> | null {
  const host = svg.select<SVGGElement>(`g.${GRAPH_LAYER_CLASS}`)
  if (host.empty()) return null
  let layer = host.select<SVGGElement>(`g.${OPTO_LAYER_CLASS}`)
  if (layer.empty()) {
    layer = host.append("g").attr("class", OPTO_LAYER_CLASS).attr("pointer-events", "none")
  }
  return layer
}

// Defs the effect needs, added once per draw (the draw effect wipes the svg).
function ensureDefs(svg: d3.Selection<SVGSVGElement, unknown, null, undefined>) {
  let defs = svg.select<SVGDefsElement>("defs")
  if (defs.empty()) defs = svg.append("defs")
  if (defs.select("#opto-soft").empty()) {
    defs.append("filter").attr("id", "opto-soft").attr("x", "-50%").attr("y", "-50%").attr("width", "200%").attr("height", "200%")
      .append("feGaussianBlur").attr("stdDeviation", 9)
    defs.append("filter").attr("id", "opto-bloom").attr("x", "-80%").attr("y", "-80%").attr("width", "260%").attr("height", "260%")
      .append("feGaussianBlur").attr("stdDeviation", 4)
    const halo = defs.append("radialGradient").attr("id", "opto-halo")
    halo.append("stop").attr("offset", "0%").attr("stop-color", OPTO_WHITE).attr("stop-opacity", 0.95)
    halo.append("stop").attr("offset", "35%").attr("stop-color", OPTO_CYAN).attr("stop-opacity", 0.55)
    halo.append("stop").attr("offset", "100%").attr("stop-color", OPTO_BLUE).attr("stop-opacity", 0)
  }
  return defs
}

function expire(el: Element, ms: number) {
  window.setTimeout(() => el.remove(), Math.min(ms, MAX_LIVE_MS))
}

// Fire the effect for a recall. Safe to call with ids the graph does not draw:
// those simply have nothing to light.
export function fireOptogenetic(svgEl: SVGSVGElement, target: OptoTarget): void {
  const svg = d3.select(svgEl)
  const layer = layerOf(svg)
  if (!layer) return
  const still = reducedMotion()
  const dots = readDots(svg)
  const direct = new Set(target.itemIds)
  const regions = new Set(target.categoryIds)
  // A recalled item that is drawn also names its region, so the beam does not
  // depend on the server having sent the kinds of the hit.
  for (const d of dots) if (direct.has(d.id)) regions.add(d.cat)
  if (regions.size === 0 && direct.size === 0) return

  // Keep the layer bounded when hits arrive faster than effects expire.
  const kids = layer.node()!.children
  while (kids.length > MAX_LAYER_CHILDREN) kids[0].remove()

  ensureDefs(svg)
  const { w, h } = viewSize(svgEl)
  const cx = w / 2, cy = h / 2

  regions.forEach(catId => {
    const members = dots.filter(d => d.cat === catId)
    if (members.length === 0) return

    // The fibre: out beyond the region, on the line from the centre of the graph.
    const mx = members.reduce((s, d) => s + d.x, 0) / members.length
    const my = members.reduce((s, d) => s + d.y, 0) / members.length
    let dx = mx - cx, dy = my - cy
    const len = Math.hypot(dx, dy)
    if (len < 1) { dx = 0; dy = -1 } else { dx /= len; dy /= len }
    const radius = Math.max(70, Math.min(190, Math.max(...members.map(d => Math.hypot(d.x - mx, d.y - my))) + 36))
    const reach = Math.min(w, h) * 0.34
    const tipX = mx + dx * reach, tipY = my + dy * reach
    // The beam continues past the centre of the region: light penetrates.
    const endX = mx - dx * radius * 0.7, endY = my - dy * radius * 0.7
    const px = -dy, py = dx // perpendicular

    const region = layer.append("g").attr("data-opto-region", catId)

    // --- the beam: a narrow fibre tip widening into a cone of blue light ------
    const gradId = `opto-beam-${Math.random().toString(36).slice(2, 8)}`
    const grad = svg.select("defs").append("linearGradient").attr("id", gradId)
      .attr("gradientUnits", "userSpaceOnUse").attr("x1", tipX).attr("y1", tipY).attr("x2", endX).attr("y2", endY)
    grad.append("stop").attr("offset", "0%").attr("stop-color", OPTO_WHITE).attr("stop-opacity", 0.95)
    grad.append("stop").attr("offset", "30%").attr("stop-color", OPTO_CYAN).attr("stop-opacity", 0.5)
    grad.append("stop").attr("offset", "100%").attr("stop-color", OPTO_BLUE).attr("stop-opacity", 0)
    expire(grad.node() as Element, 4200)

    const tipHalf = 4
    const beam = region.append("path")
      .attr("class", still ? "opto-beam opto-still" : "opto-beam")
      .attr("d",
        `M ${tipX + px * tipHalf} ${tipY + py * tipHalf} ` +
        `L ${endX + px * radius} ${endY + py * radius} ` +
        `L ${endX - px * radius} ${endY - py * radius} ` +
        `L ${tipX - px * tipHalf} ${tipY - py * tipHalf} Z`)
      .attr("fill", `url(#${gradId})`)
      .attr("filter", "url(#opto-soft)")
      .style("mix-blend-mode", "screen")
    expire(beam.node() as Element, 3200)

    // The fibre tip itself.
    const tip = region.append("circle")
      .attr("class", still ? "opto-tip opto-still" : "opto-tip")
      .attr("cx", tipX).attr("cy", tipY).attr("r", 7)
      .attr("fill", OPTO_WHITE).attr("filter", "url(#opto-bloom)")
    expire(tip.node() as Element, 3200)

    // --- the region: cells light in the order the light reaches them ----------
    const cells = svg.selectAll<SVGPathElement, unknown>(`path[data-category-id="${catId}"]`)
    const nearest = Math.min(...members.map(d => Math.hypot(d.x - tipX, d.y - tipY)))
    const farthest = Math.max(...members.map(d => Math.hypot(d.x - tipX, d.y - tipY)))
    const spread = Math.max(1, farthest - nearest)
    const byCell = new Map<string, Dot>(members.map(d => [d.id, d]))
    cells.each(function () {
      const id = this.getAttribute("data-id") ?? ""
      const d = byCell.get(id)
      const delay = still ? 0 : Math.round(((d ? Math.hypot(d.x - tipX, d.y - tipY) : nearest) - nearest) / spread * 320)
      const isDirect = direct.has(id)
      const wash = region.append("path")
        .attr("d", this.getAttribute("d"))
        .attr("fill", isDirect ? OPTO_WHITE : OPTO_BLUE)
        .attr("class", `${isDirect ? "opto-fire" : "opto-wash"}${still ? " opto-still" : ""}`)
        .style("animation-delay", `${delay}ms`)
        .style("mix-blend-mode", "screen")
      expire(wash.node() as Element, 4200 + delay)
    })
    expire(region.node() as Element, 4800)
  })

  // --- the recalled cells: spike rings and an afterglow --------------------
  const fired = dots.filter(d => direct.has(d.id))
  fired.forEach(d => {
    const halo = layer.append("circle")
      .attr("class", still ? "opto-halo opto-still" : "opto-halo")
      .attr("cx", d.x).attr("cy", d.y).attr("r", 22)
      .attr("fill", "url(#opto-halo)")
      .style("mix-blend-mode", "screen")
    expire(halo.node() as Element, 6200)

    if (still) return
    for (let i = 0; i < 3; i++) {
      const ring = layer.append("circle")
        .attr("class", "opto-ring")
        .attr("cx", d.x).attr("cy", d.y).attr("r", 5)
        .attr("fill", "none").attr("stroke", OPTO_CYAN).attr("stroke-width", 1.6)
        .style("animation-delay", `${i * 150}ms`)
      expire(ring.node() as Element, 1500 + i * 150)
    }
  })

  // --- the signal travels along the synapses to connected memories ---------
  if (!still && fired.length > 0) {
    const eps = 0.5
    svg.selectAll<SVGLineElement, unknown>("g > line").each(function () {
      const x1 = parseFloat(this.getAttribute("x1") ?? "NaN"), y1 = parseFloat(this.getAttribute("y1") ?? "NaN")
      const x2 = parseFloat(this.getAttribute("x2") ?? "NaN"), y2 = parseFloat(this.getAttribute("y2") ?? "NaN")
      if ([x1, y1, x2, y2].some(Number.isNaN)) return
      for (const d of fired) {
        const fromStart = Math.abs(d.x - x1) < eps && Math.abs(d.y - y1) < eps
        const fromEnd = Math.abs(d.x - x2) < eps && Math.abs(d.y - y2) < eps
        if (!fromStart && !fromEnd) continue
        const [ax, ay, bx, by] = fromStart ? [x1, y1, x2, y2] : [x2, y2, x1, y1]
        const length = Math.hypot(bx - ax, by - ay)
        if (length < 1) continue
        const pulse = layer.append("line")
          .attr("class", "opto-synapse")
          .attr("x1", ax).attr("y1", ay).attr("x2", bx).attr("y2", by)
          .attr("stroke", OPTO_CYAN).attr("stroke-width", 2.4).attr("stroke-linecap", "round")
          .attr("pathLength", 100)
          .attr("filter", "url(#opto-bloom)")
          .style("animation-delay", "180ms")
        expire(pulse.node() as Element, 1400)
        break
      }
    })
  }
}
