"use client"

import { useEffect, useRef } from "react"
import * as d3 from "d3"
import { Delaunay } from "d3-delaunay"

const KIND_COLOR: Record<string, string> = {
  system_identity: "#818cf8",
  stable_fact:     "#4ade80",
  recent_change:   "#fbbf24",
  event:           "#e879f9",
  contributor:     "#38bdf8",
}

const KIND_TIER: Record<string, number> = {
  system_identity: 0.60,
  stable_fact:     0.55,
  event:           0.75,
  recent_change:   0.75,
  contributor:     0.70,
}

const KIND_ANGLE: Record<string, number> = {
  system_identity: -Math.PI / 4,
  stable_fact:      Math.PI / 4,
  event:            Math.PI * 3 / 4,
  recent_change:   -Math.PI * 3 / 4,
  contributor:      Math.PI / 4 * 1.5,
}

// Deterministic fake nodes — same layout every render
const FAKE_NODES: { id: string; kind: string; tags?: string[] }[] = [
  ...Array.from({ length: 5 }, (_, i) => ({ id: `si${i}`, kind: "system_identity", tags: ["project"] })),
  ...Array.from({ length: 4 }, (_, i) => ({ id: `sf${i}`, kind: "stable_fact" })),
  ...Array.from({ length: 5 }, (_, i) => ({ id: `rc${i}`, kind: "recent_change" })),
  ...Array.from({ length: 3 }, (_, i) => ({ id: `ev${i}`, kind: "event" })),
  ...Array.from({ length: 4 }, (_, i) => ({ id: `co${i}`, kind: "contributor", tags: ["contributor"] })),
]

function hashStr(s: string): number {
  let h = 0
  for (let i = 0; i < s.length; i++) h = (Math.imul(31, h) + s.charCodeAt(i)) >>> 0
  return h
}
function lcg(seed: number): number {
  return ((seed * 1664525 + 1013904223) >>> 0) / 0x100000000
}

function seeds(items: typeof FAKE_NODES, w: number, h: number): [number, number][] {
  const cx = w / 2, cy = h / 2
  const maxR = Math.min(w, h) * 0.46
  const byKind: Record<string, typeof FAKE_NODES> = {}
  for (const it of items) { byKind[it.kind] ??= []; byKind[it.kind].push(it) }
  const arcSlice = Math.PI / 2
  return items.map((item) => {
    const tier = KIND_TIER[item.kind] ?? 0.5
    const hv = hashStr(item.id)
    const r = maxR * (tier + (lcg(hv) - 0.5) * 0.18)
    const kindItems = byKind[item.kind] ?? [item]
    const n = kindItems.length
    const itemIdx = kindItems.indexOf(item)
    const baseAngle = KIND_ANGLE[item.kind] ?? 0
    const spread = n === 1 ? 0 : (itemIdx / (n - 1) - 0.5) * arcSlice * 0.80
    const angle = baseAngle + spread + (lcg(hv + 7) - 0.5) * 0.08
    return [
      Math.max(8, Math.min(w - 8, cx + Math.cos(angle) * r)),
      Math.max(8, Math.min(h - 8, cy + Math.sin(angle) * r)),
    ]
  })
}

