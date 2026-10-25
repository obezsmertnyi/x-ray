"use client"

import { useSyncExternalStore } from "react"
import { CATEGORIES, subscribeCategories, type Category } from "@/lib/category-config"

/**
 * Subscribes a component to CATEGORIES changes. Components that read
 * CATEGORIES to build a nav/tab list must call this (instead of reading
 * CATEGORIES directly at module-eval time) so they re-render once
 * loadRuntimeCategories() resolves the per-deployment taxonomy (ADR-035 item 4).
 */
export function useCategories(): Category[] {
  return useSyncExternalStore(subscribeCategories, () => CATEGORIES, () => CATEGORIES)
}
