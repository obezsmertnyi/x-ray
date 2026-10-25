"use client"

import { useEffect, useState } from "react"

const MOBILE_WIDTH = 768

// Returns true when the viewport is narrow OR the hostname starts with "mobile-"
// (e.g. mobile-xray.example.com). Re-evaluates on window resize.
export function useMobile(): boolean {
  const [mobile, setMobile] = useState(false)

  useEffect(() => {
    const hostForced = window.location.hostname.startsWith("mobile-")

    if (hostForced) {
      setMobile(true)
      return
    }

    const check = () => setMobile(window.innerWidth < MOBILE_WIDTH)
    check()

    const mq = window.matchMedia(`(max-width: ${MOBILE_WIDTH - 1}px)`)
    mq.addEventListener("change", check)
    return () => mq.removeEventListener("change", check)
  }, [])

  return mobile
}
