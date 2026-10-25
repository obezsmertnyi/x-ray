# ADR-030: Hybrid Search Scoring & Embedding Model (Trigram + Keyword + Semantic Fusion; nomic-embed-text-v1.5 @ 256d)

> **Scope note:** despite the "scoring" name, this ADR is also the home of record
> for X-Ray's **embedding model** decision — which model, quantization, dimension,
> and serving sidecar. That choice fell out of the retrieval-quality battery run to
> validate the fusion scoring (see Addenda 1–3). The exact deployed model is pinned
> in **Addendum 3**.

**Status:** Implemented  
## Context

The memory search path has accreted three independent relevance signals, but
two of them are used as **boolean gates** rather than scores. The result is a
fragile pipeline where edge cases are fixed by adding more thresholds rather
than by improving a single ranking function.

Today's pipeline (see `internal/xray/memory.go`, `internal/xray/kwindex.go`,
`internal/xray/voice_tools.go`):

1. **Trigram pre-filter** (`trigramIndex.search`, `kwindex.go:132`) — requires
   **every** query token to trigram-match at least one word (hard AND). Returns
   a candidate ID set.
2. **Soft-AND keyword filter** (`softMatchesKW`, `memory.go:326`, added in
   `a2c7400`) — allows one missed token for 3+ token queries, strict AND for
   1–2 tokens. Also a boolean gate.
3. **Semantic search** (`semanticSearch`, `memory.go:187`) — HNSW cosine
   similarity, returns a clean `Score float32` per result. Consulted only in a
   separate branch: voice falls back to it when keyword returns `< 2` items
   (`voice_tools.go:178`).

### Why this is a problem

The `a2c7400` fix ("X-Ray RAG project" finds "X-Ray RAG") works **by accident
of fall-through ordering**: the hard trigram AND drops the item, then
`softMatchesKW` silently rescues it into the `kwHits` bucket. The two AND-filters
disagree on strictness, and the only reason the right answer surfaces is the
order in which the buckets are concatenated.

Symptoms of the underlying design issue:

- **Threshold whack-a-mole.** Each new query shape (the soft-AND rule, the
  trigram-first ranking amendment to ADR-010, the `voice_tools.go`
  `< 2` semantic-fallback count) is a new boolean patch. They interact in ways
  that are hard to reason about and easy to regress.
- **Discarded signal.** The trigram index computes a continuous per-token
  overlap fraction (`tokMatchesItem`, `kwindex.go:88`) and immediately collapses
  it to a bool. `softMatchesKW` computes a matched-token *count* and collapses it
  to a bool. Both throw away ranking information.
- **Semantic is branched, not blended.** A query either gets keyword results or
  semantic results (with a count-based hand-off), never a principled mix. Cosine
  similarity (0–1) and keyword scores (0–1000+) are never compared because they
  live on different scales and in different code paths.
- **Duplicated paths.** `keywordSearchCache` and `keywordSearchMap` each
  implement the same two-pass (trigram-hits-then-kw-hits) collect-and-rank.

### Why not "just go full semantic"

X-Ray's corpus is **short, named entities** — "X-Ray RAG", "X-Ray REC",
person names, project names — queried by voice. This is the worst case for pure
embedding search and the best case for keyword/lexical matching:

| Query pattern | Keyword/trigram | Pure semantic |
|---|---|---|
| "X-Ray RAG" vs "X-Ray REC" (1-char diff) | distinguishes exactly | often collapses — near-identical embeddings |
| voice mishear "RAC" → "RAG" | 1-edit fuzzy catches it | unpredictable neighborhood |
| exact project/person name lookup | deterministic, no network call | per-query Vertex AI latency + cost |
| "tell me about the recording thing" (paraphrase) | misses | wins |

Exact and near-exact name matching is the **dominant** query pattern here, so
lexical signal must stay primary. Semantic earns its keep only on the paraphrase
tail. The right answer is not "pick one" — it is to combine them so neither can
veto the other.

## Decision

Replace the stacked boolean gates with a **single scoring function** and fuse
the lexical and semantic rankings with **Reciprocal Rank Fusion (RRF)**.

### 1. Trigram becomes a scorer, not a filter

Add a continuous companion to `tokMatchesItem`:

```go
// tokOverlap returns the best trigram-overlap fraction (0..1) for tok against
// any single word in the item — the continuous form of tokMatchesItem.
func (idx *trigramIndex) tokOverlap(id, tok string) float64
```

