import { describe, it, expect, beforeEach, vi, afterEach } from "vitest"

import { apiFetch, setApiTenant, getApiTenant, setApiDefaultTenant, itemVisibleToTenant, subscribeMemoryEvents } from "../lib/api"

/**
 * Tenant scoping is applied inside apiFetch rather than at each call site, so
 * these assert the choke point: which URLs get ?tenant= and which are left
 * alone. The API's own contract is that an EMPTY tenant means "every tenant
 * the caller may read" — so a missing parameter does not fail, it silently
 * widens the response, which is exactly the bug worth pinning down.
 */
describe("tenant scoping", () => {
  let fetchMock: ReturnType<typeof vi.fn>

  beforeEach(() => {
    fetchMock = vi.fn().mockResolvedValue(new Response("{}", { status: 200 }))
    vi.stubGlobal("fetch", fetchMock)
    setApiTenant("")
  })

  afterEach(() => {
    setApiTenant("")
    vi.unstubAllGlobals()
  })

  const urlOf = () => String(fetchMock.mock.calls[0][0])

  it("sends no tenant parameter until one is selected", async () => {
    await apiFetch("/v1/memory")
    expect(urlOf()).not.toContain("tenant=")
  })

  it("scopes memory reads once a tenant is selected", async () => {
    setApiTenant("alpha")
    await apiFetch("/v1/memory")
    expect(urlOf()).toContain("tenant=alpha")
  })

  it("keeps existing query parameters when it adds the tenant", async () => {
    setApiTenant("beta")
    await apiFetch("/v1/memory?kind=fact&limit=10")
    const url = urlOf()
    expect(url).toContain("kind=fact")
    expect(url).toContain("limit=10")
    expect(url).toContain("tenant=beta")
  })

  it("leaves routes that take no tenant alone", async () => {
    setApiTenant("alpha")
    for (const path of ["/v1/auth/me", "/v1/categories", "/v1/agent/tenants"]) {
      fetchMock.mockClear()
      await apiFetch(path)
      // "tenant=" not "tenant": /agent/tenants carries the word in its PATH,
      // and the assertion is about the query parameter.
      expect(urlOf()).not.toContain("tenant=")
    }
  })

  it("does not override a tenant the caller set explicitly", async () => {
    setApiTenant("alpha")
    await apiFetch("/v1/memory?tenant=triage")
    const url = urlOf()
    expect(url).toContain("tenant=triage")
    expect(url).not.toContain("alpha")
  })

  it("exposes the selection for callers that build their own URLs", async () => {
    // The voice WebSocket is not built through apiFetch — it needs the raw
    // value to pin the session's tenant binding.
    setApiTenant("gcp")
    expect(getApiTenant()).toBe("gcp")
  })
})

/**
 * The live stream is the one read path apiFetch cannot scope: the server
 * filters it to the caller's grants, not to the selected tenant. These pin the
 * client-side mirror of the server's visibility rule and its use at the fan-out.
 */
describe("tenant visibility of live changes", () => {
  afterEach(() => {
    setApiTenant("")
    setApiDefaultTenant("")
  })

  it("keeps a tagged item only for its own tenant", () => {
    expect(itemVisibleToTenant({ kind: "stable_fact", tags: ["tenant/alpha"] }, "alpha", "")).toBe(true)
    expect(itemVisibleToTenant({ kind: "stable_fact", tags: ["x", "tenant/gcp"] }, "alpha", "")).toBe(false)
  })

  it("treats an untagged item of a shared kind as global", () => {
    expect(itemVisibleToTenant({ kind: "stable_fact" }, "alpha", "")).toBe(true)
    expect(itemVisibleToTenant({ kind: "stable_fact", tags: ["team/x"] }, "alpha", "triage")).toBe(true)
  })

  it("gives an untagged item of a tenant-scoped kind to the default tenant only", () => {
    expect(itemVisibleToTenant({ kind: "event" }, "triage", "triage")).toBe(true)
    expect(itemVisibleToTenant({ kind: "event" }, "alpha", "triage")).toBe(false)
    // No default configured: visible to nobody, never to "whoever also resolves empty".
    expect(itemVisibleToTenant({ kind: "active_concern" }, "alpha", "")).toBe(false)
  })

  it("applies no narrowing while no tenant is selected", () => {
    expect(itemVisibleToTenant({ kind: "event", tags: ["tenant/gcp"] }, "", "")).toBe(true)
  })

  it("drops another tenant's upsert at the fan-out and passes its own", () => {
    // Fake event source: captures the instance so the test can inject frames.
    const sources: FakeSource[] = []
    class FakeSource {
      onmessage: ((e: { data: string }) => void) | null = null
      constructor(public url: string) { sources.push(this) }
      close() {}
    }
    vi.stubGlobal("EventSource", FakeSource)
    setApiTenant("alpha")
    setApiDefaultTenant("triage")

    const onChange = vi.fn()
    const unsubscribe = subscribeMemoryEvents(() => {}, onChange)
    const es = sources[0]
    expect(es.url).toContain("/v1/memory/events")

    const frame = (tags: string[]) => JSON.stringify(
      { op: "upsert", item: { id: "i-" + tags.join(","), kind: "stable_fact", scope: "public", title: "t", content: "", status: "active", createdAt: "", updatedAt: "", tags } },
    )
    es.onmessage!({ data: frame(["tenant/gcp"]) })
    expect(onChange).not.toHaveBeenCalled()
    es.onmessage!({ data: frame(["tenant/alpha"]) })
    expect(onChange).toHaveBeenCalledTimes(1)
    // A delete carries only an id: it passes, since an unknown id is a no-op.
    es.onmessage!({ data: JSON.stringify({ op: "delete", id: "gone" }) })
    expect(onChange).toHaveBeenCalledTimes(2)

    unsubscribe()
  })
})
