import type { Metadata } from "next"
import type { ReactNode } from "react"
import { XrayLayoutClient } from "@/components/xray-layout-client"

export const metadata: Metadata = { title: "X-Ray" }

export default function XrayLayout({ children }: { children: ReactNode }) {
  return <XrayLayoutClient>{children}</XrayLayoutClient>
}
