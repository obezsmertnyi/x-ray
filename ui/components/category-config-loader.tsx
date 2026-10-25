"use client"

import { useEffect } from "react"
import { loadRuntimeCategories } from "@/lib/category-config"

// Fires once on mount to fetch the per-deployment category taxonomy
// (ADR-035 item 4) before most of the app renders. Renders nothing.
export function CategoryConfigLoader() {
  useEffect(() => {
    loadRuntimeCategories()
  }, [])
  return null
}
