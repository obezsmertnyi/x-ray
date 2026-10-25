"use client"

import { useEffect, useRef, useState, useCallback } from "react"
import { ArrowLeft, Clock, Tag } from "lucide-react"
import ReactMarkdown from "react-markdown"
import remarkGfm from "remark-gfm"
import { useMemoryItem } from "@/hooks/use-memory"
import { categoryOf } from "@/lib/category-config"

function fmt(d: string) {
  return new Date(d).toLocaleDateString("en-US", { year: "numeric", month: "short", day: "numeric" })
}

// Turn bare URLs into markdown links so they render as tappable anchors (skips
// URLs already inside a markdown link).
function autolink(text: string): string {
  return text.replace(/(?<!\(|\]\()(https?:\/\/[^\s)>"]+)/g, (url) => `[${url}](${url})`)
}

interface Props {
  id: string
  kind?: string
  onClose: () => void
}

const CLOSE_THRESHOLD = 70

// Full-screen detail view layered above the mobile swipe shell. Swipe right to
// close (back); vertical drags scroll the content natively. Closing returns to
// the same tab/scroll spot.
export function MobileItemOverlay({ id, kind, onClose }: Props) {
  const { item, loading } = useMemoryItem(id, kind)
  const [dragX, setDragX] = useState(0)
  const [dragging, setDragging] = useState(false)
  const start = useRef<{ x: number; y: number } | null>(null)
  const axis = useRef<"h" | "v" | null>(null)

  // Lock body scroll while the overlay is open.
  useEffect(() => {
    const prev = document.body.style.overflow
    document.body.style.overflow = "hidden"
    return () => { document.body.style.overflow = prev }
  }, [])

  const onTouchStart = useCallback((e: React.TouchEvent) => {
    start.current = { x: e.touches[0].clientX, y: e.touches[0].clientY }
    axis.current = null
    setDragging(true)
  }, [])

  const onTouchMove = useCallback((e: React.TouchEvent) => {
    if (!start.current) return
    const dx = e.touches[0].clientX - start.current.x
    const dy = e.touches[0].clientY - start.current.y
    if (axis.current === null && (Math.abs(dx) > 10 || Math.abs(dy) > 10)) {
      axis.current = Math.abs(dx) > Math.abs(dy) ? "h" : "v"
    }
    // Only a rightward swipe (back) follows; vertical passes through to scroll.
    if (axis.current === "h") setDragX(Math.max(0, dx))
  }, [])

  const onTouchEnd = useCallback(() => {
    if (axis.current === "h" && dragX >= CLOSE_THRESHOLD) {
      onClose()
      return
    }
    setDragX(0)
    setDragging(false)
    start.current = null
    axis.current = null
  }, [dragX, onClose])

  const label = item ? categoryOf(item).label : ""

  return (
    <div
      className="fixed inset-0 z-50 flex flex-col bg-background overflow-hidden"
      style={{
        transform: dragX ? `translateX(${dragX}px)` : undefined,
        transition: dragging ? "none" : "transform 0.2s cubic-bezier(0.4,0,0.2,1)",
      }}
      onTouchStart={onTouchStart}
      onTouchMove={onTouchMove}
      onTouchEnd={onTouchEnd}
    >
      <div className="flex-none flex items-center gap-3 px-4 pt-3 pb-2 border-b border-border/40 bg-background/95 backdrop-blur-md z-10"
           style={{ paddingTop: "max(0.75rem, env(safe-area-inset-top, 0px))" }}>
        <button
          onClick={onClose}
          className="flex items-center gap-1.5 text-sm text-muted-foreground active:text-foreground transition-colors -ml-1 px-1 py-1"
        >
          <ArrowLeft className="h-4 w-4" /> {label || "Back"}
        </button>
        <span className="ml-auto text-[10px] uppercase tracking-widest text-muted-foreground/40 select-none pointer-events-none">swipe right to close</span>
      </div>

      <div className="flex-1 overflow-y-auto overscroll-contain"
           style={{ paddingBottom: "max(2rem, env(safe-area-inset-bottom, 0px))" }}>
        {loading ? (
          <div className="flex items-center justify-center py-24">
            <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-primary/40" />
          </div>
        ) : !item ? (
          <p className="text-center text-muted-foreground py-24 text-sm">Item not found.</p>
        ) : (
          <article className="p-5">
            {item.imageUrl && (
              // eslint-disable-next-line @next/next/no-img-element
              <img
                src={item.imageUrl}
                alt=""
                onError={e => { (e.target as HTMLImageElement).style.display = "none" }}
                className="w-full max-h-56 object-cover rounded-xl border border-border/40 mb-4"
              />
            )}

            <div className="flex flex-wrap items-center gap-2 mb-3">
              <span className="text-xs font-medium bg-primary/10 text-primary rounded-full px-2.5 py-0.5">{label}</span>
              {item.scope && (
                <span className="text-xs text-muted-foreground bg-muted rounded-full px-2.5 py-0.5">{item.scope}</span>
              )}
            </div>

            <h1 className="text-xl font-bold leading-snug mb-3">{item.title}</h1>

            <div className="flex items-center gap-4 text-xs text-muted-foreground mb-5">
              {item.eventDate ? (
                <span className="flex items-center gap-1 text-violet-400 font-medium">
                  <Clock className="h-3 w-3" />
                  {new Date(item.eventDate).toLocaleDateString("en-US", { weekday: "long", month: "long", day: "numeric", year: "numeric" })}
                </span>
              ) : (
                <span className="flex items-center gap-1">
                  <Clock className="h-3 w-3" /> Updated {fmt(item.updatedAt)}
                </span>
              )}
              {item.subject && <span>by <em>{item.subject}</em></span>}
            </div>

            <div className="prose prose-sm prose-invert max-w-none
                prose-p:text-foreground/90 prose-p:leading-relaxed
                prose-headings:text-foreground prose-headings:font-bold
                prose-img:rounded-xl prose-img:border prose-img:border-border/40
                prose-code:text-primary prose-code:bg-muted prose-code:px-1 prose-code:rounded
                prose-strong:text-foreground prose-li:text-foreground/90">
              <ReactMarkdown
                remarkPlugins={[remarkGfm]}
                components={{
                  a: ({ href, children }) => (
                    <a href={href} target="_blank" rel="noopener noreferrer"
                       className="text-primary font-medium underline underline-offset-2 decoration-primary/50 break-all active:opacity-70">
                      {children}
                    </a>
                  ),
                  img: ({ src, alt }) => (
                    <img src={src} alt={alt ?? ""} className="rounded-xl border border-border/40 w-full object-cover my-3" />
                  ),
                }}
              >
                {autolink(item.content)}
              </ReactMarkdown>
            </div>

            {item.tags && item.tags.length > 0 && (
              <div className="mt-6 flex items-center gap-2 flex-wrap">
                <Tag className="h-3.5 w-3.5 text-muted-foreground flex-shrink-0" />
                {item.tags.map(t => (
                  <span key={t} className="text-xs bg-muted text-muted-foreground rounded px-2 py-0.5">{t}</span>
                ))}
              </div>
            )}
          </article>
        )}
      </div>
    </div>
  )
}
