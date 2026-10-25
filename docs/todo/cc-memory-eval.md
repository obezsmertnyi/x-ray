# Claude Code memory: markdown files vs xray recall

**Date:** 2026-08-08
**Status:** measured; hybrid search implemented; one parser defect still open

Evaluates whether Claude Code recalls better from its own markdown memory files
or from the same content indexed in xray. The whole CC memory estate (208 files
across 14 projects) was built into a graph snapshot and served over MCP.

## Final architecture (2026-08-08, revised)

No Kubernetes, no xray server, no separate memory store. One binary:

```bash
xray recall serve -repo ccmem=/Users/den/.claude/projects -addr :8084 \
  -snapshot-dir ~/.xray-ccmem \
  -embedding-endpoint http://127.0.0.1:8090 -embedding-model nomic \
  -embedding-dims 256 -embedding-max-input-chars 8192
```

`serve` now warm-seeds from `<snapshot-dir>/<label>.graph.gob.gz` when one is
present (`seedFromSnapshot`, `-no-seed` to force a cold build). Startup went from
**17.5s (565 embed calls) to 22ms (0 embed calls)** — the gob carries the
text→vector cache and the serialized HNSW, so the embedder is only used for
queries. Seeding is conservative and falls back to a cold build on any mismatch:
single repo only, exact embedding-fingerprint match, and the gob's recorded repo
path must still be the path being served.

The walk root is the real memory tree, `~/.claude/projects`, not a staged copy.
`engine.skipDir` rejects any directory whose name begins with `.`, including the
walk root, so `~/.claude` itself cannot be the root — but `projects` can, and its
children are the per-project dirs. Non-markdown files cost one `os.ReadFile`
each and never reach tree-sitter (`detectLanguage` returns empty), so walking
550MB of session transcripts takes 717ms and contributes no nodes.

That makes the markdown files themselves the memory, with xray holding only
vectors that point at them. There is no second copy and no sync problem. A write
is the normal Claude Code memory write followed by
`xray recall update -server http://127.0.0.1:8084 <file>` (~230ms, re-embeds only
that file), wired as a `PostToolUse` hook.

## Setup

- Embedder: llama.cpp, `nomic-embed-text-v1.5.Q8_0`, 256-dim MRL truncation, `:8090`
- Snapshot: `xray recall snapshot -repo ccmem=~/xray-ccmem` → 208 files, 525 nodes,
  319 edges, one embed pass, 17s
- Server: `xray --mental-maps-dir ~/.xray-snapshots`, MCP on `:8083`
- The snapshot's embedding fingerprint must match the server's. The server puts
  the **raw flag value** in the fingerprint, not the resolved default, so
  `--embedding-max-input-chars=8192` has to be passed explicitly or a snapshot
  built at the default 8192 is rejected against a server reporting 0.

## Question set

25 questions in user voice, each with one ground-truth memory file, split by
where the answer lives:

- **Set A (15)** — answer is in the current project's memory
- **Set B (10)** — answer is in a *different* project's memory

Set B is the structurally interesting half: Claude Code only loads the current
project's `MEMORY.md`, so those memories are not in context at all.

## Arms

- **md** — how recall works today: `MEMORY.md` (the one-line index) is in
  context, the model picks which file to open. Simulated by giving a model only
  that index and asking which stems it would open.
- **xray** — `search_graph_docs(question, limit=5)`, scored by the rank at which
  the ground-truth file comes back.

## Results

| Arm | Set A top-1 | Set A top-5 | Set B top-1 | Set B top-5 | All top-1 | All top-5 |
|---|---|---|---|---|---|---|
| md (markdown index) | **100%** | **100%** | 0% | 0% | 60% | 60% |
| xray, k8s server, 208 files | 67% | 80% | 90% | 90% | 76% | 84% |
| xray, index files excluded, 194 | 73% | 80% | 90% | 90% | 80% | 84% |
| **xray, single binary, 227** | 60% | 87% | **100%** | **100%** | 76% | **92%** |

The single-binary corpus adds 18 archived memories from
`memory-archived-coala-seed` plus one new memory. Those near-duplicates cost
top-1 within a project (they outrank the live memory) while top-5 improves.

Recall cost: **7.7ms p50, 8.5ms p90** (query embed 6.8ms + HNSW). An earlier
figure of 114ms in this document was wrong — that harness spawned two `python3`
processes per iteration just to read a clock, and was measuring process startup.

