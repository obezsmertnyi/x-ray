"use client"

import { useEffect, useState } from "react"
import { apiFetch, api } from "@/lib/api"

export function useAuth() {
  const [username, setUsername] = useState<string | null>(null)
  const [userID, setUserID] = useState<string | null>(null)

  useEffect(() => {
    apiFetch(`${api}/auth/me`)
      .then((r) => (r.ok ? r.json() : null))
      .then((d) => {
        if (d?.username) setUsername(d.username)
        if (d?.userID) setUserID(d.userID)
      })
      .catch(() => {})
  }, [])

  return { username, userID }
}
