# ADR-019: REST API Reference

**Status:** Implemented  
## Context

X-Ray exposes a REST API on `:8080` for the web UI, mobile clients, and external
integrations. This ADR documents all endpoints, request/response shapes, auth rules,
and the SSE event stream so developers do not need to read the source.

The server also exposes an OpenAPI spec at `/docs` (Huma auto-generated).

## Base URL

```
https://<host>/v1
```

## Authentication

Most `/v1/*` routes require a bearer token obtained from `POST /v1/mobile/auth`.

Pass it on every authenticated request:

```
Authorization: Bearer <token>
```

**Public endpoints (no token required):**

| Path | Note |
|------|------|
| `POST /v1/mobile/auth` | PIN login |
| `POST /v1/mobile/auth/validate` | Token check |
| `GET /v1/version` | Build info |
| `GET /v1/voice` | WebSocket upgrade |
| `GET /v1/memory*` | All memory routes |
| `GET /healthz` | Health check |

---

## Endpoints

### Auth

#### `POST /v1/mobile/auth`

Authenticate with a PIN and receive a bearer token (24 h expiry).

**Request**
```json
{ "pin": "1234" }
```

**Response `200`**
```json
{
  "valid": true,
  "username": "alice",
  "token": "<bearer-token>"
}
```

**Response `401`** — invalid PIN  
**Response `429`** — rate limited (too many attempts from the same IP)

---

#### `POST /v1/mobile/auth/validate`

Check whether a token is still valid.

**Request**
```json
{ "token": "<bearer-token>" }
```

**Response `200`**
```json
{
  "valid": true,
  "username": "alice"
}
```

`valid: false` when the token is expired or unknown (never a 4xx).

---

### Memory

#### `GET /v1/memory`

List memory items with optional filters.

**Query parameters**

| Param | Type | Description |
|-------|------|-------------|
| `kind` | string | Filter by kind (`stable_fact`, `recent_change`, `system_identity`, `event`, `agent_note`) |
| `scope` | string | Filter by scope (`public`, `private`) — ADR-037 |
| `subject` | string | Filter by subject |
| `limit` | int | Max results (default 200) |
| `q` | string | Keyword search — overrides list when set |

**Response `200`**
```json
{
  "items": [ /* MemoryItem[] */ ],
  "total": 42
}
```

When `q` is set, results are ranked by keyword match rather than insertion order.

---

#### `GET /v1/memory/summary`

Lightweight index — returns `id`, `title`, `kind`, and `tags` only. No `content` or `imageData`. Useful for building navigation or the Voronoi graph without paying the full payload cost.

**Query parameters**

| Param | Type | Description |
|-------|------|-------------|
| `kind` | string | Filter by kind |
| `scope` | string | Filter by scope |
| `subject` | string | Filter by subject |
| `top` | int | Max results (default 200) |

**Response `200`**
```json
{
  "items": [
    { "id": "abc", "title": "X-Ray Core", "kind": "system_identity", "tags": ["project"] }
  ],
  "total": 1
}
```

---

#### `GET /v1/memory/{id}`

Fetch a single memory item by ID.

**Response `200`** — full `MemoryItem` object  
**Response `404`** — not found

When an item has an image, the response includes `"imageURL": "/v1/memory/{id}/image"`.

---

#### `GET /v1/memory/{id}/image`

Serve the image attached to a memory item as its native MIME type (usually `image/jpeg`).

**Response `200`** — binary image body with `Content-Type` and `Cache-Control: public, max-age=86400`  
**Response `404`** — item not found or has no image

---

#### `GET /v1/memory/events`

Server-Sent Events stream. Subscribe to receive real-time memory activity. The connection stays open; the server pushes events as they occur.

**Event types**

`MemoryHitEvent` — fired when a voice tool searches memory (used by X-Ray to highlight graph nodes):
```json
{
  "type": "memory_hit",
  "ids": ["abc", "def"],
  "query": "search text"
}
```

`MemoryChangeEvent` — fired on upsert or delete:
```json
{
  "type": "memory_change",
  "op": "upsert",
  "item": { /* MemoryItem */ }
}
```
```json
{
  "type": "memory_change",
  "op": "delete",
  "id": "abc"
}
```

**Usage**
```js
const es = new EventSource("/v1/memory/events")
es.onmessage = e => {
  const ev = JSON.parse(e.data)
  if (ev.type === "memory_hit") { /* highlight nodes */ }
  if (ev.type === "memory_change") { /* refresh UI */ }
}
```

---

#### `GET /v1/memory/metrics`

Internal diagnostics — search latency, embedding stats, item count, and voice session metrics. Intended for ops/debugging.

**Response `200`**
```json
{
  "search": { "calls": 12, "avg_ms": 3.2 },
  "embed":  { "calls": 5,  "errors": 0 },
  "items":  88,
  "voice":  { "active": 1, "total": 7 }
}
```

---

### Voice

#### `GET /v1/voice`

WebSocket upgrade. Opens a bidirectional audio session with Gemini Live.

**Protocol** — binary WebSocket frames carrying JSON-wrapped PCM audio. See `internal/xray/voice.go` for the `VoiceBrowserMessage` / `VoiceServerMessage` shapes.

The server loads the system prompt, enabled tools, voice name, and model from the `system_configuration` memory cell (subject `xray`) at session start — no restart needed to update.

---

### Misc

#### `GET /v1/version`

```json
{
  "version": "v1.15.18-d2f6228",
  "commit":  "d2f6228",
  "buildDate": "2026-05-18T10:47:00Z"
}
```

#### `GET /healthz`

Returns `200 ok` when the server is up.

---

## MemoryItem Schema

```jsonc
{
  "id":         "string",          // unique slug
  "kind":       "stable_fact | recent_change | system_identity | event | agent_note",
  "scope":      "public | private",         // ADR-037 privacy axis
  "subject":    "string",          // author or subject tag
  "title":      "string",
  "content":    "string",          // Markdown body
  "confidence": "high | medium | low",
  "tags":       ["string"],
  "links":      ["string"],
  "eventDate":  "RFC3339",         // kind=event only
  "imageURL":   "string",          // set when item has an image; use GET /v1/memory/{id}/image
  "createdAt":  "RFC3339",
  "updatedAt":  "RFC3339"
}
```

## Kind × UI Page Mapping

| `kind` | `tags` | UI page |
|--------|--------|---------|
| `system_identity` | `project` | Projects |
| `system_identity` | `contributor` | Team |
| `recent_change` | — | News |
| `stable_fact` | — | Blog |
| `event` | — | Events |
| `agent_note` | — | hidden (session-scoped, not persisted) |
| `system_configuration` | — | hidden (voice config only) |

