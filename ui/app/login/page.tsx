"use client"

import { useEffect, useState } from "react"
import { GhostGraph } from "@/components/ghost-graph"
import { apiFetch, beginLogin, api } from "@/lib/api"
import { asset } from "@/lib/utils"

export default function LoginPage() {
  const [providers, setProviders] = useState<{ github?: boolean; azure?: boolean } | null>(null)

  useEffect(() => {
    apiFetch(`${api}/auth/providers`)
      .then(r => r.ok ? r.json() : {})
      .then(setProviders)
      .catch(() => setProviders({ github: true }))
  }, [])

  // Stamp the pending-login marker before leaving, so /auth/callback can tell
  // a token this tab asked for from one an attacker put in a link.
  const start = (provider: string) => (e: React.MouseEvent<HTMLAnchorElement>) => {
    e.preventDefault()
    window.location.assign(beginLogin(provider))
  }

  return (
    <div className="relative flex h-dvh items-center justify-center bg-black overflow-hidden">
      <GhostGraph />
      <div className="relative z-10 flex flex-col items-center gap-6">
        <img src={asset("/logo.svg")} alt="X-Ray" className="h-12 w-12 opacity-80" />
        <h1 className="text-xl font-semibold text-white tracking-tight">X-Ray</h1>

        <div className="flex flex-col gap-3 w-full">
          {providers?.github && (
            <a
              href={`${api}/auth/github`}
              onClick={start("github")}
              className="flex items-center gap-3 rounded-lg border border-white/20 bg-white/5 px-6 py-3 text-sm font-medium text-white transition-colors hover:bg-white/10"
            >
              <svg viewBox="0 0 24 24" className="h-5 w-5 fill-current flex-none" aria-hidden="true">
                <path d="M12 0C5.37 0 0 5.37 0 12c0 5.31 3.435 9.795 8.205 11.385.6.105.825-.255.825-.57 0-.285-.015-1.23-.015-2.235-3.015.555-3.795-.735-4.035-1.41-.135-.345-.72-1.41-1.23-1.695-.42-.225-1.02-.78-.015-.795.945-.015 1.62.87 1.845 1.23 1.08 1.815 2.805 1.305 3.495.99.105-.78.42-1.305.765-1.605-2.67-.3-5.46-1.335-5.46-5.925 0-1.305.465-2.385 1.23-3.225-.12-.3-.54-1.53.12-3.18 0 0 1.005-.315 3.3 1.23.96-.27 1.98-.405 3-.405s2.04.135 3 .405c2.295-1.56 3.3-1.23 3.3-1.23.66 1.65.24 2.88.12 3.18.765.84 1.23 1.905 1.23 3.225 0 4.605-2.805 5.625-5.475 5.925.435.375.81 1.095.81 2.22 0 1.605-.015 2.895-.015 3.3 0 .315.225.69.825.57A12.02 12.02 0 0 0 24 12c0-6.63-5.37-12-12-12z" />
              </svg>
              Sign in with GitHub
            </a>
          )}

          {providers?.azure && (
            <a
              href={`${api}/auth/entra`}
              onClick={start("entra")}
              className="flex items-center gap-3 rounded-lg border border-white/20 bg-white/5 px-6 py-3 text-sm font-medium text-white transition-colors hover:bg-white/10"
            >
              <svg viewBox="0 0 24 24" className="h-5 w-5 flex-none" aria-hidden="true">
                <path d="M11.5 0L0 20.8h4.3l3.6-6.3 4.4 4.9-2.7 4.6H24L11.5 0zm.1 6.2l5.8 10H9.2l2.4-4.3-2.2-2.5 2.2-3.2z" fill="currentColor" />
              </svg>
              Sign in with Microsoft
            </a>
          )}
        </div>

        <p className="text-xs text-white/30">Access restricted to authorised members</p>
      </div>
    </div>
  )
}
