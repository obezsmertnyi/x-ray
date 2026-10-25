"use client"

import { Fragment, createContext, useCallback, useContext, useEffect, useMemo, useState } from "react"
import { fetchTenants, setApiTenant, setApiDefaultTenant } from "@/lib/api"

/**
 * The tenant currently in view, shared by the graph, the lists and the HUD.
 *
 * The server already scopes every response to the caller's grant, so this is
 * not an access control — it is the choice of WHICH tenant to look at inside
 * that grant. Without it a user entitled to several sees them blended into one
 * graph with nothing to say so: the counts are plausible, the nodes render,
 * and only the totals give it away.
 *
 * The list comes from `GET /agent/tenants`, which the controller filters to the
 * caller's scope and returns alongside the deployment's default — so a bound
 * user's switcher has a single entry and the client never filters the list
 * itself. Mirrors triage-ui's TenantProvider so the two clients cannot
 * disagree about what a tenant selection means.
 */
interface TenantContextValue {
  /** Tenants this user may view. Empty on a single-tenant deployment. */
  tenants: string[]
  /** The tenant in view; "" until loaded, or where the API has no tenants. */
  tenant: string
  setTenant: (t: string) => void
  /** True when there is more than one tenant to choose between. */
  canSwitch: boolean
  loading: boolean
}

const TenantContext = createContext<TenantContextValue>({
  tenants: [],
  tenant: "",
  setTenant: () => {},
  canSwitch: false,
  loading: true,
})

/** Remembers the choice across reloads, per tab — same scoping as the token. */
const STORAGE_KEY = "xray.tenant"

function remembered(): string | null {
  try {
    return sessionStorage.getItem(STORAGE_KEY)
  } catch {
    return null // storage blocked (private mode / embedded)
  }
}

export function TenantProvider({ children }: { children: React.ReactNode }) {
  const [tenants, setTenants] = useState<string[]>([])
  const [tenant, setTenantState] = useState("")
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false
    fetchTenants()
      .then(({ tenants: names, default: def }) => {
        if (cancelled) return
        setTenants(names)
        // Restore the last choice, but only if it is still one this user may
        // view: a remembered tenant otherwise survives a permission change and
        // every request 403s with no obvious cause.
        const prev = remembered()
        const initial = prev && names.includes(prev) ? prev : def || names[0] || ""
        setTenantState(initial)
        setApiTenant(initial)
        setApiDefaultTenant(def)
      })
      .catch(() => {
        // Single-tenant deployment, or an API without the endpoint: no
        // selector, and every request omits ?tenant= exactly as it used to.
        if (!cancelled) setTenants([])
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })

    return () => {
      cancelled = true
    }
  }, [])

  const setTenant = useCallback((t: string) => {
    setTenantState(t)
    setApiTenant(t)
    try {
      sessionStorage.setItem(STORAGE_KEY, t)
    } catch {
      // Storage blocked: the choice still applies for this page's lifetime.
    }
  }, [])

  const value = useMemo<TenantContextValue>(
    () => ({ tenants, tenant, setTenant, canSwitch: tenants.length > 1, loading }),
    [tenants, tenant, setTenant, loading],
  )

  return (
    <TenantContext.Provider value={value}>
      {/*
        Remount everything below when the tenant changes.

        Every data hook here fetches from a mount effect with its own
        dependency list — [key], [id, kind], [query, kind], and the graph's
        summary on [] — and none of them names the tenant. Threading it into
        each would work until the next hook forgot to, which is the exact
        failure this design already avoids at the fetch layer by scoping inside
        apiFetch. A key is the one place that cannot be forgotten.

        It is also the honest semantic: switching tenant means a different
        store, not a filter over the current one, so keeping the previous
        tenant's nodes and panels on screen while new data arrives would be
        showing two tenants at once — the very thing this fixes.
      */}
      <Fragment key={tenant || "unscoped"}>{children}</Fragment>
    </TenantContext.Provider>
  )
}

export function useTenant() {
  return useContext(TenantContext)
}
