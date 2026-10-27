# X-Ray Memory Search Review & Optimization Recommendations (X-Ray + Core)

## 1. Architecture Overview & Feature Drift Between Repos

Across `x-ray` and `core`, there are **two hybrid search families** sharing the ADR-030 lineage:

1. **Authored / Operational Memory Search**:
   - **In `x-ray`**: [`internal/xray/memory.go`](../../internal/xray/memory.go), [`internal/xray/lexical.go`](../../internal/xray/lexical.go), [`internal/xray/kwindex.go`](../../internal/xray/kwindex.go), [`internal/xray/memory_rerank.go`](../../internal/xray/memory_rerank.go), [`internal/xray/memory_dedup.go`](../../internal/xray/memory_dedup.go), [`internal/vectorstore/memorycellstore.go`](../../internal/vectorstore/memorycellstore.go), and [`pkg/memory/store.go`](../../pkg/memory/store.go).
   - **In `core`**: `internal/memory/search.go`, `internal/memory/lexical.go`, `internal/memory/kwindex.go`, `internal/memory/kwvocab.go`, and `internal/vectorstore/memorycellstore.go`.
2. **Corpus / Recall Map Search (`codegraph/index`)**:
   - **In `x-ray`**: [`internal/codegraph/index/index.go`](../../internal/codegraph/index/index.go) and [`internal/codegraph/index/lexical.go`](../../internal/codegraph/index/lexical.go).
   - **In `core`**: `internal/corpusmaps/corpus.go` and `internal/corpusmaps/lexical.go`.

| Capability | `x-ray/internal/xray` | `x-ray/internal/codegraph/index` | `core/internal/memory` | `core/internal/corpusmaps` |
| :--- | :---: | :---: | :---: | :---: |
| Punctuation stripping on tokenization (`lexFields`) | No (`strings.Fields`) | Yes (`lexFields`) | No (`strings.Fields`) | Yes (`lexFields`) |
| Conversational query stopword filtering (`what`, `is`, `with`, `the`) | Only 6 UI words | 30+ stopwords | None | 30+ stopwords |
| Exact-Title 3rd RRF channel (`searchExactTitle`) | No | Yes | No | No |
| Adaptive score-gap cliff (`CutAtLargestGap`) | Fixed `0.02` gap | `CutAtLargestGap` | Fixed `0.02` gap | Top-k + Z-score |
| Precomputed `lexicalCache` (`wordTokens` + rune lengths) | Recomputed every query | N/A | Indexed on write | N/A |
| Inverted `vocabIndex` pre-filter (`wordsByLen` + `joinsByLen`) | Full O(N) scan | 2-level `gramWords` | `vocabIndex` | 1-level `grams` |
| Bounded-DP `editDistanceWithin` (ASCII fast path + row-min bailout) | Yes | Yes | Yes | Full rune DP |
| Provenance (`Origin`) & `Confidence` in search rerank | Dedup only | N/A | Dedup only | N/A |

---

## 2. Search Quality & Relevance Findings ("Better Results")

