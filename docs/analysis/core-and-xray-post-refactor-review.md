# Post-Refactor Review: `x-ray` (`feat/memory-library`) & `core` (`refactor/store-cells`)

## 1. Executive Summary

Both repositories underwent a major unification and search-quality overhaul:
- **`x-ray` (`feat/memory-library`, `65bd368`)** extracted the full memory engine into `pkg/memstore` (ADR-062: `pkg/memstore`, `pkg/memstore/index`, `pkg/memstore/cellsync`, `pkg/memstore/hub`, `pkg/memstore/memtest`) and implemented every P0–P3 search-quality recommendation from the previous review.
- **`core` (`refactor/store-cells`, `b8c6d232e`)** deleted ~6,000 lines of duplicated memory store, lexical engine, hybrid searcher, cell reconciler, and SSE hub code in favor of `github.com/triageagent-dev/x-ray/v2/pkg/memstore`, updated `corpusmaps/lexical.go` with the 2-level vocabulary index and pigeonhole trigram probing, and completed a 70-item code hygiene sweep (`core-second-review.md`).

All unit test suites in both repositories compile and pass. Below is:
1. A verification matrix of the previous search & architecture recommendations.
2. **7 new edge-case bugs / clean-code findings** introduced during the `pkg/memstore` adapter wiring and private-item migration.

---

## 2. Verification of Previous Recommendations

| ID | Recommendation | Status | Implementation |
| :--- | :--- | :--- | :--- |
| **Arch** | Unify `core` and `x-ray` memory engine | **Done** | Extracted to `x-ray/pkg/memstore`; `core/internal/memory` is now a 170-line vocabulary wrapper. |
| **P0-1** | Strip punctuation at token boundaries in queries & fields | **Done** | `lexFields` strips leading/trailing ASCII punctuation and splits on `-`/`_` across both `NormalizeQuery` and `buildLexCache`. |
| **P0-2** | Strip conversational query stopwords in `KwTokens` | **Done** | `queryStopwords` removes question words/articles/auxiliaries with a full-query fallback if all tokens are stopwords. |
| **P0-3** | Require IDF mass before `lexConfident` skips HNSW | **Done** | `keywordSearch` checks `weightedMatchFraction(top, toks, weights) >= lexConfidentMass (0.75)`. |
| **P1-4** | Weight `topCov` cutoff by token IDF mass | **Done** | `rankAndLimit` spares items via `carriesMass` (`>= 0.35`). |
| **P1-5** | Move `scoreGap` cut after tenant/subject filtering | **Done** | Removed from `CellStore.Search`; `CutAtGap` and `CutAtLargestGap` run on `semKept` in `Searcher.search` before RRF fusion. |
| **P1-6** | Add 3rd RRF channel (`exactMatches`) + tie-breaking | **Done** | `Store.ExactMatches` feeds `rrfFuse` with `preferLex` -> `UpdatedAt` -> `ID` tie-breaking. |
| **P2-7** | Apply provenance (`Origin`) & `Confidence` in `Rerank` | **Done** | `provenanceMultiplier` (`1.10`) and `confidenceMultiplier` (`1.05` / `0.92`) applied in `Rerank`; `collapseBetter` breaks equal-score ties by confidence and origin. |
| **P2-8** | Sort `expandViaLinks` by `link.Weight` | **Done** | Outbound links sorted descending by `link.Weight` before hitting `maxExpandedResults`. |
| **P2-9** | Use `EventDate` in `effectiveTime` for `kind=event` | **Done** | Uses `item.EventDate.Time` when present for `KindEvent`. |
| **P3-10** | Upgrade `corpusmaps/lexical.go` | **Done** | Uses 2-level vocabulary index + pigeonhole trigram probing + query stopword filtering. |

---

## 3. New Findings in the Refactored Code

### Finding 1 (Medium — Bug): Private Items Are Starved in `MemoryStore.Search` and `KeywordSearchForUser` (`x-ray`)

When a caller searches with a `subject` in `x-ray/internal/xray/memory.go` (`MemoryStore.Search`, lines 349–376), two compounding bugs prevent the user's private items (`scope=private`, `user/<subject>`) from being returned:

1. **Hard cap reached before private items are checked:**
   ```go
   results := s.srch().HybridSearch(ctx, query, kind, subject, subject, limit)
   ...
   add := func(v MemoryItem) {
       if _, dup := seen[v.ID]; dup || len(out) >= limit {
           return
       }
       ...
   }
   for _, r := range results {
       ...
       add(v)
   }
   if subject != "" {
       for _, it := range s.eng.KeywordSearchForUser(subject, query, kind, "", limit) {
           if memstore.IsPrivateItem(it) {
               add(viewOf(it))
           }
       }
   }
   ```
   Whenever `HybridSearch` returns `limit` shared items (including low-scoring semantic tail results), `len(out) == limit` before the `KeywordSearchForUser` loop runs, so `add(viewOf(it))` is a no-op and **zero private items can ever be returned**.
