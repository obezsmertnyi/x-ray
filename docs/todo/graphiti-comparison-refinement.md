# Graphiti (Zep) vs xray: mechanism comparison and retrieval refinement plan

Status: research complete 2026-08-14, build decisions pending. Source basis:
Graphiti source at HEAD (search/search.py, search_config_recipes.py,
maintenance operations, extraction prompts), the Zep paper (arXiv 2501.13956),
Zep's LoCoMo blog, and Zep's actual LoCoMo eval code in
getzep/zep-papers/kg_architecture_agent_memory/locomo_eval/. xray ground truth
verified in internal/xray/memory.go (Search :285, rrfK=60 :279) and
cellsync.go. Measured xray numbers below come from our own LoCoMo run
(hack/locomo, 2026-08-13/14): core 37.1%, recall@20 53.4%, accuracy 56.5% with
gold evidence retrieved vs 8.8% without.

## The key discovery

Zep's winning LoCoMo configuration (75.14 J-score, above the ~73 full-context
baseline) is much smaller than Graphiti's architecture. Their eval code runs
exactly two concurrent graph.search() calls per question: scope=edges with
cross_encoder rerank limit 20, and scope=nodes with rrf rerank limit 20.
Context is composed as `fact (event_time: {edge.valid_at})` lines plus
`{node.name}: {node.summary}` lines. No BFS, no episode retrieval, no
communities, no MMR, no node-distance. The score is bought by three things:

1. Ingest-time atomic-fact extraction with relative dates resolved against the
   episode's created_at.
2. Entity nodes whose summaries pre-aggregate evidence across sessions.
3. A cross-encoder rerank over facts.

Everything else in Graphiti is optional machinery their own benchmark run does
not exercise.

## Mechanism map

| Graphiti mechanism (verified in source) | xray nearest equivalent | Gap |
|---|---|---|
| Episode ingestion: raw messages stored as non-lossy episode nodes with created_at; derived facts keep provenance edges to episodes. Extraction context = current message + last 4. | Raw cells stored as-is (kind=event, EventDate, author tags). Non-lossy store exists. | No provenance link from distilled cells back to source cells. |
| Ingest-time extraction: per-episode LLM calls extract entities (reflexion anti-hallucination loop) and atomic facts; prompt demands every concrete noun, number, descriptor survive into the fact. | Batch reflection/knowledge passes, hours cadence, distill from cell digests. | Cadence (seconds vs hours) and granularity (per-episode atomic facts vs batch summaries). This is the core delta. |
| Entity resolution: embedding candidates (cos >= 0.6, <= 15) + fulltext; deterministic pass first (exact match, MinHash/LSH shingles, Jaccard >= 0.9); LLM dedupe only for unresolved; merged nodes get updated name+summary. | Absent in the memory substrate. (The triage service graph is a curated entity layer for the ops domain, but separate, and never built from memory ingestion.) | Whole layer absent. It is what makes entity summaries possible. |
| Bi-temporal model: created_at/expired_at (transactional) plus valid_at/invalid_at (event time); relative dates resolved by the LLM against the episode timestamp at ingest. | CreatedAt/UpdatedAt exist; EventDate is a single event-time point (kind=event only); StaleAfter is decay TTL rather than validity. | Event-time interval absent; relative-date resolution absent (raw text stored verbatim). |
| Edge invalidation on contradiction: new edge triggers fetch of same-entity-pair and semantically related edges; LLM flags contradicted facts; invalid_at = new.valid_at; new information wins; old facts kept and queryable historically. | supersede_memory_item has the same lifecycle semantics (old cell kept, status flipped) but is manual: a writer must notice the contradiction. | Auto-detection at write time absent. The primitive to wire it to already exists. |
| Hybrid search channels: per scope, parallel cosine + BM25 + optional BFS n-hop; four scopes (edges/nodes/episodes/communities) searched concurrently. | Trigram/keyword lexical + per-subject HNSW, RRF fusion (rrfK=60), single scope, single shot. The lexical full-coverage confidence short-circuit has no Graphiti equivalent and is a real latency advantage. | At parity on channels for one scope. Missing multi-scope fan-out and the BFS channel. |
| Rerankers: RRF, MMR, episode-mentions, node-distance, cross-encoder. LoCoMo run used cross_encoder on edges only. | RRF only. | Cross-encoder absent, and it is the one reranker the winning config uses. |
| Graph expansion at query time: BFS from result nodes or recent episodes into candidates before rerank. | ThoughtLinks (closed relation enum, out-degree capped) + get_memory_context(anchor) traversal exist but are not wired into Search. | Wiring gap rather than substrate gap. Zep's LoCoMo config did not use BFS either. |
| Community summaries: label-propagation clustering, hierarchical LLM summarization into community nodes. | Knowledge pass distilling operational_pattern/stable_fact cells approximates thematic rollups. | Rough analog exists. Zep did not query communities on LoCoMo. Low priority. |
| Session/user model: group_id partitions the graph; per-episode created_at first-class. LoCoMo ingestion: one group per conversation, speaker prefixed into text, session datetime as created_at, BLIP captions inlined. | Per-subject partitioning + tenant tags. | None material. |

