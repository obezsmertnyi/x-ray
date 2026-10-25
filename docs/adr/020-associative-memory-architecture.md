# ADR-020: Associative Memory Architecture — In-Memory HNSW with Kubernetes-Native Persistence

**Status:** Implemented  
**Date:** 2026-05-19  
**Author:** Denys Vasyliev

> **Scope note:** the reflection and knowledge loops described below were removed from X-Ray.
> X-Ray is the memory substrate only; consolidation is done by the calling agent through
> `upsert_memory`. The storage, indexing and retention parts of this ADR still apply.

---

## Context

X-Ray requires a memory system for agentic runtimes that supports:

- Sub-millisecond semantic retrieval without external service dependencies
- Multi-agent shared memory without distributed coordination
- Memory governance — preventing hallucination propagation across agents
- Incremental updates without full graph rebuilds
- Temporal consolidation across operational, episodic, and semantic timescales

Standard RAG approaches (flat vector stores, Graph RAG, ColBERT, Fractal RAG) were evaluated and found insufficient for the SRE and agentic use cases described below.

---

## Decision

Use an **in-process HNSW graph** as the unified memory substrate, backed by **Kubernetes MemoryCell CRs** as the durable source of truth, with a **three-tier approval pipeline** and **dual consolidation loops**.

---

## Memory Kind Taxonomy

Each MemoryCell has a `kind` field that maps to a real cognitive category. Kinds determine how the model reasons about an item, how long it lives, and where it appears in the Voronoi graph UI.

| Kind | Meaning | Expires? |
|------|---------|----------|
| `system_identity` | What this platform is — tech stack, namespace, topology, people | No |
| `stable_fact` | High-confidence durable truth about system behaviour | No |
| `operational_pattern` | Recurring behaviour: self-heals, false negatives, known noise | No |
| `active_concern` | Cross-system issue being monitored right now | No |
| `hypothesis` | Unverified root cause worth tracking | Yes (`stale_after` required) |
| `unknown` | Open question that should bias future triage | Yes (`stale_after` required) |
| `watch_item` | Something to watch but not act on yet | No |
| `recent_change` | Deployment or config change with potential side effects | No |
| `event` | Time-bound event with a specific date | No |
| `agent_note` | Session-scoped ephemeral note — never persisted to CR | Session only |
| `system_configuration` | Voice session config, tool allowlists — reserved, not user-facing | No |

**Item schema fields:**

| Field | Type | Notes |
|-------|------|-------|
| `id` | string | Stable identifier |
| `kind` | enum | See above |
| `scope` | `global\|domain\|service` | `global` = user-facing default |
| `subject` | string | Service, person, or domain name |
| `title` | string | Short label |
| `content` | string | Markdown body |
| `confidence` | `low\|medium\|high` | `stable_fact` must be high |
| `status` | `active\|stale\|superseded` | |
| `evidence_refs` | []string | `incident:`, `event:`, `service:` refs |
| `stale_after` | time? | Required for `hypothesis` and `unknown` |
| `tags` | []string | Used for UI page routing (e.g. `project`, `contributor`) |
| `data` | `map[string]DataEntry` | Named blobs — e.g. `data["image"]` for thumbnails |
| `links` | []ThoughtLink | Typed directed edges: `triggers`, `caused_by`, `correlates_with`, `precedes`, `explains`, `contradicts` |

`hypothesis` and `unknown` items expire automatically — stale speculation does not accumulate in the graph.

---

## Architecture

### Memory Graph Structure

The HNSW graph serves as a unified semantic index across four memory domains:

```
HNSW graph (in-mem)
├── Service graph         → nodes = services, edges = dependencies + semantic proximity
├── Event correlation     → nodes = events, edges = temporal + causal + semantic
├── Incident graph        → nodes = incidents/alerts, edges = correlation + causality
└── Mem-cells             → nodes = memory units, edges = associative proximity
```

The critical distinction from "HNSW in Qdrant": the graph is controlled directly — typed edges, traversal logic, and decay functions are first-class. HNSW is both the similarity engine and the topological structure simultaneously.

### Read/Write Path

```
┌─────────────────────────────────────────────────────┐
│                    etcd (K8s)                        │
│         MemCell CRs  ←  approved only               │
└──────────────┬──────────────────────────────────────┘
               │ watch / reconcile
               ▼
┌─────────────────────────────────────────────────────┐
│              MemCell Controller                      │
│   seed on start → sync on change → update HNSW      │
└──────────────┬──────────────────────────────────────┘
               │ graph updates (immediate)
               ▼
┌─────────────────────────────────────────────────────┐
│         In-Memory HNSW Graph (per agent)             │
│   reads: instant  │  writes: local → approval gate  │
└─────────────────────────────────────────────────────┘
```

