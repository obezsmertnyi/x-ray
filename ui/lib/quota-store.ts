"use client"

import { useSyncExternalStore } from "react"

// Live per-user token quota pushed over the voice WebSocket during a session.
// The voice hook writes here each turn; StatsHud subscribes and prefers this
// over the slower polled /v1/memory/metrics value while a session is active.
// TokenBreakdown is the current session's cumulative per-modality token split,
// mirroring the separate Vertex Live billing SKUs (audio dominates voice cost).
export interface TokenBreakdown {
  inputAudio: number
  inputText: number
  outputAudio: number
  outputText: number
  cached: number
}

export interface LiveQuota {
  used: number
  limit: number
  remaining: number
  breakdown?: TokenBreakdown
}

let current: LiveQuota | null = null
const listeners = new Set<() => void>()

export function setLiveQuota(q: LiveQuota | null) {
  current = q
  listeners.forEach(l => l())
}

function subscribe(cb: () => void) {
  listeners.add(cb)
  return () => listeners.delete(cb)
}

function getSnapshot() {
  return current
}

export function useLiveQuota(): LiveQuota | null {
  return useSyncExternalStore(subscribe, getSnapshot, () => null)
}