## Attribution against our measured failure modes

Temporal (recall 70.1%, accuracy-given-evidence 20.4%). Addressed by
ingest-time date resolution plus rendering dates in reader context. Verified in
their code in both places. Our own extraction arm with resolved dates measured
+11.2pp on temporal. Confidence HIGH: it attacks exactly the
accuracy-given-evidence hole, our largest per-category deficit.

Multi-hop (needs avg 3.1 evidence turns; all-retrieved 20.6% vs any 69%).
Addressed by entity resolution creating aggregation points rather than by graph
traversal. Deduped entities pre-join facts across sessions in the node summary,
and atomic facts make each hop a separate compact retrievable unit. Their
LoCoMo run used no BFS, so traversal cannot be the source of the lift.
Corroborating: LongMemEval multi-session +30.7% in the paper. Confidence
MEDIUM-HIGH (mechanism verified, no per-category LoCoMo breakdown published).

Retrieval-bound overall (accuracy 56.5% with gold evidence vs 8.8% without;
embedder ceiling 70-76% any-recall; 768-dim = 256-dim). Addressed by changing
the retrieval unit plus cross-encoder rerank. Atomic single-relation facts
embed better than 100+-token turns; our finding that dims change nothing while
k=50 adds only +4.1pp says the ceiling is granularity, not embedder capacity.
Confidence MEDIUM (granularity effect inferred; rerank use verified).

Extraction redistributes (our facts arm: temporal +11.2, single-hop -8.3).
Zep's mitigations, both verified: the specificity-preserving extraction prompt,
and dual storage (episodes kept beside facts). Their LoCoMo config retrieved
only facts+entities, and their LongMemEval single-session-assistant regression
(-17.7%, 94.6 to 80.4, conceded in the paper) is the same phenomenon as our
single-hop -8.3. Confidence HIGH that dual storage is the correct design, and
verified that even Zep has not fully solved verbatim-detail loss.

## Is Neo4j load-bearing? No.