2. **`KeywordSearchForUser` ranks the user's private items against all shared items across all subjects:**
   In `MemoryStore.Search` (line 369), `s.eng.KeywordSearchForUser(subject, query, kind, "", limit)` passes `subject = ""` to `Store.keywordSearch` (`pkg/memstore/store.go:1324-1354`). Inside `keywordSearch`, `cands` includes **every shared item in the store across all subjects** plus the user's private items (`own`), and truncates to `limit`. If `limit` shared items from other subjects score slightly higher, the user's private items are truncated before `if memstore.IsPrivateItem(it)` ever sees them.

**Recommendation:**
- Either include `userHash`'s private items directly in `Searcher.search` when `partition` (or a dedicated user parameter) is provided, or add a dedicated `Store.PrivateKeywordSearch(userHash, query, kind string, limit int)` helper that ranks *only* `own` (`s.forUserOwnLocked(userHash, "active", kind, "", "", "")`), and merge/rerank before truncating `out` to `limit`.

---

### Finding 2 (Medium — Bug): `mergeOwnPrivateItems` Uses Raw `strings.Fields` All-Token Substring Matching & Skips Tenant Confinement (`core`)

In `core/internal/voice/tools.go:1834-1877`, `search_memory` merges the caller's private session notes via `mergeOwnPrivateItems`:
```go
toks := strings.Fields(strings.ToLower(query))
...
own := ag.State().Memory().ForUser(userHash, "active", "", "", "", "", 0)
for _, it := range own {
    ...
    haystack := strings.ToLower(it.Title + " " + it.Content)
    allPresent := true
    for _, t := range toks {
        if !strings.Contains(haystack, t) {
            allPresent = false
            break
        }
    }
```
Two issues:
1. **Natural-language queries fail `allPresent`:** `search_memory`'s declaration instructs the voice model: *"formulate a natural-language query describing what you want to find"*. Because `strings.Fields(strings.ToLower(query))` neither strips punctuation nor drops conversational stopwords (`"what"`, `"did"`, `"we"`, `"decide"`, `"session?"`), any natural voice query fails `allPresent` unless the session note literally contains every conversational word and trailing punctuation mark.
2. **Missing tenant check on multi-tenant bound sessions:** `own` is fetched for `userHash` across all tenants without filtering by `core.ItemVisibleToTenant(it, tenant, ag.Tenants())` when `tenant != xmem.PartitionAll`. If the same user hash interacts with two tenants and is in a tenant-scoped session, a private note tagged `tenant/other` can be returned.

