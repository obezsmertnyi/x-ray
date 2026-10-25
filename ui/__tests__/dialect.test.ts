/**
 * The UI ships inside X-Ray and speaks X-Ray's own API, /v1, by default; a build
 * with NEXT_PUBLIC_API_PREFIX=/v2 speaks the Triage deployment's instead. These
 * pin what differs between the two, because a UI that gets the wrong dialect
 * fails quietly: the page loads and every request is answered by a login redirect.
 */
import { describe, it, expect, vi, afterEach } from "vitest"
import { API_PREFIX, api, endpoints, fetchTenants, subscribeMemoryEvents } from "../lib/api"

afterEach(() => { vi.unstubAllGlobals() })

describe("the default dialect is X-Ray's own", () => {
  it("is /v1", () => {
    expect(API_PREFIX).toBe("/v1")
    expect(api).toBe("/v1")
  })

  it("uses its own voice route, a server-sent event stream and per-user notes", () => {
    expect(endpoints.voice).toBe("/v1/voice")
    expect(endpoints.events).toEqual({ transport: "sse", path: "/v1/memory/events" })
    expect(endpoints.userNotes).toBe(true)
  })

  it("has no tenants, and does not ask for them", async () => {
    const fetchMock = vi.fn()
    vi.stubGlobal("fetch", fetchMock)
    expect(endpoints.tenants).toBe(false)
    expect(await fetchTenants()).toEqual({ tenants: [], default: "" })
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it("lights a recall from the hit frames the server streams", () => {
    const sources: FakeSource[] = []
    class FakeSource {
      onmessage: ((e: { data: string }) => void) | null = null
      constructor(public url: string) { sources.push(this) }
      close() {}
    }
    vi.stubGlobal("EventSource", FakeSource)
    const onHit = vi.fn()
    const unsubscribe = subscribeMemoryEvents(onHit)
    expect(sources[0].url).toBe("/v1/memory/events")
    // The frame the server publishes for a recall: no envelope, just the event.
    sources[0].onmessage!({ data: JSON.stringify({ itemIds: ["a", "b"], query: "payment retries", kinds: ["operational_pattern", "stable_fact"] }) })
    expect(onHit).toHaveBeenCalledTimes(1)
    expect(onHit.mock.calls[0][0].itemIds).toEqual(["a", "b"])
    unsubscribe()
  })
})