What Graphiti demands of its store: string-keyed nodes/edges, a BM25 fulltext
index (Neo4j's is embedded Lucene), cosine vector search, bounded n-hop
traversal, predefined Cypher. It also runs on FalkorDB and Neptune, and Neptune
needs an external OpenSearch cluster bolted on for the fulltext channel, which
shows the graph database is really four generic capabilities. xray already has
three of the four in-process (HNSW, trigram/keyword index, typed links
traversable in memory); bounded BFS over ThoughtLinks is an in-memory map walk.
The score comes from the data model (atomic dated facts, resolved entities,
kept episodes) and the pipeline. Do not adopt a graph database to chase this.

## Ranked refinement plan

Ranked by measured impact per effort. Substrate = changes cell schema/store;
pipeline = layer over existing primitives.

| # | Item | Expected bench impact (grounded) | Product relevance (ops memory) | Effort | Type |
|---|---|---|---|---|---|
| 1 | Render timestamps in reader context and resolve session date at ingest. Stamp every ingested cell's EventDate from the source timestamp; render `(event_time: ...)` per retrieved item as Zep's template does. | Temporal accuracy-given-evidence is 20.4% with recall at 70.1%; extraction-arm evidence says >= +11pp temporal; likely +3-5pp overall. | HIGH. Incident recall is date arithmetic too. | S | Pipeline. Uses MemoryItem.EventDate (widen rendering, no schema change). |
| 2 | Ingest-time atomic-fact extraction beside raw cells (dual storage). Per human-source episode (chat, meeting, webhook; not per k8s event), one LLM call extracts dated atomic facts as stable_fact cells with provenance ThoughtLinks to the source cell; raw cell kept; reader retrieves both populations. | Attacks granularity ceiling, multi-hop assembly, and temporal at once. Dual storage prevents the measured single-hop -8.3 redistribution. Plausible path from 37% toward 55-60% on LoCoMo core. | HIGH for human ingress. Machine pipeline already has budget-gated LLM stages to hang this on. | M | Pipeline. Uses existing kinds, EventDate, author tags, ThoughtLinks. IMPLEMENTED (read side only) 2026-09-12 (`SetSourceCollapse`/`collapseToSources` + `SetLinks` in `internal/xray/memory.go`, default off). Note: this ships Search's collapse-fact-to-episode step and the ThoughtLink write path an extraction pipeline needs to use it, not the extraction itself — the LLM step that produces the episode + facts split lives in the `xray-recall-evals` harness (`locomo/ingest.go`) for now, not in the server ingest path. |
| 3 | Query-time decomposition (multi-query). For multi-clause questions, 2-3 subqueries, union, RRF-fuse. | Multi-hop all-retrieved 20.6% vs any 69% is a single-query assembly failure by our own measurement. Largest multi-hop lever without an entity layer. | MEDIUM. RCA questions are multi-part; adds an LLM call, so gate on query shape and keep out of voice latency path. | S/M | Pipeline (caller/MCP layer; Search stays LLM-free). |
| 4 | ThoughtLink expansion of results. After top-k, pull 1-hop link neighbors and shared-tag siblings into candidates, re-fuse. | Modest unless #2 builds provenance links first, then it recovers raw detail behind a matched fact and patches redistribution. Zep's winning config used no BFS. | HIGH. Incident-pattern-fact chains already exist in live data. | S | Pipeline. IMPLEMENTED 2026-08-14 (expandViaLinks in memory.go, SetLinkExpansion default off). Note: this was genuinely new wiring — MemoryCellSpec.Links was plumbed through cellsync but never read by any consumer, and get_memory_context never traversed links (plain fetch, retired in ADR-051). #1 also implemented same day (eventtime.go, event_time annotation in MCP + voice search renders, event_date validation on upsert). |
| 5 | LLM rerank of top-30 to 10 (cross-encoder role). | Bounded by candidate recall (k=50 bought +4.1pp), expect +2-4pp. Do after #2 changes the candidate pool. | MEDIUM. Latency budget on voice; fine for MCP/System-2 callers. | M | Pipeline. IMPLEMENTED 2026-09-12 (`SetReranker`/`vectorstore.HTTPReranker` in `internal/xray/memory.go`, `internal/vectorstore/reranker.go`, nil/default off). Note: a cross-encoder second stage over the fused RRF pool only — never the lexical confident short-circuit — falling back to RRF order on a reranker error or empty result; `candidateFanout` (3) widens both channels' over-fetch so there is a pool to rerank. The extraction that populates the candidate pool with atomic facts (item #2) still lives in the `xray-recall-evals` harness (`locomo/ingest.go`), not the server ingest path. |
| 6 | Validity intervals and auto-invalidation on the supersede primitive. On stable_fact write, search same-subject semantically near facts, LLM contradiction check, auto-supersede with invalid_at = new.valid_at semantics (new information wins, old cell kept). | Near zero on LoCoMo (no knowledge-update questions there). LongMemEval knowledge-update is where it scores (Zep +6.5% there). | HIGH. The most product-shaped item here: ops facts churn (ownership, endpoints, runbooks) and stale-fact poisoning is a live substrate risk. | M | Substrate-adjacent (semantic on existing supersede + StaleAfter; possibly validFrom/validUntil on the cell). |
| 7 | Entity resolution layer and entity summaries. | The remaining multi-hop delta after #2-#4; Zep's biggest structural differentiator. | LOW-MEDIUM. For the ops domain the service graph already is the resolved entity layer; a second one for people/teams duplicates it. | L | Substrate. Defer until #1-#4 are measured. |
| 8 | Community summaries. | Not exercised in Zep's LoCoMo run; knowledge pass approximates it. | LOW | L | Skip. |

Sequencing: #1 and #3 are independently shippable and measurable on the
existing hack/locomo harness. #2 is the big bet; evaluate against the harness's
extraction arm as baseline. #6 should be built regardless of benchmarks because
it hardens the substrate xray actually sells.

## Verdict on chasing LoCoMo

LoCoMo is a compromised yardstick. Conversations average 16-26K tokens and fit
any modern context window, so the ~73 full-context baseline beats nearly every
memory system, which indicts the benchmark. Category 5 shipped without usable
ground truth. It never tests knowledge updates. The leaderboard is a vendor
dispute: Mem0 published 65.99 for Zep, Zep rebutted with 75.14 after fixing
Mem0's ingestion errors, and Mem0 counter-filed that Zep's earlier 84% claim
corrects to 58.44% (zep-papers issue #5). Keep our LoCoMo harness as a cheap
regression instrument for retrieval mechanics, where our failure-mode
instrumentation is already better than what either vendor published, and aim
the refinement at LongMemEval's categories, which map nearly one-to-one onto
xray's real workload: knowledge-update is the supersede/validity lifecycle
(#6), temporal-reasoning is #1/#2, multi-session is cross-incident recall
(#3/#4). Zep's own LongMemEval single-session-assistant regression (-17.7%)
shows verbatim-detail loss is the open flank in this architecture class;
xray's dual-storage design (#2) plus the lexical confidence short-circuit,
which Graphiti has no equivalent of, is a differentiated answer to it.