Context cost: the markdown index is **~4,860 tokens paid on every session**, for
one project. xray costs nothing at rest and ~600-900 tokens per query actually made.

## Reading

**The markdown index wins where it applies and cannot apply anywhere else.**
100% on set A is not a fluke: the index is hand-curated, one line per memory,
and a model reasoning over 141 curated lines beats cosine similarity. But set B
is 0% by construction, and that is 40% of the questions. md memory's ceiling here
is 60% and no amount of tuning moves it, because the other projects' indexes are
never loaded.

**xray is worse in-project and is the only thing that works cross-project.** It
loses 3 of 15 set-A questions to genuine semantic misses:

- "who is supposed to write the implementation code" → returned memory-loop and
  awareness-cell docs; the target is phrased as "Sonnet subagents / tech lead"
- "I edited the jucce cli source, what do I need to run?" → matched the `jucce`
  topic (`project_jucce_identity`) instead of the actionable rule
- "how should the whitepaper prose read?" → complete miss

These are the failure mode already documented for this embedder: short queries
whose target is phrased in different vocabulary lose to longer, topically-adjacent
prose.

**Excluding the 14 `MEMORY.md` index files barely helped.** They looked like
attractors (one query returned nothing but index files), so the corpus was
rebuilt without them, 208 → 194 files. Top-1 went 76% → 80%; top-5 did not move
and **none of the four misses were fixed**. The hypothesis was mostly wrong.

## Index health

Each of the 194 documents was queried with its own `description:` line, which
should return that document first.

- rank 1: **190 (97.9%)**
- rank 2-10: 2 (1.0%)
- **not retrievable at all: 2 (1.0%)**

## Hybrid search (implemented)

`internal/recall/index/lexical.go` adds a keyword channel and fuses it with the
semantic one by Reciprocal Rank Fusion in `Index.Search`. Ported from triage's
`kwindex.go` + `memory_search.go` (ADR-030): per-word trigram index with
length-scaled match thresholds, `lexScore` over title/content with a
per-missing-token penalty, `rrfK=60`.

RRF rather than a weighted blend because the channels' scores are not comparable
— cosine sits in a narrow 0.5-0.8 band while `lexScore` is unbounded integers —
so only ranks can be combined. Both channels over-fetch to `3k` so an item
ranked 8th semantically and 1st lexically still surfaces.

Three design points that only emerged from measurement:

- **Stopwords are mandatory.** A question is mostly stopwords, and indexing them
  makes every document match every query.
- **The coverage gate cannot demand every token.** Requiring all tokens rejected
  the exact documents it should return: "who is supposed to write the
  implementation code on this project?" carries `supposed`, which the answering
  memory does not contain even though it holds every other token several times.
  The gate is `ceil(2n/3)` for n>2 tokens (all of them for n≤2), with the miss
  penalty keeping full coverage ranked above partial.
- **Lexical-only hits are stamped with their true cosine.** Otherwise they
  surface with `Score: 0` and a caller reading Score as similarity sees a hit
  that looks broken.

### Effect

| Arm | A top-1 | A top-5 | B top-1 | B top-5 | All top-1 | All top-5 |
|---|---|---|---|---|---|---|
| semantic only | 60% | 87% | **100%** | 100% | **76%** | 92% |
| **hybrid** | **67%** | **100%** | 80% | **100%** | 72% | **100%** |

**Zero misses.** Both documents that no semantic query could reach are now found:
`feedback_jucce_cli_always_build` and `feedback_subagents_always_sonnet`.

The trade is real and worth stating: **top-5 goes to 100% everywhere, top-1
drops 4 points overall** because fusion promotes lexical candidates that
sometimes displace a correct semantic top-1 (set B, 100% → 80%). For an agent
that reads several results this is the right trade — nothing is unfindable —
but a caller that consumes only the first hit is slightly worse off.

Cost: search **7.7ms → 24.5ms p50** (36.5ms p90). Index health improved:
**96.2% rank-1** and **0.5% unreachable**, up from 94.8% / 0.9%.

Tests: `internal/recall/index/lexical_test.go`.

## Freshness caveat

A seeded graph is exactly as fresh as its gob. During this work a memory written
after the last flush showed as unreachable in the health test purely because the
graph predated the file; one `xray recall update` fixed it. The `PostToolUse` hook
exists to make that automatic, and a flush on shutdown (`snapshot-interval`, or
SIGTERM) rolls new nodes back into the gob — verified end to end: flush wrote 567
nodes, restart re-seeded them in 61ms.

