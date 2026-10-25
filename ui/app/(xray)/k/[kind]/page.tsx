// Category route (server shell). The `[kind]` segment is a UI category id
// (e.g. /k/projects, /k/team), NOT a raw memory kind. The client component
// resolves the id back to a (kind, tag) filter against the generic backend.

import { ALL_KNOWN_CATEGORY_IDS } from "@/lib/category-config"
import { KindPageClient } from "./kind-client"

// Pre-generate routes for every category id any deployment's runtime taxonomy
// might select (ADR-035 item 4) — not just the in-repo default. An id unused
// by a given deployment just renders an empty/unknown category if visited
// directly; the sidebar only links ids the runtime config actually defines.
export function generateStaticParams() {
  return ALL_KNOWN_CATEGORY_IDS.map(id => ({ kind: id }))
}

export default async function KindPage({ params }: { params: Promise<{ kind: string }> }) {
  const { kind } = await params
  return <KindPageClient categoryId={kind} />
}
