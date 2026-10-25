"use client"

import { useEffect, useState, useCallback, useRef } from "react"
import { fetchMemory, fetchMemoryItem, fetchMemorySearch, fetchMetrics, subscribeMemoryEvents, type MemoryItem, type MemoryHitEvent, type MemoryChangeEvent, type RuntimeMetrics } from "@/lib/api"

export function useMemoryList(
  params: { kind?: string; scope?: string; subject?: string } = {}
) {
  const [items, setItems] = useState<MemoryItem[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const key = JSON.stringify(params)

  useEffect(() => {
    setLoading(true)
    fetchMemory(params)
      .then(setItems)
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false))
  }, [key]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    return subscribeMemoryEvents(
      () => {},
      (evt: MemoryChangeEvent) => {
        if (evt.op === "upsert" && evt.item) {
          const item = evt.item
          const kindMatch = !params.kind || item.kind === params.kind
          const scopeMatch = !params.scope || item.scope === params.scope
          const subjectMatch = !params.subject || item.subject === params.subject
          if (!kindMatch || !scopeMatch || !subjectMatch) return
          setItems(prev => {
            const idx = prev.findIndex(i => i.id === item.id)
            if (idx >= 0) {
              const next = [...prev]
              next[idx] = item
              return next
            }
            return [...prev, item]
          })
        } else if (evt.op === "delete" && evt.id) {
          setItems(prev => prev.filter(i => i.id !== evt.id))
        }
      }
    )
  }, [key]) // eslint-disable-line react-hooks/exhaustive-deps

  return { items, loading, error }
}

export function useMemoryItem(id: string | null, kind?: string) {
  const [item, setItem] = useState<MemoryItem | null>(null)
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    if (!id) { setItem(null); return }
    setLoading(true)
    fetchMemoryItem(id, kind)
      .then(setItem)
      .catch(() => setItem(null))
      .finally(() => setLoading(false))
  }, [id, kind])

  useEffect(() => {
    if (!id) return
    return subscribeMemoryEvents(
      () => {},
      (evt: MemoryChangeEvent) => {
        if (evt.op === "upsert" && evt.item?.id === id) setItem(evt.item)
        if (evt.op === "delete" && evt.id === id) setItem(null)
      }
    )
  }, [id])

  return { item, loading }
}

// useMemorySearch debounces `query` (300ms — matches memory-graph.tsx's
// existing search debounce) and searches the FULL backend store via
// GET /v1/memory?q=... (fetchMemorySearch). A client-side filter over
// whatever useMemoryList already fetched can only ever match items already
// loaded in the browser (a capped list fetch); this reaches everything else
// in the store. `kind` optionally scopes the search server-side, mirroring
// the caller's own local list filter (a per-category browsing page
// shouldn't surface matches from every other kind). Callers merge `results`
// with their own local matches, deduped by id — see memory-graph.tsx's
// `filteredItems` for the reference merge pattern this hook factors the
// fetch/debounce half out of.
export function useMemorySearch(query: string, kind?: string) {
  const [results, setResults] = useState<MemoryItem[]>([])
  const [searching, setSearching] = useState(false)
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    if (debounceRef.current) clearTimeout(debounceRef.current)
    const q = query.trim()
    if (!q) { setResults([]); return }
    debounceRef.current = setTimeout(async () => {
      setSearching(true)
      try {
        const items = await fetchMemorySearch(q, 100, undefined, kind)
        setResults(items)
      } catch {
        setResults([])
      } finally {
        setSearching(false)
      }
    }, 300)
    return () => {
      if (debounceRef.current) clearTimeout(debounceRef.current)
    }
  }, [query, kind])

  return { results, searching }
}

export function useMemoryHits() {
  const [hits, setHits] = useState<MemoryHitEvent | null>(null)

  useEffect(() => {
    return subscribeMemoryEvents(setHits)
  }, [])

  const clearHits = useCallback(() => setHits(null), [])
  return { hits, clearHits }
}

export function useMetrics(intervalMs = 30_000) {
  const [metrics, setMetrics] = useState<RuntimeMetrics | null>(null)

  useEffect(() => {
    fetchMetrics().then(setMetrics).catch(() => {})
    const id = setInterval(() => fetchMetrics().then(setMetrics).catch(() => {}), intervalMs)
    return () => clearInterval(id)
  }, [intervalMs])

  return metrics
}
