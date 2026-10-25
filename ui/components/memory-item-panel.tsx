"use client"

import { useState, useEffect, useRef, useCallback } from "react"
import { useMemoryItem, useMemoryList } from "@/hooks/use-memory"
import { X, Pin, Clock, Tag, ArrowLeft, Minus, ChevronDown } from "lucide-react"
import { cn } from "@/lib/utils"
import { fetchUserNotes, type MemoryItem } from "@/lib/api"
import { categoryOf, categoryById, type Category } from "@/lib/category-config"
import ReactMarkdown from "react-markdown"
import remarkGfm from "remark-gfm"

function autolink(text: string): string {
  return text.replace(/(?<!\(|\]\()(https?:\/\/[^\s\)>"]+)/g, (url) => `[${url}](${url})`)
}

function fmt(d: string) {
  return new Date(d).toLocaleDateString("en-US", { year: "numeric", month: "short", day: "numeric" })
}

// ── Section list ─────────────────────────────────────────────────────────────

function SectionList({ categoryId, color, onSelect }: { categoryId: string; color: string; onSelect: (id: string) => void }) {
  const category = categoryById(categoryId)
  const isNotes = categoryId === "notes"
  // Fetch by the category's backend kind, then narrow to the category by
  // (kind, tag) — Projects and Team share a kind and split by tag.
  const { items: sharedItems, loading: sharedLoading } = useMemoryList({ kind: category?.kind })
  const [noteItems, setNoteItems] = useState<MemoryItem[]>([])
  const [notesLoading, setNotesLoading] = useState(false)
  useEffect(() => {
    if (!isNotes) return
    setNotesLoading(true)
    fetchUserNotes().then(setNoteItems).catch(() => {}).finally(() => setNotesLoading(false))
  }, [isNotes])
  const items = isNotes ? noteItems : sharedItems
  const loading = isNotes ? notesLoading : sharedLoading
  const filtered = category ? items.filter(it => categoryOf(it).id === category.id) : items
  const sorted = [...filtered].sort((a, b) => new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime())

  if (loading) return (
    <div className="flex items-center justify-center h-32">
      <div className="animate-spin rounded-full h-6 w-6 border-b-2" style={{ borderColor: color }} />
    </div>
  )

  return (
    <div className="flex flex-col gap-2">
      {sorted.map(item => (
        <button
          key={item.id}
          onClick={() => onSelect(item.id)}
          className="text-left rounded-xl border border-white/8 bg-white/4 hover:bg-white/8 hover:border-white/15 p-4 transition-all group"
        >
          <div className="flex items-start justify-between gap-2 mb-1">
            <span className="text-xs font-medium" style={{ color, opacity: 0.7 }}>
              {item.eventDate
                ? new Date(item.eventDate).toLocaleDateString("en-US", { month: "short", day: "numeric", year: "numeric" })
                : fmt(item.updatedAt)}
            </span>
            {item.subject && item.kind !== "note" && <span className="text-[10px] text-muted-foreground italic">{item.subject}</span>}
          </div>
          <div className="flex items-start gap-3 mt-1">
            <p className="text-sm font-semibold leading-snug group-hover:text-foreground text-foreground/80 flex-1">{item.title}</p>
            {item.imageUrl && (
              <img
                src={item.imageUrl}
                alt=""
                onError={e => { (e.target as HTMLImageElement).style.display = "none" }}
                className="w-14 h-10 rounded-lg object-cover border border-border/30 flex-shrink-0"
              />
            )}
          </div>
          {item.tags && item.tags.filter(t => !["high","medium","low"].includes(t)).length > 0 && (
            <div className="mt-2 flex gap-1 flex-wrap">
              {item.tags.filter(t => !["high","medium","low"].includes(t)).slice(0, 3).map(t => (
                <span key={t} className="text-[10px] bg-muted text-muted-foreground rounded px-1.5 py-0.5">{t}</span>
              ))}
            </div>
          )}
        </button>
      ))}
    </div>
  )
}

// ── Item detail ───────────────────────────────────────────────────────────────

function ItemDetail({ id, kind, color, onViewAll }: { id: string; kind?: string; color: string; onViewAll: () => void }) {
  const { item, loading } = useMemoryItem(id, kind)

  if (loading) return (
    <div className="flex items-center justify-center h-32">
      <div className="animate-spin rounded-full h-6 w-6 border-b-2" style={{ borderColor: color }} />
    </div>
  )
  if (!item) return <p className="text-muted-foreground text-sm">Item not found.</p>

  return (
    <article>
      {item.imageUrl && (
        <img
          src={item.imageUrl}
          alt={item.title}
          onError={e => { (e.target as HTMLImageElement).style.display = "none" }}
          className="w-full rounded-xl border border-border/40 object-cover mb-4"
          style={{ maxHeight: "180px" }}
        />
      )}
      <div className="w-8 h-0.5 rounded-full mb-4" style={{ background: color }} />
      <h2 className="text-sm font-bold leading-snug mb-2">{item.title}</h2>

      <div className="flex flex-wrap items-center gap-3 text-[11px] text-muted-foreground mb-4">
        {item.eventDate ? (
          <span className="flex items-center gap-1 font-medium" style={{ color }}>
            <Clock className="h-3 w-3" />
            {new Date(item.eventDate).toLocaleDateString("en-US", { weekday: "short", month: "short", day: "numeric", year: "numeric" })}
          </span>
        ) : (
          <span className="flex items-center gap-1">
            <Clock className="h-3 w-3" />{fmt(item.updatedAt)}
          </span>
        )}
        {item.subject && item.kind !== "note" && <span className="italic">{item.subject}</span>}
      </div>

      <div className="prose prose-xs prose-invert max-w-none text-[13px]
          prose-p:text-foreground/85 prose-p:leading-relaxed prose-p:my-2
          prose-a:no-underline
          prose-img:rounded-xl prose-img:border prose-img:border-border/40 prose-img:my-3
          prose-headings:text-foreground prose-headings:font-bold prose-headings:text-sm
          prose-code:text-primary prose-code:bg-muted prose-code:px-1 prose-code:rounded
          prose-blockquote:border-l-primary/40 prose-blockquote:text-muted-foreground
          prose-strong:text-foreground prose-li:text-foreground/85 prose-li:my-0.5">
        <ReactMarkdown
          remarkPlugins={[remarkGfm]}
          components={{
            a: ({ href, children }) => (
              <a href={href} target="_blank" rel="noopener noreferrer" className="break-all" style={{ color, textDecoration: "none" }}>{children}</a>
            ),
            img: ({ src, alt }) => (
              <img src={src} alt={alt ?? ""} className="rounded-xl border border-border/40 w-full object-cover my-3" />
            ),
          }}
        >
          {autolink(item.content)}
        </ReactMarkdown>
      </div>

      {item.tags && item.tags.filter(t => !["high","medium","low"].includes(t)).length > 0 && (
        <div className="mt-6 flex items-center gap-1.5 flex-wrap">
          <Tag className="h-3 w-3 text-muted-foreground flex-shrink-0" />
          {item.tags.filter(t => !["high","medium","low"].includes(t)).map(t => (
            <span key={t} className="text-[11px] bg-muted text-muted-foreground rounded px-2 py-0.5">{t}</span>
          ))}
        </div>
      )}

      <div className="mt-8 pt-4 border-t border-white/8 flex items-center justify-between gap-4">
        <button
          onClick={onViewAll}
          className="text-xs text-muted-foreground hover:text-foreground transition-colors"
        >
          View all {categoryOf(item).label} →
        </button>
        <span className="text-[10px] font-mono text-muted-foreground/40 select-all truncate">{item.id}</span>
      </div>
    </article>
  )
}

// ── Floating panel ────────────────────────────────────────────────────────────

export const PANEL_W = 380
export const PANEL_H_MAX = 520
const SNAP_DIST = 20  // pixels within which edges snap together

export interface PanelProps {
  id: string
  kind?: string
  initialX: number
  initialY: number
  initialMinimized?: boolean
  pinned?: boolean
  pinnedIds?: Set<string>  // IDs already pinned — suppresses pin button for duplicates
  onPin?: (id: string, x: number, y: number, kind?: string) => void  // passes currently-viewed id + drag position
  onFocus?: () => void  // called on mousedown — parent uses to bring panel to front
  onMove?: (x: number, y: number) => void  // called after drag ends
  onMinimizedChange?: (minimized: boolean) => void
  onClose: () => void
}

type PanelView =
  | { mode: "item"; id: string; kind?: string }
  | { mode: "list"; categoryId: string }

export function MemoryItemPanel({ id, kind: initialKind, initialX, initialY, initialMinimized, pinned, pinnedIds, onPin, onFocus, onMove, onMinimizedChange, onClose }: PanelProps) {
  const { item } = useMemoryItem(id, initialKind)
  const [view, setView] = useState<PanelView>({ mode: "item", id, kind: initialKind })
  const [pos, setPos] = useState<{ x: number; y: number }>({ x: initialX, y: initialY })
  const [minimized, setMinimized] = useState(initialMinimized ?? false)
  const dragOffset = useRef<{ dx: number; dy: number } | null>(null)
  const panelRef = useRef<HTMLDivElement>(null)
  const onFocusRef = useRef(onFocus)
  onFocusRef.current = onFocus
  const onMoveRef = useRef(onMove)
  onMoveRef.current = onMove
  const onMinimizedChangeRef = useRef(onMinimizedChange)
  onMinimizedChangeRef.current = onMinimizedChange

  // When the id changes (active panel navigated to a different item), reset view
  useEffect(() => {
    setView({ mode: "item", id, kind: initialKind })
  }, [id, initialKind])

  const resolvedView = view

  // The active category drives the header label and accent color. In list mode
  // it's the section's category; in item mode it's resolved from the item.
  const category: Category | undefined = resolvedView.mode === "list"
    ? categoryById(resolvedView.categoryId)
    : item ? categoryOf(item) : undefined
  const color = category?.color ?? "#94a3b8"
  const panelW = PANEL_W

  const onMouseDown = useCallback((e: React.MouseEvent) => {
    e.preventDefault()
    onFocusRef.current?.()
    dragOffset.current = { dx: e.clientX - pos.x, dy: e.clientY - pos.y }

    const onDrag = (ev: MouseEvent) => {
      if (!dragOffset.current) return
      const vw = window.innerWidth
      const vh = window.innerHeight
      const w = panelRef.current?.offsetWidth ?? PANEL_W
      const h = panelRef.current?.offsetHeight ?? PANEL_H_MAX
      let x = Math.max(0, Math.min(vw - w, ev.clientX - dragOffset.current.dx))
      let y = Math.max(0, Math.min(vh - h, ev.clientY - dragOffset.current.dy))

      // Snap to edges of sibling panels.
      const siblings = Array.from(document.querySelectorAll<HTMLElement>("[data-panel]"))
        .filter(el => el !== panelRef.current)
      for (const sib of siblings) {
        const r = sib.getBoundingClientRect()
        // Horizontal: snap this left→sib right, this right→sib left, or align left edges
        if (Math.abs(x - r.right) < SNAP_DIST) x = r.right + 4
        else if (Math.abs((x + w) - r.left) < SNAP_DIST) x = r.left - w - 4
        else if (Math.abs(x - r.left) < SNAP_DIST) x = r.left
        // Vertical: snap this top→sib bottom, this bottom→sib top, or align top edges
        if (Math.abs(y - r.bottom) < SNAP_DIST) y = r.bottom + 4
        else if (Math.abs((y + h) - r.top) < SNAP_DIST) y = r.top - h - 4
        else if (Math.abs(y - r.top) < SNAP_DIST) y = r.top
      }

      setPos({ x, y })
    }
    const onUp = () => {
      if (dragOffset.current) {
        // Report final position after drag so parent can persist it.
        setPos(p => { onMoveRef.current?.(p.x, p.y); return p })
      }
      dragOffset.current = null
      window.removeEventListener("mousemove", onDrag)
      window.removeEventListener("mouseup", onUp)
    }
    window.addEventListener("mousemove", onDrag)
    window.addEventListener("mouseup", onUp)
  }, [pos])

  return (
    <div
      ref={panelRef}
      data-panel=""
      className="fixed z-40 flex flex-col rounded-2xl overflow-hidden"
      style={{
        width: panelW,
        maxHeight: PANEL_H_MAX,
        left: pos.x,
        top: pos.y,
        background: `
          radial-gradient(ellipse 80% 40% at 50% 0%, color-mix(in srgb, ${color} 18%, transparent) 0%, transparent 70%),
          linear-gradient(160deg, color-mix(in srgb, ${color} 10%, #080810) 0%, #09090f 45%, #060608 100%)
        `,
        border: `1px solid color-mix(in srgb, ${color} 25%, rgba(255,255,255,0.06))`,
        boxShadow: `
          0 0 0 1px color-mix(in srgb, ${color} 10%, transparent),
          0 32px 64px rgba(0,0,0,0.7),
          0 0 120px color-mix(in srgb, ${color} 12%, transparent),
          inset 0 1px 0 color-mix(in srgb, ${color} 30%, rgba(255,255,255,0.08))
        `,
      }}
    >
      {/* Top glow strip */}
      <div
        className="absolute top-0 left-0 right-0 h-px pointer-events-none"
        style={{ background: `linear-gradient(90deg, transparent, ${color}88, transparent)` }}
      />

      {/* Header — always a drag handle */}
      <div
        className="flex items-center justify-between px-4 py-3 flex-shrink-0 cursor-move select-none relative z-10"
        style={{ borderBottom: `1px solid color-mix(in srgb, ${color} 15%, rgba(255,255,255,0.05))` }}
        onMouseDown={onMouseDown}
      >
        <div className="flex items-center gap-2 min-w-0">
          {resolvedView.mode === "list" && !minimized && (
            <button
              onClick={() => setView({ mode: "item", id, kind: initialKind })}
              className="p-1 rounded-md text-muted-foreground hover:text-foreground hover:bg-white/8 transition-colors flex-shrink-0"
              onMouseDown={e => e.stopPropagation()}
            >
              <ArrowLeft className="h-3.5 w-3.5" />
            </button>
          )}
          <span className="text-[10px] font-bold tracking-[0.15em] uppercase flex-shrink-0" style={{ color }}>
            {category?.label ?? ""}
          </span>
          {minimized && item?.title && (
            <span className="text-[11px] text-white/50 truncate ml-1">{item.title}</span>
          )}
        </div>
        <div className="flex items-center gap-1" onMouseDown={e => e.stopPropagation()}>
          {onPin && !pinnedIds?.has(resolvedView.mode === "item" ? resolvedView.id : id) && (
            <button
              onClick={() => onPin(resolvedView.mode === "item" ? resolvedView.id : id, pos.x, pos.y, resolvedView.mode === "item" ? resolvedView.kind : initialKind)}
              title="Pin panel"
              className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-white/8 transition-colors"
            >
              <Pin className="h-3.5 w-3.5" />
            </button>
          )}
          {pinned && (
            <button
              onClick={() => setMinimized(v => { onMinimizedChangeRef.current?.(!v); return !v })}
              title={minimized ? "Restore" : "Minimize"}
              className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-white/8 transition-colors"
            >
              {minimized ? <ChevronDown className="h-3.5 w-3.5" /> : <Minus className="h-3.5 w-3.5" />}
            </button>
          )}
          <button
            onClick={onClose}
            className="p-1.5 rounded-md text-muted-foreground hover:text-foreground hover:bg-white/8 transition-colors"
          >
            <X className="h-3.5 w-3.5" />
          </button>
        </div>
      </div>

      {/* Body */}
      {!minimized && (
        <div className="flex-1 overflow-y-auto px-4 py-4 relative z-10">
          {resolvedView.mode === "list" && (
            <SectionList
              categoryId={resolvedView.categoryId}
              color={color}
              onSelect={id => setView({
                mode: "item", id,
                // Notes carry kind "note" so the fetch routes to the per-user
                // notes endpoint; other categories fetch by id.
                kind: resolvedView.categoryId === "notes" ? "note" : undefined,
              })}
            />
          )}
          {resolvedView.mode === "item" && (
            <ItemDetail
              id={resolvedView.id}
              kind={resolvedView.kind}
              color={color}
              onViewAll={() => category && setView({ mode: "list", categoryId: category.id })}
            />
          )}
        </div>
      )}
    </div>
  )
}