### 2.1. Punctuation Gluing & Conversational Stopword Blindness (High Impact — Both `x-ray` & `core`)
- **Where**:
  - [`x-ray/internal/xray/memory.go`](../../internal/xray/memory.go#L742-L800) (`normalizeQuery`, `kwTokens`)
  - `core/internal/memory/lexical.go` (`NormalizeQuery`, `KwTokens`)
- **Problem 1 (Punctuation Gluing)**:
  - `normalizeQuery` / `NormalizeQuery` only replaces `_` and `-` with spaces, then splits with `strings.Fields`. Unlike [`dedupTokens`](../../internal/xray/memory_dedup.go#L35-L59) or [`lexFields`](../../internal/codegraph/index/lexical.go#L221-L246), it **does not strip punctuation** (`?`, `.`, `,`, `:`, `;`, `'`, `"`, `()`, `[]`, `/`, `` ` ``).
  - **Consequence**:
    1. In a query like `"what is wrong with payment-api?"`, the last token is `"api?"` (4 chars). `strings.Contains(nt, "api?")` **fails** against a title `"payment-api outage"` (normalized to `"payment api outage"`). It only survives via 1-edit fuzzy match, scoring `wTokFuzzyTle (60)` instead of `wTokExactTle (100)`.
    2. For a 2-letter token with punctuation like `"db?"` or `"ui,"`, `strings.Contains` fails against `"db"`, and `editDistance("db?", "db")` is 1, which corrupts short-token exactness.
    3. `docFreqs` in [`x-ray/internal/xray/lexical.go`](../../internal/xray/lexical.go#L88) checks whole words from `strings.Fields(strings.ToLower(content))`. A word followed by a period or comma (`"bigquery."`, `"bigquery,"`) is **not counted** in `df[t]` for `"bigquery"`, artificially lowering `df` and distorting IDF weights.
- **Problem 2 (Conversational Stopword Blindness + False-Positive `lexConfident` Short-Circuit)**:
  - `x-ray/internal/xray/memory.go` only strips 6 UI category words (`project`, `contributor`, `blog`, `news`, `event`, `events`), and `core/internal/memory/lexical.go` strips **zero** stopwords.
  - While ADR-030 Step 2 added IDF weighting (`idfWeight`) and `wCoverageMassThreshold (0.35)` as a patch for conversational queries, keeping conversational filler tokens (`what`, `is`, `the`, `with`, `how`, `why`, `when`, `where`, `who`, `does`, `for`, `in`, `on`, `to`, `of`) in `toks` causes two remaining relevance bugs:
    1. **False `lexConfident` suppresses Semantic Search**: In `x-ray` (where `lexicalShortCircuit = true` by default in `NewMemoryStore` / `pkg/memory`) and in `core`'s `HybridSearch`, `lexConfident` is purely count-based (`matched >= len(toks)-1`). For `"what is the status of auth"`, a verbose prose cell containing `"what"`, `"is"`, `"the"`, `"status"`, `"of"` (5 of 6 tokens, missing `"auth"`) satisfies `matched = 5 >= need (5)`. When it lands at `items[0]`, `lexConfident` evaluates `items[0]`'s `matched >= need` → `true` and **short-circuits semantic search completely**, returning an unrelated cell.
    2. **Coverage Cutoff (`topCov`) Distortion**: If any cell happens to contain all 4 filler words + the entity token (`matched = 5`), `topCov` becomes `5`, and another valid cell that matches the entity token + 3 of 4 filler words (`matched = 4 >= need`, so `ok = true` and `viaFallback = false`) gets **dropped by the `topCov` filter** (`h.matched >= topCov` fails because `4 < 5`).
- **Recommendation**:
  1. Strip punctuation at token boundaries (or use `lexFields`-style splitting) in both `NormalizeQuery` and document word tokenization.
  2. Strip conversational query stopwords in `KwTokens` (using the same fallback rule as `codegraph/index/lexical.go:kwTokens`: if *all* tokens in the query are stopwords, keep the non-stopword-filtered list).
  3. Fix the `lexConfident` and `topCov` checks in `rankAndLimit` / `KeywordSearchScored`:
     - Require `weightedMatchFraction >= 0.75` in addition to `matched >= need` before reporting `lexConfident = true`.
     - In `rankAndLimit`'s `topCov` filter, exempt items whose `weightedMatchFraction >= wCoverageMassThreshold` or compare weighted coverage instead of raw filler-token count.

---

### 2.2. Premature Semantic Score-Gap Truncation Before RRF Fusion (High Impact — Both `x-ray` & `core`)
- **Where**:
  - [`x-ray/internal/vectorstore/memorycellstore.go`](../../internal/vectorstore/memorycellstore.go#L204-L227) (`defaultSemanticScoreGap = 0.02`, `applyScoreGap`)
  - `core/internal/vectorstore/memorycellstore.go`
- **Problem**:
  - `MemoryCellStore.Search` and `SearchSubject` apply `applyScoreGap(results, s.scoreGap)` (`0.02` on the `1 - dist/2` scale, which equals `0.08` raw cosine) **inside the semantic channel before RRF fusion**.
  - When a query has one near-verbatim cell (say `Score = 0.96`), `applyScoreGap` immediately truncates every other semantic candidate with `Score < 0.94` — even if the caller asked for `fetchLimit = 40` (`limit * 4`).
  - As a result:
    1. A cell that ranked #2 in lexical and #4 in semantic (at `Score = 0.935`) loses its semantic RRF vote completely.
    2. In `core`, where tenant filtering (`keep(it)`) and active/kind filtering happen **after** `cellStore.SearchTimed` (`search.go:321-345`), if the #1 semantic hit belongs to **Tenant A** and has `Score = 0.96`, and **Tenant B**'s relevant cells sit at `Score = 0.93`, `applyScoreGap` drops Tenant B's cells *before* `hybridSearch` filters by tenant. If lexical had 1 weak hit for Tenant B, `exhaustiveSemanticFallback` doesn't trigger (`len(lexFiltered) > 0`), so Tenant B gets **zero semantic results**.
- **Recommendation**:
  - Do **not** apply a hard `scoreGap` truncation inside `MemoryCellStore.Search` before tenant/status filtering and RRF fusion.
  - Instead, filter by tenant/active/kind first, fuse the full `fetchLimit` candidate pool, and use **largest-gap cliff detection** (`CutAtLargestGap` from [`x-ray/internal/codegraph/index/index.go`](../../internal/codegraph/index/index.go#L767-L793)) when a post-search cutoff is desired.

---

### 2.3. Unweighted 2-Channel RRF Tie-Breaking & Missing Exact-Title Channel (Medium-High Impact)
- **Where**:
  - [`x-ray/internal/xray/memory.go`](../../internal/xray/memory.go#L660-L699) (`fuseRRF`)
  - `core/internal/memory/lexical.go` (`fuseRRF`)
- **Problem**:
  - In `fuseRRF(lex, sem, limit)`, rank 0 of `lex` gets `1/(60+0)` and rank 0 of `sem` gets `1/(60+0)`.
  - If `lex` only found 1 weak `viaFallback` partial match and `sem` found an exact semantic answer at rank 0 (only in `sem`), both have score `1/60 = 0.01667`, and `fuseRRF` breaks ties **alphabetically by `Item.ID`** (`ranked[i].it.ID < ranked[j].it.ID`).
  - Furthermore, `x-ray/internal/codegraph/index` uses a **3-channel RRF** (`fuseRRF(lex, sem, limit, exact)` in [`codegraph/index/lexical.go`](../../internal/codegraph/index/lexical.go#L505-L560)), where exact title/identifier matches cast a dedicated vote so an exact subject/title match is guaranteed to outrank single-channel hits.
- **Recommendation**:
  1. Break equal-RRF-score ties deterministically by channel confidence and `UpdatedAt` descending before falling back to `ID`.
  2. Add the 3rd RRF channel (or rank-0 bonus) for **exact `Title` or exact `Subject` match**, matching `codegraph/index/lexical.go`.

---

### 2.4. `stateRerank` / `Rerank` Ignores Provenance (`Origin`), `Confidence`, and `EventDate` (Medium Impact)
- **Where**:
  - [`x-ray/internal/xray/memory_rerank.go`](../../internal/xray/memory_rerank.go#L140-L167) (`stateRerank`)
  - `core/internal/memory/search.go` (`Searcher.Rerank`)
- **Problem**:
  1. **Provenance (`Origin`) and `Confidence` are ignored during ranking**:
     - Both `x-ray` and `core` implement ADR-053 provenance (`OriginHuman`, `OriginVerified`, `OriginObserved`, `OriginDerived`) and `Confidence` (`high`, `medium`, `low`), and use `betterFact` / `BetterRoutingFact` when *consolidating* duplicates.
     - Yet `stateRerank` (`x-ray`) and `Searcher.Rerank` (`core`) ignore both fields when scoring search results. A newly written `OriginDerived` `hypothesis` with `Confidence: "low"` and a fresh `UpdatedAt` outranks a 5-day-old `OriginHuman` / `OriginVerified` `stable_fact` with `Confidence: "high"` at the same RRF rank.
  2. **`EventDate` ignored for `kind="event"` recency**:
     - `recencyMultiplier` uses `it.UpdatedAt`. When historical `event` cells are bulk-seeded or restored, their `UpdatedAt` is the ingestion timestamp (`1.0`), even if `it.EventDate` was months ago.
  3. **In `core`, `collapseBetter` (`search.go:785-798`) ignores `Confidence` and `Origin`**:
     - Unlike `x-ray`'s `betterFact` (`memory_dedup.go:227`), `core`'s `collapseBetter` breaks score ties by `isOpenConcern`, then `UpdatedAt`, then `ID` — so if a newly written `low`-confidence restatement clusters with an older `high`-confidence human fact at the same score, `collapseNearDuplicateKnowledge` picks the `low`-confidence item as the survivor.
- **Recommendation**:
  - Add a bounded provenance/confidence multiplier in `stateRerank` / `Rerank` (e.g., `OriginHuman`/`OriginVerified`: `1.10`, `Confidence="high"`: `1.05`, `Confidence="low"`: `0.92`).
  - In `recencyMultiplier`, if `it.Kind == "event"` and `it.EventDate != ""`, parse `EventDate` as the effective timestamp before falling back to `UpdatedAt`.
  - In `core`'s `collapseBetter`, include `confidenceRank` and `originRank` before `UpdatedAt`, matching `x-ray`'s `betterFact`.

---

### 2.5. ThoughtLink 1-Hop Expansion (`expandViaLinks`) Ignores `link.Weight` (Low-Medium Impact)
- **Where**:
  - [`x-ray/internal/xray/memory.go`](../../internal/xray/memory.go#L701-L740)
  - `core/internal/memory/search.go`
- **Problem**:
  - `expandViaLinks` caps additions at `maxExtra = limit / 2` and orders candidate neighbors solely by `fromRank` and slice insertion order, ignoring `link.Weight`.
- **Recommendation**:
  - Sort `targets` in `expandViaLinks` by `(fromRank ASC, link.Weight DESC)` so the strongest-weighted links from top hits are selected first.

---

## 3. Performance & Clean-Code Optimizations in `x-ray` (and `core`)

### 3.1. `x-ray/internal/xray`: Trigram Index is Computed and Discarded on Every Search, Plus Broken Single-Token Seeding
- **Where**:
  - [`x-ray/internal/xray/memory.go`](../../internal/xray/memory.go#L1099-L1109) and [`memory.go#L1286-L1295`](../../internal/xray/memory.go#L1286-L1295)
  - [`x-ray/internal/xray/kwindex.go`](../../internal/xray/kwindex.go#L156-L187) (`trigramIndex.search`)
- **Problem**:
  1. `keywordSearchCache` and `keywordSearchMap` call `s.kwIdx.search(q)` to build `trigramCandidates`, pass it into `rankAndLimit`, and `rankAndLimit` **never uses `trigramCandidates` to filter `cands`** — it only increments `kwTrigramHits` while scanning 100% of `cands`.
  2. `kwIdx.search(q)` (`kwindex.go:161-181`) seeds candidates **only** from `firstTok := toks[0]` and requires every token to trigram-match. If `toks[0]` is a conversational word absent from the document or has a 1-edit typo on a short word, `kwIdx.search(q)` returns empty.
  3. In `x-ray/internal/xray/memory.go:1185-1197`, `rankAndLimit` calls `idx.itemScoreGrams(it.ID, tokGrams)` inside the per-candidate loop, acquiring `idx.mu.RLock()` once per candidate.
- **Recommendation**:
  - Port `core/internal/memory`'s **`vocabIndex`** (`kwvocab.go`) and **`QueryOverlapScores`** (`kwindex.go:160-208`) back to `x-ray/internal/xray`.

---

### 3.2. `x-ray/internal/xray`: Missing `lexicalCache` Causes Repeated Per-Query Allocations (`ToLower`, `Fields`, `RuneCount`)
- **Where**:
  - [`x-ray/internal/xray/memory.go`](../../internal/xray/memory.go#L909-L1063) (`softMatchCoverage`, `lexScore`, `fuzzyWordMatch`, `weightedMatchFraction`)
  - [`x-ray/internal/xray/lexical.go`](../../internal/xray/lexical.go#L76-L99) (`docFreqs`)
- **Problem**:
  - On every search in `x-ray/internal/xray`, `docFreqs`, `softMatchCoverage`, `weightedMatchFraction`, and `lexScore` repeatedly allocate lowercased strings, run `normalizeQuery`, split `strings.Fields`, and recompute `utf8.RuneCountInString` for every item in the corpus.
- **Recommendation**:
  - Port `core/internal/memory`'s **`lexicalCache` + `wordTokens`** (`core/internal/memory/lexical.go:184-206, 593-675`) and `vocabIndex.wordDocFreq(t)` into `x-ray/internal/xray`.

---

### 3.3. `x-ray/internal/vectorstore/memorycellstore.go`: Port the Lock & Map-Clone Fixes from `core`
- **Where**:
  - [`x-ray/internal/vectorstore/memorycellstore.go`](../../internal/vectorstore/memorycellstore.go#L351-L569)
- **Problem**:
  - `ensureFreshIndex()` takes an unconditional exclusive `s.mu.Lock()`, and `Search()`, `SearchSubject()`, and `Neighborhood()` clone the entire `s.entries` map on every query.
- **Recommendation**:
  - Port the `RLock` fast path in `ensureFreshIndex()` and direct `s.entries[nb.Key]` lookups under `RLock` from `core/internal/vectorstore/memorycellstore.go`.

---

### 3.4. `core/internal/corpusmaps/lexical.go`: Upgrade to `x-ray/internal/codegraph/index`'s 2-Level Vocabulary Index & `editDistanceWithin`
- **Where**:
  - `core/internal/corpusmaps/lexical.go`
  - Compare with: [`x-ray/internal/codegraph/index/lexical.go`](../../internal/codegraph/index/lexical.go#L33-L212)
- **Problem**:
  - `core/internal/corpusmaps/lexical.go` still uses a 1-level `grams` index (no word-level deduplication, no pigeonhole 2-gram typo probing for 4–5 char identifiers), uses the old `[]rune`-allocating `editDistance` instead of `editDistanceWithin`, and lacks `searchExactTitle` (the 3rd RRF channel).
- **Recommendation**:
  - Sync `x-ray/internal/codegraph/index/lexical.go`'s 2-level vocabulary index, pigeonhole typo probing, `editDistanceWithin`, and `searchExactTitle` into `core/internal/corpusmaps`.

---

## 4. Prioritized Action Plan

| Priority | Target Repo(s) | Change | Expected Impact |
| :---: | :--- | :--- | :--- |
| **P0** | `x-ray` & `core` | **Fix query/document punctuation stripping & conversational stopwords** in `NormalizeQuery`/`KwTokens`, and require `weightedMatchFraction` before setting `lexConfident = true` or dropping items in `topCov`. | Prevents punctuation (`auth?`, `db.`) from breaking exact matches; stops conversational filler tokens from triggering false `lexConfident` short-circuits or `topCov` drops. |
| **P1** | `x-ray` & `core` | **Fix premature `scoreGap` truncation before RRF fusion** in `MemoryCellStore` / `hybridSearch` (apply gap cutoff after tenant filtering & fusion, or use adaptive `CutAtLargestGap`). | Eliminates multi-fact and cross-tenant semantic starvation when rank-0 is a near-verbatim hit. |
| **P1** | `x-ray` & `core` | **Improve RRF tie-breaking & add Exact-Title/Subject RRF boost** (`fuseRRF`). | Stops breaking lexical-vs-semantic rank ties alphabetically by `Item.ID`; guarantees exact title/subject matches surface at the top. |
| **P2** | `x-ray` & `core` | **Add Provenance (`Origin`), `Confidence`, and `EventDate` to `stateRerank` / `Rerank` & `collapseBetter`** + sort `expandViaLinks` by `link.Weight`. | Ensures `human`/`verified` and `high`-confidence facts outrank `derived`/`low`-confidence restatements; orders 1-hop link expansion by edge strength. |
| **P2** | `x-ray` | **Port `vocabIndex`, `lexicalCache`, `QueryOverlapScores`, and `MemoryCellStore` lock/map fixes** from `core` to `x-ray`. | ~50–100x faster lexical search in `x-ray`, zero per-query full-corpus tokenization allocations, and concurrent `RLock` vector searches. |
| **P3** | `core` | **Port 2-level vocabulary index, pigeonhole typo probing, `editDistanceWithin`, and `searchExactTitle`** from `x-ray/internal/codegraph/index` to `core/internal/corpusmaps`. | Unifies recall-map search quality (typo tolerance on 4–5 char identifiers + exact-node-name RRF channel) and lowers memory footprint in `core`. |

## 7. Known library behaviour: identical vectors collapse in the graph

The HNSW library the cell index uses (`github.com/fogfish/hnsw`) does not keep
two nodes apart when their vectors are exactly equal: a graph search over four
cells seeded with the same vector returns one of them. Real embeddings are
never bit-identical, so the store is not affected, but a test that seeds
several cells with one vector and expects all of them back sees one. An exact
scan (a kind filter, or `SearchExhaustiveTimed`) returns every entry. The test
that shows it is `TestHNSW_IdenticalVectorsCollapseToOneNode` in
`pkg/memstore/index`. Remember it when the P1 work changes how the vector
channel is fetched and filtered.
