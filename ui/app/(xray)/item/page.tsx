"use client"

import { useSearchParams } from "next/navigation"
import { Suspense } from "react"
import { useMemoryItem } from "@/hooks/use-memory"
import Link from "next/link"
import { ArrowLeft, Clock, Tag } from "lucide-react"
import ReactMarkdown from "react-markdown"
import remarkGfm from "remark-gfm"
import { categoryOf, categoryById } from "@/lib/category-config"

function fmt(d: string) {
  return new Date(d).toLocaleDateString("en-US", { year: "numeric", month: "short", day: "numeric" })
}

function ItemContent() {
  const params = useSearchParams()
  const id = params.get("id")
  const { item, loading } = useMemoryItem(id)

  if (loading) return (
    <div className="flex items-center justify-center py-24">
      <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-primary/40" />
    </div>
  )

  if (!item) return (
    <div className="p-6 max-w-3xl mx-auto">
      <Link href="/" className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground mb-6">
        <ArrowLeft className="h-3.5 w-3.5" /> Index
      </Link>
      <p className="text-muted-foreground">{id ? "Item not found." : "No item ID provided."}</p>
    </div>
  )

  // Back-link routes to the item's UI category (e.g. /k/projects), not the raw
  // kind — Projects and Team share a kind but are distinct categories. An
  // unrecognized category (no /k/[id] route generated) falls back to the index.
  const category = categoryOf(item)
  const backHref = categoryById(category.id) ? `/k/${category.id}` : "/"
  const backLabel = category.label

  return (
    <div className="p-6 max-w-3xl mx-auto">
      <Link href={backHref} className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground mb-6 transition-colors">
        <ArrowLeft className="h-3.5 w-3.5" /> {backLabel}
      </Link>

      <article>
        <div className="flex flex-wrap items-center gap-2 mb-3">
          <span className="text-xs font-medium bg-primary/10 text-primary rounded-full px-2.5 py-0.5">
            {backLabel}
          </span>
          {item.scope && (
            <span className="text-xs text-muted-foreground bg-muted rounded-full px-2.5 py-0.5">{item.scope}</span>
          )}

        </div>

        <h1 className="text-2xl font-bold leading-snug mb-4">{item.title}</h1>

        <div className="flex items-center gap-4 text-xs text-muted-foreground mb-6">
          {item.eventDate ? (
            <span className="flex items-center gap-1 text-violet-400 font-medium">
              <Clock className="h-3 w-3" />
              {new Date(item.eventDate).toLocaleDateString("en-US", { weekday: "long", month: "long", day: "numeric", year: "numeric" })}
            </span>
          ) : (
            <span className="flex items-center gap-1">
              <Clock className="h-3 w-3" />
              Updated {fmt(item.updatedAt)}
            </span>
          )}
          {item.subject && <span>by <em>{item.subject}</em></span>}
        </div>

        <div className="prose prose-sm prose-invert max-w-none
            prose-p:text-foreground/90 prose-p:leading-relaxed
            prose-headings:text-foreground prose-headings:font-bold
            prose-a:text-primary prose-a:no-underline
            prose-img:rounded-xl prose-img:border prose-img:border-border/40
            prose-code:text-primary prose-code:bg-muted prose-code:px-1 prose-code:rounded
            prose-strong:text-foreground prose-li:text-foreground/90">
          <ReactMarkdown
            remarkPlugins={[remarkGfm]}
            components={{
              a: ({ href, children }) => (
                <a href={href} target="_blank" rel="noopener noreferrer" className="break-all">{children}</a>
              ),
              img: ({ src, alt }) => (
                <img src={src} alt={alt ?? ""} className="rounded-xl border border-border/40 w-full object-cover my-3" />
              ),
            }}
          >
            {item.content}
          </ReactMarkdown>
        </div>

        {item.tags && item.tags.length > 0 && (
          <div className="mt-8 flex items-center gap-2 flex-wrap">
            <Tag className="h-3.5 w-3.5 text-muted-foreground flex-shrink-0" />
            {item.tags.map(t => (
              <span key={t} className="text-xs bg-muted text-muted-foreground rounded px-2 py-0.5">{t}</span>
            ))}
          </div>
        )}
      </article>
    </div>
  )
}

export default function ItemPage() {
  return (
    <Suspense fallback={
      <div className="flex items-center justify-center py-24">
        <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-primary/40" />
      </div>
    }>
      <ItemContent />
    </Suspense>
  )
}