## Superseded: the case for building this

The two remaining misses both have strong lexical support in their target and
lost anyway, which is the signature of a missing keyword channel rather than a
tuning problem:

- "I edited the jucce cli source, what do I need to run?" — the target contains
  `jucce` ×6, `cli` ×6, `build-cli` ×4, and lost to the jucce *topic* document.
- "who is supposed to write the implementation code on this project?" — the
  target contains `implementation` ×4, `code` ×4, `write` ×2, `Sonnet` ×15.

Neither `xray/internal/recall/index` nor triage's `internal/corpusmaps` has any
lexical index — both are pure cosine. The hybrid lexical-plus-semantic search
that does exist in triage (`internal/triageagent/kwindex.go`, trigram index plus
Levenshtein, ADR-030) is wired only to **memory cells**, not to the map/corpus
path. So there is nothing in triage's map search to port that is better than
what xray already has; the thing worth porting is triage's *cell* search into
the map path, which triage has not done for its own corpus maps either — its
CLAUDE.md lists exactly this as the proposed, unimplemented fix.

Implemented — see "Hybrid search" above for the design and the measured effect.

## Open defect: one memory is unreachable by any query

`vllm-macos-embeddings-build.md` (present in both `code-xray/` and
`code-other/`, so 2 files, 1 unique memory) cannot be retrieved by:

- its own `description:` line verbatim
- a near-verbatim quote of a distinctive sentence in its body
- the fake title the parser assigned it
- its section-level `qn` (reports `node not found`)

It is present in the graph, fetchable by its Doc-level `qn` with complete and
correct content, and counted in `Embedded: 442`. So it is indexed and carries a
vector, but no query reaches it.

The one trait separating it from the 192 healthy documents: it is the only file
whose fenced code block contains a line starting with `# `, and the parser
promoted that comment to a heading. `get_graph_node` shows the damage in the
title:

```
"title": "build backend deps must be installed manually when using --no-build-isolation:"
```

**Causation established by the fix.** Rewriting that one file as prose
paragraphs with no fenced `# ` line, then rebuilding, moved it from unreachable
to **rank 1 at score 0.784** for the natural-language query it previously failed.
The earlier theory that the node had been embedded on its title was already
disproven (querying the fake title also failed), so the rewrite is what settled
it. Fence handling in `internal/recall/docs` still needs the actual fix; the
rewrite only removed the trigger from one file.

**A second, unrelated failure mode exists.** Re-running index health on the
final 212-document corpus: **201 rank-1 (94.8%), 9 at rank 2-10, 2 unreachable**.
The vLLM file is now healthy; the 2 unreachable are
`project_inmemory_stores.md` (the same memory in two projects), which has **no
fenced blocks and a correct title**. That one is ranking competition, not the
parser bug. Note the rank-1 rate fell from 97.9% to 94.8% when the 14
`MEMORY.md` index files and the archived seed joined the corpus — more
near-duplicate competition.

**A retracted claim, recorded so it is not re-derived.** During this work the
vLLM file appeared unreachable once on a snapshot-seeded server after an
incremental `update`, which looked like the seed path corrupting the index. It
did not reproduce: seeded-then-updated returns it at rank 1, matching cold build.
There is no evidence of a seed/update interaction defect.

## Reproducing

```bash
# embedder
llama-server --host 127.0.0.1 --port 8090 --embeddings -b 4096 -ub 4096 -c 4096 \
  -ngl all --model <nomic-embed-text-v1.5.Q8_0.gguf>

# stage CC memory (skipDir rejects dot-dirs, so ~/.claude cannot be the walk root)
# then build + serve
xray recall snapshot -repo ccmem=~/xray-ccmem -snapshot-dir ~/.xray-snapshots \
  -embedding-endpoint http://127.0.0.1:8090 -embedding-model nomic -embedding-dims 256
xray --mental-maps-dir ~/.xray-snapshots --embedding-max-input-chars=8192 ...
```

Scripts: `eval-xray.sh` (retrieval ranks), `selftest.sh` (index health),
`questions.tsv` (ground truth).

## What this implies for using xray as CC memory

Neither surface dominates, so use both rather than replacing one with the other:
the markdown index stays authoritative and is what wins in-project, and xray is
what makes the other 13 projects' memory reachable at all. That is the dual-write
arrangement the `xray-memory` skill describes — write markdown as usual, mirror
into xray, and search xray first only because it is the only surface that spans
projects.
