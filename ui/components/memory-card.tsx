"use client"

import Link from "next/link"
import { cn } from "@/lib/utils"
import type { MemoryItem } from "@/lib/api"
import { categoryOf } from "@/lib/category-config"

// Strip markdown syntax to a clean plain-text snippet for card previews.
function plainPreview(md: string): string {
  return md
    .replace(/^!\[[^\]]*\]\([^)]*\)\s*/g, "")  // leading image
    .replace(/!\[[^\]]*\]\([^)]*\)/g, "")        // inline images
    .replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")     // links → text
    .replace(/`{1,3}([^`]*)`{1,3}/g, "$1")       // code spans
    .replace(/^#{1,6}\s+/gm, "")                  // headings
    .replace(/(\*\*|__)(.*?)\1/g, "$2")           // bold
    .replace(/(\*|_)(.*?)\1/g, "$2")              // italic
    .replace(/^>\s?/gm, "")                        // blockquotes
    .replace(/^[-*+]\s+/gm, "")                    // list bullets
    .replace(/\s+/g, " ")                          // collapse whitespace
    .trim()
}

interface Props {
  item: MemoryItem
  highlighted?: boolean
  compact?: boolean
  // When provided, the card opens via this callback (mobile in-shell overlay)
  // instead of navigating to /item.
  onOpen?: (id: string, kind?: string) => void
}

export function MemoryCard({ item, highlighted, compact, onOpen }: Props) {
  const category = categoryOf(item)
  const label = category.label
  const Icon = category.icon

  const className = cn(
    "block w-full text-left rounded-xl border overflow-hidden transition-all hover:border-primary/40 hover:shadow-sm",
    highlighted
      ? "border-primary/60 bg-primary/5 shadow-md shadow-primary/10"
      : "border-border bg-card hover:bg-accent/30",
  )

  const inner = (
    <>
      {item.imageUrl && !compact && (
        // eslint-disable-next-line @next/next/no-img-element
        <img src={item.imageUrl} alt="" onError={e => { (e.target as HTMLImageElement).style.display = "none" }} className="w-full h-32 object-cover" />
      )}
      <div className={cn("p-4", compact && "py-3")}>
      <div className="flex items-start justify-between gap-2 mb-1.5">
        <span
          className="inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[10px] font-medium"
          style={{ color: category.color, borderColor: `${category.color}33`, background: `${category.color}1a` }}
        >
          <Icon size={11} />
          {label}
        </span>
      </div>
      <h3 className={cn("font-medium leading-snug", compact ? "text-sm" : "text-base")}>{item.title}</h3>
      {item.eventDate && (
        <p className="mt-1 text-xs text-violet-400 font-medium">
          {new Date(item.eventDate).toLocaleDateString("en-US", { weekday: "short", month: "short", day: "numeric", year: "numeric" })}
        </p>
      )}
      {!compact && (
        <p className="mt-1.5 text-sm text-muted-foreground line-clamp-2">{plainPreview(item.content)}</p>
      )}
      {item.tags && item.tags.length > 0 && (
        <div className="mt-2 flex flex-wrap gap-1">
          {item.tags.slice(0, 4).map(t => (
            <span key={t} className="text-[10px] bg-muted text-muted-foreground rounded px-1.5 py-0.5">{t}</span>
          ))}
        </div>
      )}
      </div>
    </>
  )

  if (onOpen) {
    return (
      <button type="button" onClick={() => onOpen(item.id, item.kind)} className={className}>
        {inner}
      </button>
    )
  }

  return (
    <Link href={`/item?id=${item.id}`} className={className}>
      {inner}
    </Link>
  )
}
