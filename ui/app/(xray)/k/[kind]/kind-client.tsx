"use client"

import { useMemoryList, useMemoryHits, useMemorySearch } from "@/hooks/use-memory"
import { MemoryCard } from "@/components/memory-card"
import { useState, useMemo, useEffect } from "react"
import { categoryById, categoryOf, UNKNOWN_CATEGORY } from "@/lib/category-config"
import { useCategories } from "@/hooks/use-categories"
import { fetchUserNotes, type MemoryItem } from "@/lib/api"

export function KindPageClient({ categoryId }: { categoryId: string }) {
  // Subscribes to category config so a direct load of /k/<id> (before
  // loadRuntimeCategories() resolves) re-renders once the real taxonomy arrives.
  useCategories()
  const category = categoryById(categoryId) ?? UNKNOWN_CATEGORY
  // Notes are private per-user cells — excluded from the shared list endpoint;
  // they're served only via the ownership-checked /v1/user/notes path.
  const isNotes = category.id === "notes"

  // Fetch by the backend kind, then narrow to this category client-side: a
  // tagged category (e.g. Projects/Team) and the bare-kind fallback share a
  // kind, so the same fetch serves both and the category resolver splits them.
  const { items: sharedItems, loading: sharedLoading } = useMemoryList(
    isNotes ? {} : { kind: category.kind },
  )
  const [noteItems, setNoteItems] = useState<MemoryItem[]>([])
  const [notesLoading, setNotesLoading] = useState(isNotes)
  useEffect(() => {
    if (!isNotes) return
    setNotesLoading(true)
    fetchUserNotes().then(setNoteItems).catch(() => setNoteItems([])).finally(() => setNotesLoading(false))
  }, [isNotes])

  const items = isNotes ? noteItems : sharedItems
  const loading = isNotes ? notesLoading : sharedLoading
  const { hits } = useMemoryHits()
  const [query, setQuery] = useState("")
  const hitSet = useMemo(() => new Set(hits?.itemIds ?? []), [hits])
  const { label, color } = category

  const inCategory = useMemo(
    () => isNotes ? items : items.filter(it => categoryOf(it).id === category.id),
    [items, category.id, isNotes],
  )

  // Server-first search (mirrors memory-graph.tsx): the local list above only
  // ever holds what useMemoryList's capped fetch returned — a client-side
  // filter alone silently scopes this page's search to that capped list.
  // useMemorySearch debounces the same query against the full store, scoped
  // to this category's kind, merged with the local matches below, deduped by
  // id. Notes are private per-user cells excluded from the shared search
  // endpoint (server-side, unconditionally) — skip it here so the notes tab
  // stays entirely on its own ownership-checked fetch path.
  const { results: searchResults } = useMemorySearch(isNotes ? "" : query, isNotes ? undefined : category.kind)

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return inCategory
    const localHits = inCategory.filter(it =>
      it.id.toLowerCase().includes(q) ||
      it.title.toLowerCase().includes(q) ||
      it.content?.toLowerCase().includes(q) ||
      it.subject?.toLowerCase().includes(q) ||
      it.tags?.some(t => t.toLowerCase().includes(q)),
    )
    if (isNotes) return localHits
    const remoteHits = searchResults.filter(it => categoryOf(it).id === category.id)
    const byId = new Map<string, MemoryItem>()
    for (const it of [...remoteHits, ...localHits]) byId.set(it.id, it)
    return Array.from(byId.values())
  }, [inCategory, query, searchResults, category.id, isNotes])

  return (
    <div className="p-6 max-w-4xl mx-auto">
      <div className="mb-6 flex items-center gap-2">
        <span className="h-3 w-3 rounded-full" style={{ background: color }} />
        <h1 className="text-2xl font-bold">{label}</h1>
        <span className="text-muted-foreground text-sm">({filtered.length})</span>
      </div>

      <input
        className="w-full rounded-lg border border-border bg-card px-4 py-2 text-sm placeholder:text-muted-foreground focus:outline-none focus:ring-2 focus:ring-primary/40 mb-6"
        placeholder={`Search ${label.toLowerCase()}…`}
        value={query}
        onChange={e => setQuery(e.target.value)}
      />

      {loading ? (
        <div className="flex items-center justify-center py-24">
          <div className="animate-spin rounded-full h-8 w-8 border-b-2 border-primary/40" />
        </div>
      ) : filtered.length === 0 ? (
        <p className="text-center text-muted-foreground py-24 text-sm">No {label.toLowerCase()} yet.</p>
      ) : (
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
          {filtered.map(item => (
            <MemoryCard key={item.id} item={item} highlighted={hitSet.has(item.id)} />
          ))}
        </div>
      )}
    </div>
  )
}