`trigramScore(item, toks)` averages `tokOverlap` across tokens, yielding a value
in [0,1]. No item is **excluded** by trigram coverage; weak matches simply score
low and fall below the limit. `trigramIndex.search` is retained only as a cheap
candidate-narrowing pre-filter for the cache path (to avoid scoring every CR),
but it no longer determines inclusion — it widens, never vetoes.

### 2. One lexical score, one rank function

Replace `softMatchesKW` and the duplicated two-pass collect in both
`keywordSearchCache` and `keywordSearchMap` with a single scorer applied to every
candidate:

```
lexScore(item, q, toks) =
      wExact   * exactPhraseHit(item, q)      // current 1000/600 title bonus — keeps name lookups deterministic
    + wToken   * matchedTokenFraction(item, toks)  // continuous replacement for softMatchesKW
    + wTrigram * trigramScore(item, toks)      // continuous replacement for the hard trigram AND
```

A low score **floor** (`lexScore > minLex`) replaces the AND gates. This is what
makes "X-Ray RAG project" surface without any special soft-AND rule: the missing
"project" token lowers `matchedTokenFraction` but the item still clears the floor
and ranks below any full-coverage match. The `softMatchesKW` function, the
3-token special case, and the trigram hard-AND all **disappear**.

### 3. Blend semantic via RRF, not via a branch

When the semantic store is available (`s.cells != nil`), run both rankings and
fuse by **rank**, not by raw score:

```
fused[id] = 1/(k + lexRank[id]) + 1/(k + semRank[id])   // k ≈ 60 (standard RRF constant)
```

RRF is chosen deliberately: cosine similarity (0–1) and lexical scores
(0–1000+) are not on a comparable scale, so fusing raw scores requires fragile
per-query normalization. Fusing **ranks** sidesteps that entirely. An item
missing from one ranking simply contributes only the other term. Keyword's
exact-name precision and semantic's paraphrase recall both contribute and
neither can veto the other.

### 4. Cost control: coverage-gated semantic call

Blending semantic on *every* query means an embedding round-trip (Vertex AI
latency + cost) per voice search, **and** re-introduces the looser semantic
recall the lexical coverage gate just excluded. To preserve today's "skip the
network when a strong match exists" behavior — on a principled basis rather than
a result *count* — only invoke semantic when the top lexical hit is *not* a
confident match.

Confidence is **coverage-based, not score-based**:

```
matched := softMatchCoverage(topHit, toks)
need := len(toks); if len(toks) >= 3 { need = len(toks) - 1 }
if matched >= need { return lexical directly }  // confident — skip semantic
else               { run semantic + RRF fuse }
```

A raw-score bar was tried first and rejected: the per-token miss penalty drives a
legitimate partial match's score near zero (e.g. "X-Ray RAG project" → "X-Ray
RAG" scores ~0 after the −200 miss penalty), which would force semantic fusion on
essentially every multi-token name query and re-pollute the result. Coverage is
the stable signal — it asks "did the best hit cover the query?" independent of
the ranking weights.

This replaces the `voice_tools.go` `len(items) < 2` heuristic. Cheap, confident
exact-name hits (the common case) skip the embedding call entirely.

## Consequences

**Positive:**

- "X-Ray RAG project" and future partial-coverage queries are handled by a
  continuous floor, not a special case — no more threshold whack-a-mole.
- One scorer, shared by `keywordSearchCache` and `keywordSearchMap`; the two-pass
  trigram/kw bucket collect is removed.
- `softMatchesKW`, the 3-token special case, the trigram hard-AND, and the `< 2`
  voice fallback are all deleted, not added to.
- Semantic contributes to every query that needs it, fused on a sound basis,
  while exact-name precision is preserved by lexical weighting.
- Confidence-gated semantic keeps the cheap-exact-match fast path.

**Negative / tradeoffs:**

- Net new tuning surface: `wExact`, `wToken`, `wTrigram`, `wMissPenalty`, and
  RRF `k`. Mitigated by table-driven tests that pin the current SoftAND/trigram
  behaviors as a regression floor before weights are tuned. The semantic gate is
  *not* on this surface — it is coverage-based and weight-independent.
- Coverage-gating means a paraphrase query whose top lexical hit *also* fully
  covers the tokens will skip semantic. Acceptable: full lexical coverage is
  already a good answer. Paraphrases that do *not* lexically cover still fuse.
- RRF discards score magnitude in favor of rank, so two near-tied lexical results
  fuse identically with semantic regardless of how close they were. Acceptable
  for this corpus size; revisit if score-aware fusion is ever needed.

## Measured results