export function GhostGraph() {
  const containerRef = useRef<HTMLDivElement>(null)
  const svgRef = useRef<SVGSVGElement>(null)

  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    const w = container.clientWidth || 800
    const h = container.clientHeight || 600

    const svg = d3.select(svgRef.current)
    svg.selectAll("*").remove()
    svg.attr("viewBox", `0 0 ${w} ${h}`)

    const defs = svg.append("defs")

    // Edge fade mask
    const gradDefs = [
      { id: "gl-fade-l", x1: "0%", y1: "0%", x2: "100%", y2: "0%" },
      { id: "gl-fade-r", x1: "100%", y1: "0%", x2: "0%", y2: "0%" },
      { id: "gl-fade-t", x1: "0%", y1: "0%", x2: "0%", y2: "100%" },
      { id: "gl-fade-b", x1: "0%", y1: "100%", x2: "0%", y2: "0%" },
    ]
    gradDefs.forEach(({ id, x1, y1, x2, y2 }) => {
      const g = defs.append("linearGradient").attr("id", id)
        .attr("x1", x1).attr("y1", y1).attr("x2", x2).attr("y2", y2)
      g.append("stop").attr("offset", "0%").attr("stop-color", "black").attr("stop-opacity", 1)
      g.append("stop").attr("offset", "22%").attr("stop-color", "black").attr("stop-opacity", 0)
      g.append("stop").attr("offset", "100%").attr("stop-color", "black").attr("stop-opacity", 0)
    })
    const mask = defs.append("mask").attr("id", "gl-rmask")
    mask.append("rect").attr("width", w).attr("height", h).attr("fill", "white")
    ;["gl-fade-l", "gl-fade-r", "gl-fade-t", "gl-fade-b"].forEach(id => {
      mask.append("rect").attr("width", w).attr("height", h)
        .attr("fill", `url(#${id})`).attr("fill-opacity", 1)
        .style("mix-blend-mode", "multiply")
    })

    const pts = seeds(FAKE_NODES, w, h)
    const delaunay = Delaunay.from(pts)
    const voronoi = delaunay.voronoi([0, 0, w, h])

    const g = svg.append("g").attr("mask", "url(#gl-rmask)")

    // Voronoi cells
    FAKE_NODES.forEach((item, i) => {
      const color = KIND_COLOR[item.kind] ?? "#6b7280"
      g.append("path")
        .attr("d", voronoi.renderCell(i))
        .attr("fill", color)
        .attr("fill-opacity", 0.18)
        .attr("stroke", "rgba(255,255,255,0.12)")
        .attr("stroke-width", 1)
        .attr("pointer-events", "none")
    })

    // Synapses
    for (let i = 0; i < FAKE_NODES.length; i++) {
      const tagsI = new Set(FAKE_NODES[i].tags ?? [])
      if (tagsI.size === 0) continue
      for (let j = i + 1; j < FAKE_NODES.length; j++) {
        const tagsJ = FAKE_NODES[j].tags ?? []
        const shared = tagsJ.filter(t => tagsI.has(t)).length
        if (shared < 1) continue
        const [x1, y1] = pts[i]
        const [x2, y2] = pts[j]
        if (Math.hypot(x2 - x1, y2 - y1) > Math.min(w, h) * 0.55) continue
        const colorI = KIND_COLOR[FAKE_NODES[i].kind] ?? "#94a3b8"
        const gradId = `gl-syn-${i}-${j}`
        const grad = defs.append("linearGradient").attr("id", gradId)
          .attr("x1", x1).attr("y1", y1).attr("x2", x2).attr("y2", y2)
          .attr("gradientUnits", "userSpaceOnUse")
        grad.append("stop").attr("offset", "0%").attr("stop-color", colorI)
        grad.append("stop").attr("offset", "100%").attr("stop-color", KIND_COLOR[FAKE_NODES[j].kind] ?? "#94a3b8")
        g.append("line")
          .attr("x1", x1).attr("y1", y1).attr("x2", x2).attr("y2", y2)
          .attr("stroke", `url(#${gradId})`)
          .attr("stroke-width", 1)
          .attr("stroke-opacity", 0.18)
          .attr("pointer-events", "none")
      }
    }

    // Dots
    FAKE_NODES.forEach((item, i) => {
      const color = KIND_COLOR[item.kind] ?? "#6b7280"
      const [px, py] = pts[i]
      g.append("circle")
        .attr("cx", px).attr("cy", py).attr("r", 4)
        .attr("fill", color).attr("fill-opacity", 0.25)
        .attr("stroke", color).attr("stroke-width", 1.5)
        .attr("pointer-events", "none")
    })
  }, [])

  return (
    <div ref={containerRef} className="absolute inset-0 pointer-events-none">
      <svg ref={svgRef} className="w-full h-full" />
      {/* Radial fade */}
      <div className="absolute inset-0" style={{
        background: "radial-gradient(ellipse at center, transparent 40%, hsl(var(--background)) 85%)"
      }} />
      {/* Overall dim overlay */}
      <div className="absolute inset-0 bg-background/60" />
    </div>
  )
}
