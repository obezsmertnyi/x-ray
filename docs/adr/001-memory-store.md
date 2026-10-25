# ADR-001: Memory Store — Two Modes, One Interface

**Status:** Implemented  
## What it is

`MemoryStore` (`internal/xray/memory.go`) is the central in-memory store of all `MemoryItem` records. It is the single source of truth for every read and write at runtime. All items live in a `map[string]*MemoryItem` protected by a `sync.RWMutex`.

## Two operating modes

### Keyword mode (no embedder)
- Activated when no `--embedding-endpoint` or `--gcp-project` is set.
- `Search()` → `keywordSearch()`: iterates all items, matches on `Title` and `Content` substrings.
- `SeedFromDoc()` is still called by `CellSyncer` but skips HNSW indexing.
- `seedItemDirect()` loads CRs straight into the items map.

### Semantic mode (embedder configured)
- Activated when `NewMemoryStoreWithEmbedder()` is called.
- `Search()` → `semanticSearch()`: calls `MemoryCellStore.SearchSubject()` or `Search()` on the HNSW index, then resolves IDs back to `MemoryItem` structs.
- `Upsert()` calls `cells.Index()` to embed and index the item; sets `status="indexed"` on success.
- `SeedFromDoc()` calls `cells.SeedFromEmbedding()` with a pre-existing vector from the CR (no API call needed).

## Key behaviours

### `Search(query, kind, subject, limit)`
- Returns `nil` immediately if `query == ""`. **Always use `List()` for filter-only queries with no query string.**
- `system_configuration` items are excluded unless `kind="system_configuration"` is explicitly passed.
- Subject filter: substring match in keyword mode, exact match in `List()`.

### `List(kind, scope, subject, limit)`
- Does not require a query string — filters by kind/scope/subject only.
- `system_configuration` items are excluded unless `kind="system_configuration"` is explicitly passed.
- Subject filter: **exact match** (not substring).

### `Upsert()`
- If `id` is empty, generates a random 8-byte hex ID (`newID()`).
- Creates or updates the item in the map, then asynchronously embeds (if embedder present).
- Does **not** write to Kubernetes — that is `CellSyncer.Persist()`'s job.
- Thumbnail images passed as `imageData` are stored on the item but **stripped** before embedding (`stripThumbnail()`).

### `AllItems(subject)`
- Returns all active items for a subject, **excluding** `system_configuration`.
- Used by MCP `get_knowledge_context` and voice `get_memory` tool.

### `system_configuration` kind
- Reserved — never appears in `Search`, `List`, or `AllItems` results unless explicitly requested.
- Read only via `List("system_configuration", "", subject, 1)`.
- Controls voice session config: system prompt, enabled tools, voice name, model.

## Item status lifecycle
- `"active"` — created or updated, not yet embedded.
- `"indexed"` — embedded successfully into HNSW.
- `"stale"` / `"superseded"` — skipped during CR seeding, never loaded.
- Both `"active"` and `"indexed"` pass the `isActive()` check.