**Read path:** each agent has its own graph; all converge to the same state via the controller — eventual consistency without distributed locking. This applies level-triggered semantics to memory.

**Write path:** agent generates mcell locally → approval gate → only then CR in etcd → other agents' controllers pick it up. Memory consensus via Kubernetes admission.

---

## Comparison with Alternatives

| | Simple RAG | Graph RAG | Fractal RAG | Tensor/ColBERT | This HNSW |
|---|---|---|---|---|---|
| Multi-hop | ❌ | ✅ | ⚠️ | ⚠️ | ✅ causal chain |
| Persistence | external store | external store | external store | external store | etcd native |
| Memory governance | ❌ | ❌ | ❌ | ❌ | approval gate |
| Shared memory | manual sync | manual sync | manual sync | manual sync | K8s reconcile |
| Incremental updates | append | complex | complex | ok | controller watch |
| Memory poisoning protection | ❌ | ❌ | ❌ | ❌ | ✅ |
| Operational complexity | low | high | medium | high | medium (if K8s exists) |
| Latency | store-dependent | store-dependent | store-dependent | high | sub-ms |

**Key advantage for SRE use cases:** incidents do not have pre-defined typed relations. HNSW finds anomaly clusters via proximity, while Graph RAG would require pre-defined relation types.

**Key advantage for agentic runtimes:** in-process, sub-ms, full control — no network round-trip to an external vector store.

---

## Approval Pipeline

Memory writes pass through a three-tier defense-in-depth gate:

```
agent local mcell
        │
        ▼
┌───────────────────┐
│  L1: Guardrails   │  ← fast, deterministic, cheap
│  Policy (OPA/CEL) │    confidence, schema, safety rules
└───────┬───────────┘
        │ pass / reject (immediate)
        ▼
┌───────────────────┐
│  L2: Model-as-    │  ← semantic validation
│  Judge            │    "is this actually true?"
└───────┬───────────┘
        │ approved / flagged / rejected
        ▼
┌───────────────────┐
│  L3: HITL         │  ← flagged or high-stakes only
│                   │    human decision → etcd
└───────┬───────────┘
        │
        ▼
    MemCell CR → broadcast to all agents
```

**L1 — Guardrails:** validates form and safety, not content. Checks confidence threshold, trusted source, TTL bounds, sensitive pattern exclusion, embedding dimensions. Sub-ms, synchronous, stateless.

**L2 — Model-as-Judge:** validates semantic content. Checks factual consistency against existing graph, causal plausibility, novelty vs noise, confidence calibration. Judge receives the new mcell, its HNSW neighborhood (top-k), and relevant historical mcells. Returns `approved | rejected | escalate_to_hitl` with reasoning. Judge must differ from the generating model to avoid self-validation loops.

**L3 — HITL:** receives only escalated cases — Judge uncertainty or high-stakes domains (incident root cause, new service dependencies, modifications to existing causal edges). Human reads pre-analyzed case with Judge verdict and reasoning, not raw mcell.

### Threat Model

| Threat | Blocked by |
|---|---|
| Malformed / low-quality mcell | L1 Guardrails |
| Agent hallucination | L2 Judge |
| Adversarial memory injection | L1 + L2 |
| Ambiguous causal attribution | L3 HITL |
| Silent knowledge drift | L2 novelty check vs existing graph |

---

## Consolidation Loops

Two asynchronous cycles with different cadence, prompts, and goals — orthogonal by design, not competitive.

```
─────────────────────────────────────────────────────────────────
  0h    3h    6h    8h    9h   12h   16h   24h
  │     │     │     │     │    │     │     │
  ├─────┤─────┤     ├─────┤───────────────┤
  
  [reflection] [reflection] [──── knowledge ────]
  
  working memory           long-term memory
  consolidation            conceptual update
─────────────────────────────────────────────────────────────────
```

### Reflection Loop (3h) — Episodic Memory

Prompt: *"what happened in the last 3 hours, what to keep, what to remove?"*

- Operates on episodic events with timestamp delta
- Prunes duplicates (cosine sim > 0.97), contradicted mcells, closed-incident noise, zero-degree nodes
- Consolidates related episodic mcells into single summary mcells
- Promotes frequently-read mcells (confidence bump) and confirmed causal edges

### Knowledge Loop (8h) — Semantic Memory

Prompt: *"what patterns do I see across several reflection cycles?"*

- Operates on consolidated mcells with pattern frequency
- Generalizes specific incidents into reusable rules
- May override or supersede earlier knowledge
- Reads a different part of the graph — minimal overlap with reflection

