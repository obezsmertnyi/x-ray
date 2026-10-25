"use client"

import { useEffect, useState } from "react"
import { useRouter } from "next/navigation"
import type { ReactNode } from "react"
import { useMobile } from "@/hooks/use-mobile"
import { MobileSwipeLayout } from "@/components/mobile-swipe-layout"
import { apiFetch, api } from "@/lib/api"
import { useTenant } from "@/components/tenant-provider"

export function XrayLayoutClient({ children }: { children: ReactNode }) {
  const mobile = useMobile()
  const router = useRouter()
  const [ready, setReady] = useState(false)
  // Every child fetches on mount, and the tenant is only known once
  // /agent/tenants answers. Rendering before then would fire the whole first
  // page of requests unscoped — which does not fail, it quietly returns every
  // tenant the caller may see, so the mistake would be invisible.
  const { loading: tenantLoading } = useTenant()

  useEffect(() => {
    apiFetch(`${api}/auth/me`).then((res) => {
      if (res.status === 401) {
        router.replace("/login")
      } else {
        setReady(true)
      }
    }).catch(() => {
      router.replace("/login")
    })
  }, [router])

  if (!ready || tenantLoading) return null

  if (mobile) {
    return (
      <div className="h-dvh">
        <MobileSwipeLayout />
      </div>
    )
  }

  return (
    <main className="h-dvh overflow-hidden">
      {children}
    </main>
  )
}