Per-query `search_memory` latency (call → result) from a live voice session on
`v1.17.6-71d907f`, measured from the debug log timestamps:

| Query | Path | Embed call | call → result |
|-------|------|-----------|---------------|
| `X-Ray Rak` (mishear) | confident lexical | none | **36 ms** |
| `X-Ray project` | confident lexical | none | **30 ms** |
| `blog LLM as charge` (garbled) | confident lexical | none | **32 ms** |
| `launch plan` (paraphrase) | semantic fusion | `dur=186.5 ms` | **224 ms** |

- **The win is eliminating the Vertex AI embedding round-trip, not faster
  ranking.** The lexical match itself is sub-millisecond; the ~30–36 ms on the
  fast path is surrounding handler overhead (cache list, JSON marshal of
  content, `PublishHit` SSE broadcast) — the same overhead the semantic path
  also pays.
- On the semantic path, the embed call (`~187 ms`) is **83% of the 224 ms**. So
  a confident lexical answer is **~190 ms faster (≈ 6.8×)** than the old
  always-embed behavior, and costs no Vertex AI call.
- In this session **3 of 4 searches (75%) took the fast path** — name lookups,
  mishears, and even the triple-garbled "blog LLM as charge" → the LLM-as-Judge
  post. Only the true paraphrase ("launch plan", no name to lock onto) needed
  semantic.

Caveat: 4 data points from one session. The per-query delta (~187 ms saved when
the embed call is skipped) is unambiguous, but a production-wide speedup figure
depends on the confident-vs-fused query mix. That split is already tracked in
`SearchMetrics` as `KWVoiceHits` (confident lexical, embed skipped) vs
`KWVoiceMisses` (semantic fused) — the hit ratio gives the real production
fast-path rate.

### Addendum (2026-05-31): local 128d embedding profile — semantic-leg quality

The original results above used Vertex AI (768d `text-embedding-005`). This
addendum characterises the **fully-local** embedding path enabled by
`33ee2f3` (the gateway now honours `--embedding-dims`, so Matryoshka models
truncate cleanly): **Ollama `nomic-embed-text` truncated to 128d**, no GCP, run
against the envtest devenv with the standard 24-cell X-Ray seed set.

**Embed latency (warm).** ~40–50 ms per `RETRIEVAL_QUERY` embed call against
local Ollama — comparable to the Vertex ~187 ms embed but with no network /
cost. **Cold start:** the *first* query after Ollama evicts the model
(`keep_alive` default 5 min) pays a one-shot model reload — observed **555 ms**,
then back to ~44 ms for every subsequent call. Pin with `keep_alive: -1` (or a
startup warm-up ping) to keep the avatar's first turn off the cold path.

**Semantic-leg quality.** Six paraphrase queries worded to share **near-zero
vocabulary** with their target cell — isolating the embedding leg, since
keyword-free queries fail the lexical coverage gate and fall through to
semantic + RRF. Each result is the rank of the intended cell:

| Query (no shared keywords) | Intended target | Top-1 | In top-3 |
|---|---|---|---|
| "burning through unlimited model usage every day" | Per-user token quota | ✅ | ✅ |
| "how is the design rationale written down and tracked" | ADRs | ✅ | ✅ |
| "prevent strangers from hijacking the live audio connection" | Public-exposure hardening | ❌ (#3) | ✅ |
| "fuse keyword and meaning-based ranking into one result list" | Unified hybrid search | ❌ (#2) | ✅ |
| "register a new colleague so they appear under People" | Contributor/team format | ✅ | ✅ |
| "keep my jotting hidden so only I can see it" | Scope isolation | ✅ | ✅ |

**Top-1: 4/6 (67%). Top-3: 6/6 (100%).** Results were identical across a
controller restart (which clears the in-process embed cache and rehydrates cells
from CRs), confirming the embed+rank pipeline is deterministic — not a cache
artefact.

**The two misses are content-structure, not dimensionality.** Both intended
targets are long, multi-topic cells (the hardening cell lists 8 unrelated
security controls; the hybrid-search cell is dense jargon). Mean-pooling dilutes
the one relevant sentence, so a shorter, focused cell out-ranks them on the
keyword-free phrasing. 768d would likely show the same ordering — the cause is
the cell, not the vector width.

**Lexical fusion recovers both misses — as designed.** Re-querying with the
keyword the user would plausibly say floats the right cell to **#1**:

| Keyword-bearing query | Path | Intended target rank |
|---|---|---|
| "which security control blocks websocket **hijacking** on the realtime endpoint" | lexical→RRF | **#1** |
| "where is **reciprocal rank fusion** combining **lexical** and semantic scores described" | confident lexical (semantic skipped) | **#1** |

The second is the coverage-gated fast path in action: the literal terms gave a
full-coverage lexical hit, so per the confidence gate it returned lexical-only
and never embedded. This is the ADR's core thesis confirmed on a local 128d
setup — semantics catch the paraphrases, lexical catches the keyword-bearing
queries, RRF blends the ambiguous middle.

**Conclusion:** local 128d nomic + this ADR's hybrid fusion is production-viable
for X-Ray's corpus (100% top-3 on adversarial paraphrases, ~45 ms warm, zero
cloud dependency). 128d's smaller vectors also shrink the HNSW graph and CR-cached
embeddings vs 768d. The semantic-only top-1 ceiling is a function of cell
structure; the lexical leg is what keeps named-entity and keyword queries exact.

Caveat: 6 queries, single corpus, one run (plus a restart replay). Indicative of
the mechanism, not a recall benchmark.

### Addendum 2 (2026-05-31): model + dimension + truncation sweep

The first addendum tested only nomic@128d on Ollama and concluded "the misses are
content-structure, not dimensionality." A wider sweep — same 6-query battery, same
corpus, all run live through X-Ray's full hybrid path — **partially revises that**:
dimensions *do* matter up to a knee, and the embedding *server* matters too.

**Server choice (less deps).** Swapped Ollama for **llama.cpp `llama-server
--embeddings`** (a single 23 MB binary, no daemon, no model-management process, no
idle eviction): warm embed latency dropped to **~9–11 ms** (vs Ollama's ~40–50 ms).
Caveat: llama.cpp ignores the OpenAI `dimensions` param (returns native dims), which
motivated the client-side truncation below.

**Client-side Matryoshka truncation.** Because llama.cpp won't truncate and Ollama's
`dimensions` truncation is not the canonical MRL procedure, `embedGateway` now does
it itself: fetch native dims, keep the first N components, **L2-renormalize**
(`truncateNormalize`, the procedure documented by nomic). The gateway no longer sends
`dimensions`. This makes `--embedding-dims` work uniformly across any gateway.

**Model / dimension matrix** (top-1 / top-3 over the 6 queries; the lone universal
miss is Q3 "hijacking the live audio", whose target is a bloated 8-item security
cell):

| Config | Server | Model | Dims | top-1 | top-3 |
|---|---|---|---|---|---|
| local | Ollama | all-minilm | 384 | 3/6 | 5/6 |
| local | Ollama/llama.cpp | nomic | 128 | 4/6 | 6/6 |
| local | llama.cpp | nomic | **256** | **5/6** | 6/6 |
| local | llama.cpp | nomic | 768 (native) | 5/6 | 6/6 |
| cloud | Vertex | gemini-embedding-001 | 128 | 5/6 | 6/6 |

**Findings:**

- **256d is the knee for nomic.** 128→256 recovers the one query 128d lost (Q2,
  "design rationale" → ADRs); 256→768 buys nothing on this corpus. So **256d local
  nomic ties native 768d *and* ties cloud Vertex (5/6)** at one-third the vector
  size — smaller HNSW graph and CR-cached embeddings for free. This is the new local
  default (`EMBED_DIMS=256`).
- **Model quality is per-dimension, not just absolute.** Vertex `gemini-embedding-001`
  reaches 5/6 at *128d* where nomic needs 256d — a stronger MRL model. nomic still
  matches it once given enough dims, locally and free.
- **all-minilm (384d, prefix-free) is the weakest (3/6)** despite more dims than
  nomic@256 and no prefix handling — confirming model choice dominates dim count.
- **Task prefixes don't help nomic here.** A separate controlled test (nomic@128,
  with vs without `search_query:`/`search_document:`) showed **no change** (4/6 both
  ways), so X-Ray's prefix-less gateway path is not the limiter. Low ROI; not added.
- **Truncation method is ≈neutral live.** An offline pure-cosine script suggested
  client-truncate beat Ollama's `dimensions` (5/6 vs 4/6), but that gap **did not
  reproduce** in live X-Ray on the full prose corpus (both 4/6 at 128d). The
  truncation code is kept as **correctness/uniformity plumbing** (spec-correct MRL;
  makes `--embedding-dims` work on llama.cpp), not as a measured quality win.
- **Q3 misses in every config** (128/256/384/768, nomic/minilm/Vertex) — conclusively
  a content-dilution problem (the security cell averages one relevant sentence across
  eight topics), recoverable only by splitting the cell or via the lexical leg (which
  kept it in top-3 in all live runs).

**Recommended local profile:** llama.cpp + nomic @ **256d** with client-side
truncation — Vertex-equivalent quality (5/6 top-1, 6/6 top-3), ~9–11 ms warm, 23 MB
binary, zero cloud dependency.

Caveat: same 6-query / single-corpus limits as Addendum 1; a recall benchmark would
need a larger labelled query set.

### Addendum 3 (2026-06-05): the exact deployed model

The sweep above says "nomic" generically; for the record, the **specific build now
in production** is:

| Property | Value | Source of truth |
|---|---|---|
| Model | **nomic-embed-text-v1.5** | HF [`nomic-ai/nomic-embed-text-v1.5`](https://huggingface.co/nomic-ai/nomic-embed-text-v1.5) |
| GGUF / quant | `nomic-embed-text-v1.5.Q8_0.gguf` | `Makefile` `HF_REPO`/`HF_FILE`; `charts/xray-xray/values.yaml` `embedding.sidecar.hf*` |
| Native dims | 768 (Matryoshka: 768/512/**256**/128/64) | model card |
| Deployed dims | **256** (client-side L2-renormalized truncation) | chart `embedding.dims: 256` |
| Max sequence | 8192 tokens | model card; chart `ctxSize 16384` / `physicalBatch 2048` give a 2048-token/slot window |
| Delivery | GGUF **baked** into `xray-embedder` image (no HF pull at runtime) | `deploy/embedder/Dockerfile`, chart `sidecar.image` + `modelPath: /models/model.gguf` |
| Serving | llama.cpp `llama-server --embeddings`, in-pod sidecar | chart `embedding.sidecar` |

**v1.5 vs the matrix.** The Addendum-2 sweep was run on this same v1.5 family — v1.5
is the version that *has* Matryoshka (v1 is fixed-768), which is precisely what makes
the 256d truncation valid. So "256d is the knee for nomic" is specifically a v1.5
result; it would not transfer to v1.

**Task prefixes.** v1.5's card recommends `search_query:` / `search_document:`
prefixes. Addendum 2 measured them as **no-change on this corpus** (4/6 both ways), so
X-Ray's gateway path stays prefix-less by choice — not an oversight. Revisit only if
a larger labelled query set shows a gain.

**Quantization headroom.** Q8_0 is near-lossless; F16 would only add size for no
measurable retrieval gain at 256d, and lighter quants (Q4_K_M) are an untested
size/speed trade. Swapping is a one-line `make embedder-push HF_FILE=...` build-arg
change (`HF_REPO`/`HF_FILE`), then bump `sidecar.image` — no app change.

## Implementation notes

Files to touch:

1. `internal/xray/kwindex.go` — add `tokOverlap`; keep `search` as a
   candidate-widening pre-filter only.
2. `internal/xray/memory.go` — add `lexScore` + `softMatchCoverage`; replace
   `softMatchesKW` and the two-pass collect in `keywordSearchCache` /
   `keywordSearchMap` with a single scored pass (`rankAndLimit`); add RRF fusion
   in `Search`, gated on `s.cells != nil` and the coverage-based confidence of
   the top lexical hit (`keywordSearchScored` returns a `confident bool`).
3. `internal/xray/voice_tools.go` — remove the `len(items) < 2` semantic
   fallback heuristic (now handled inside `Search` by the confidence gate).
4. `internal/vectorstore` — no change; `CellSearchResult.Score` already exists.
5. Tests — `internal/xray/memory_test.go`: keep the existing `SoftAND`,
   `Trigram*`, and `TokenNormalization` cases passing as a regression floor; add
   RRF fusion + confidence-gate cases.

## Relationship to other ADRs

- **ADR-001** (Memory Store — Two Modes): this ADR collapses the two modes
  (keyword-only vs semantic) into a single blended path when an embedder is
  present; keyword-only remains the graceful-degradation path when `s.cells` is
  nil.
- **ADR-010** (Cache-Backed Memory Search): the `162d2ff` trigram-first ranking
  amendment is superseded — the two-pass bucket collect it introduced is replaced
  by the unified scorer. Field-index narrowing and the ephemeral-map merge are
  unchanged.
- **ADR-020** (Associative Memory Architecture): the HNSW store and its cosine
  `Score` are the semantic half of the fusion; no change to the index itself.
- Supersedes the soft-AND mechanism introduced in `a2c7400` (no ADR; the
  `softMatchesKW` function is removed by this change).