**Why no conflict:** they operate on different graph partitions (fresh vs consolidated mcells) with different prompts. If they run concurrently, worst case reflection adds one mcell that knowledge misses in this cycle — seen next cycle. This is the eventual consistency already baked into the architecture. Cadence is a form of isolation, analogous to namespaces in Kubernetes.

### Three Timescales

```
Operational  (real-time)     agents act, mcells written
     │
     ▼ 3h
Episodic     (reflection)    what happened, consolidation
     │
     ▼ 8h  
Semantic     (knowledge)     what it means, generalization
     │
     ▼ morning
Social       (human audit)   do we agree with what the model learned
```

This maps to the human memory taxonomy: episodic → semantic via consolidation, with a human as external anchor preventing full autonomous drift.

### Model Agency in Consolidation

The model exercises judgment, not just algorithm:

- Is this signal or noise?
- Is this causal link real or spurious correlation?
- Does this mcell contradict what I know — or was I wrong before?
- Should I preserve a contradiction as an open question rather than force-resolve?

Forced consistency loses signal. The model may retain two conflicting mcells as an open question.

### Reflection Agent Permissions

The Reflection agent is the only agent with delete rights on approved mcells. To prevent it becoming a single point of trust failure, deletions are soft: mcells are marked `as_pruned`, excluded from HNSW traversal, but retained for audit and rollback.

---

## Morning Conversational Audit

Instead of a diff dashboard, a human converses with the model to audit knowledge changes:

```
human: "what's new about the payments service?"
model: "I noticed payments failures correlate with database 
        connection pool exhaustion, but only on Mondays — 
        stored as weak hypothesis, needs confirmation"
```

Semantic drift is visible through behavior, not vector comparison. The model explains its own reasoning. HITL without friction:

```
human: "why did you delete the Tuesday incident mcell?"
model: "it contradicted three newer observations and confidence 
        was low — if you think it's important, I can restore it"
```

---

## Consequences

**Positive:**
- Zero external dependencies for retrieval — in-process, sub-ms
- Memory governance as a first-class primitive — hallucination from one agent cannot infect shared knowledge
- Multi-agent shared memory without coordination — agents watch the controller, not each other
- Graph rebuilds from source of truth on startup — always consistent with what was approved
- HNSW is natively hierarchical (fractal-like) — coarse-to-fine traversal without extra infrastructure

**Negative:**
- Memory is lost on pod restart if etcd sync has not completed (mitigated by fast reconcile on startup)
- Approval pipeline adds latency to the write path (mitigated: reads are always instant)
- Reflection and knowledge loops require a capable model — cheap models may produce poor consolidation judgments

**Open questions:**
- Approval gate implementation: all three layers (OPA/CEL policy, model-as-judge, HITL) are in use — L1+L2+L3 as described
- ~~Judge recursive pipeline risk~~ — closed: the Judge is the self model reflecting on real-time events grounded to the existing approved mcell graph; grounding to the approved graph is the safeguard, not a separate validator

---

## Implementation note — episodic write/recall for voice (2026-06-20)

A first concrete slice of the episodic tier landed for the voice agent — the
**write and recall** of episodic memories, not yet the 3h/8h consolidation loops:

- `save_memory(kind=episodic)` (the renamed `note_observation` tool) writes a
  private session memory (the model is steered to save a short one at session
  end). `kind=note` (default) remains the standing-observation path; an
  unrecognized kind falls back to `note`.
- `MemoryStore.ListRecent(kind, scope, subject, limit)` returns items
  **newest-first by `CreatedAt`** — the time-ordering `List`/`listMap` lacked.
- `get_recent_memory` voice tool recalls the N most recent episodic memories
  newest-first (each with `created_at`), so a new session continues from the last.

This realizes "Operational → Episodic (what happened)" at the storage + recall
level. The reflection loop (consolidate/prune episodic → semantic) remains future
work.

A **TTL-based forget mechanism** was added so episodic memory does not grow
without bound (a precursor to the reflection loop's pruning, not a replacement
for it):

- `--episodic-ttl` (default 168h/7d, 0 = never) sets the max age, measured from
  `CreatedAt`.
- `ListRecent` filters expired episodic items at read time — a stale memory stops
  surfacing in `get_recent_memory` immediately.
- `StartEpisodicReaper` (hourly) deletes expired episodic CRs so they don't
  accumulate in etcd.

This is age-based eviction only — it does not consolidate or judge relevance the
way the reflection loop will; an old-but-important memory expires the same as an
old-trivial one. The model is also steered (system prompt) to save an episodic
memory **only** when a session produced something worth remembering, so trivial
sessions don't create memories in the first place.
- ~~Adaptive loop cadence~~ — closed: per-incident reflection trigger fires immediately on incident events, independent of the 3h schedule