**Recommendation:**
- Tokenize the query with `xmem.KwTokens(xmem.NormalizeQuery(query))` (or use `Store`'s lexical scorer over `own`), and filter `own` with `core.ItemVisibleToTenant` when `tenant != xmem.PartitionAll`.

---

### Finding 3 (Medium — Bug): `MemoryStore.matching` Appends Private Items After Shared Items Without Sorting by `UpdatedAt` (`x-ray`)

In `x-ray/internal/xray/memory.go:893-911`:
```go
func (s *MemoryStore) matching(kind, scope, subject string) []MemoryItem {
    var out []MemoryItem
    for _, it := range s.eng.Filtered("active", kind, scope, "", "", 0) {
        if subject != "" && it.Subject != subject {
            continue
        }
        out = append(out, viewOf(it))
    }
    if subject != "" {
        for _, it := range s.eng.ForUser(subject, "active", kind, scope, "", "", 0) {
            if memstore.IsPrivateItem(it) {
                out = append(out, viewOf(it))
            }
        }
    }
    return out
}
```
When `List(kind, scope, subject, limit)` is called with `scope == ""` and `subject != ""`, all private items are placed at the tail of `out` *after* all shared items regardless of `UpdatedAt`, and `items[:limit]` then drops them whenever shared items `>= limit`. Furthermore, `s.eng.ForUser(subject, ...)` already returns both shared items and `subject`'s private items sorted by `UpdatedAt` descending!

**Recommendation:**
- In `matching`, if `subject != ""` and `scope != "private"`, either sort `out` by `UpdatedAt` descending (tie-breaking on `ID`) before returning, or iterate `s.eng.ForUser(subject, "active", kind, scope, "", "", 0)` once and keep items where `memstore.IsPrivateItem(it) || it.Subject == subject`.

---

### Finding 4 (Low-Medium — Bug): `expireStaleItemsLocked` Does Not Remove Lazily Expired Items From `s.kw` and `s.vocab` (`x-ray` & `core`)

When `Store.Expire(id)` (`pkg/memstore/store.go:749-774`) is called explicitly, it marks `item.Status = "stale"` and removes `id` from both lexical indexes:
```go
s.kw.Remove(id)
s.vocab.Remove(id)
```
However, when `expireStaleItemsLocked` (`pkg/memstore/store.go:1206-1214`) lazily expires items past `StaleAfter` during `Items()`, `AllItems()`, `Filtered()`, or `ForUser()`, it updates `s.cells[id]` to `Status = "stale"` without calling `s.kw.Remove(id)` and `s.vocab.Remove(id)`:
```go
for id, item := range s.cells {
    if item.Status == "active" && item.StaleAfter != nil && now.After(item.StaleAfter.Time) {
        item.Status = "stale"
        s.cells[id] = item
        expired++
    }
}
```
As a result, lazily expired items remain in `vocabIndex` (skewing `wordDocFreq` IDF weights and returning stale candidate IDs in `CandidateIDsForTokens`) and `trigramIndex` until `Delete` or `PruneExcept` is called.

**Recommendation:**
- Call `s.kw.Remove(id)` and `s.vocab.Remove(id)` inside the `expireStaleItemsLocked` loop when transitioning an item from `"active"` to `"stale"`.

---

### Finding 5 (Low-Medium — Bug): `Store.CloneInto` Leaves `out.vocab` Non-Nil but Empty, Breaking `KeywordSearch` on Cloned Stores (`x-ray` & `core`)

In `Store.CloneInto` (`pkg/memstore/store.go:1594-1606`):
```go
func (s *Store) CloneInto(l Locker, cfg Config) *Store {
    out := NewStore(WithLocker(l), WithConfig(cfg))
    s.rlock()
    defer s.runlock()
    out.cells = make(map[string]Item, len(s.cells))
    for k, v := range s.cells {
        out.cells[k] = v.Clone()
    }
    ...
```
Because `NewStore` initializes `out.vocab = newVocabIndex()` (non-nil, 0 indexed documents), and `keywordSearch` (`store.go:1368-1381`) checks `if vocab != nil && len(toks) > 0 && len(cands) > 0`, calling `KeywordSearch` on a cloned store intersects `cands` against an empty `vocabIndex` and **always returns 0 hits**.
Additionally, in `core/internal/core/livestate.go:2217`, `w.mem.CloneInto(&clone.mu, clone.memoryConfig())` does not copy `s.cfg.Vocabulary` when `cfg.Vocabulary.IsEmpty()`.

**Recommendation:**
- In `CloneInto`:
  1. Inherit `s.cfg.Vocabulary` when `cfg.Vocabulary.IsEmpty()`.
  2. Set `out.vocab = nil` and `out.kw = nil` (or rebuild them on demand) so any `KeywordSearch` on a cloned store falls back to linear candidate scoring instead of filtering against an empty index.

---

### Finding 6 (Low — Bug): `Store.GetFold` Bypasses `s.readOne(&clone)` (`x-ray` & `core`)

`Store.Get(id)` (`pkg/memstore/store.go:521-531`) calls `s.readOne(&clone)` to invoke `Config.Read` (which in `core` runs `guardTenantStatusItemsLocked` to scrub stale closed-concern mentions from tenant status notes).
`Store.GetFold(id, foldScan)` (`pkg/memstore/store.go:537-555`) returns `item.Clone()` directly without calling `s.readOne(&clone)`. Since `Searcher.exactIDMatch` uses `GetFold`, an exact-ID search hit on a status note bypasses `Config.Read`.

**Recommendation:**
- Call `s.readOne(&clone)` in both branches of `Store.GetFold`.

---

### Finding 7 (Low — Performance): `MemoryStore.Refute` Clones and Sorts the Entire Store Before Refuting (`x-ray`)

In `x-ray/internal/xray/memory.go:744-751`:
```go
func (s *MemoryStore) Refute(id, reason string) (MemoryItem, []string, string) {
    before := make(map[string]memstore.Item)
    for _, it := range s.eng.AllItems() {
        before[it.ID] = it
    }
    refuted, cascaded, validErr := s.eng.Refute(id, reason)
```
`s.eng.AllItems()` clones and sorts every cell in the store solely so lines 759–765 can look up `before[cid]` for the few `cascaded` IDs that `s.eng.Refute` expired. However, `s.eng.Refute` only marks cascaded items `Status = "stale"` in `s.eng` (it does not delete them), so `s.eng.Get(cid)` after `s.eng.Refute` still returns each cascaded item with its `Scope` and `Tags` intact.

**Recommendation:**
- Remove the `s.eng.AllItems()` snapshot in `Refute` and look up each `cid` in `cascaded` with `s.eng.Get(cid)`.
