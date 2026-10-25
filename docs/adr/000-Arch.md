# ADR-000: Architecture Overview

**Status:** Reference
**Date:** 2026-06-06
**Author:** Denys Vasyliev

---

## Overview

X-Ray is a **Kubernetes-native associative-memory system** for agentic and SRE
runtimes. Its core idea: use an **in-memory HNSW graph not merely as a vector
index, but as a living semantic graph** — the similarity engine and the
topological structure at once. Nodes are memory cells (and, in the SRE domain,
services, events, incidents); edges are a mix of embedding-space proximity and
explicit typed relations the system adds deliberately.

This document is the canonical, high-level map. Deeper decisions live in
[ADR-020 (Associative Memory Architecture)](020-associative-memory-architecture.md);
this overview ties them together and records the rationale.

## The Core Idea: HNSW as a Living Graph

A single in-memory HNSW graph backs several views:

```
HNSW graph (in-mem)
├── Service graph      → nodes = services,  edges = dependencies + semantic proximity
├── Event correlation  → nodes = events,    edges = temporal + causal + semantic
├── Incident graph     → nodes = incidents, edges = correlation + causality
└── Mem-cells          → nodes = memory units, edges = associative proximity
```

The key difference from "just HNSW inside Qdrant": the graph is controlled
**directly**. The system can add typed edges, custom traversal logic, and decay
functions for memory cells. At retrieval time a query returns not a flat top-k of
chunks but the **sub-graph around the query node** — which is the natural shape
for service dependencies and incident correlation.

## Why Not Standard RAG

| Concern | Simple RAG | Graph RAG | Fractal RAG | ColBERT/Tensor | X-Ray HNSW |
|---|---|---|---|---|---|
| Retrieval | flat top-k | graph query | hierarchy by text | multi-vector | graph-aware neighborhood |
| Multi-hop | ❌ | ✅ | ⚠️ | ⚠️ | ✅ via causal-chain traversal |
| Persistence | external store | external store | external store | external store | **etcd-native** |
| Incremental update | append | hard (rebuild summaries) | hard | ok | native HNSW insert |
| Memory governance | ❌ | ❌ | ❌ | ❌ | **approval gate** |
| Shared memory | manual sync | manual sync | manual sync | manual sync | **K8s reconcile** |
| Poisoning protection | ❌ | ❌ | ❌ | ❌ | ✅ |
| Operational cost | low | high | medium | high | medium (if K8s already present) |
| Latency | store-dependent | store-dependent | store-dependent | high | sub-ms (in-mem) |

- **vs Simple RAG** — retrieval returns a neighborhood with graph context, not
  isolated chunks; updates are in-place and can rewire edges.
- **vs Graph RAG** — X-Ray builds an *implicit* semantic graph (proximity) plus
  *explicit* typed edges, without an expensive NER/relation-extraction pipeline.
  For incidents this matters: an incident has no predefined typed relations — it
  emerges from a proximity pattern among anomalous signals.
- **vs Fractal RAG** — HNSW layers are already hierarchical (coarse→fine), so the
  index is fractal-like natively; traversal goes coarse to fine automatically.
- **vs ColBERT/Tensor** — instead of enriching the vector representation, X-Ray
  enriches the *topological* context; for event correlation the relationships
  between events matter more than each event's internal semantics.

## Where This Design Wins

1. **Associative memory for interactive models.** Memory cells are not stateless
   retrieval: a new event embeds, finds its neighborhood, strengthens edges to
   correlated nodes, and lets distant edges decay — "forgetting" via graph
   pruning. This is closer to episodic memory than any flat RAG.
2. **Correlation without a schema.** You don't know in advance what correlates
   with what; HNSW finds clusters of anomalies by proximity, where a typed graph
   would need predefined relation types.
3. **Zero external dependencies on the hot path.** In-process, sub-millisecond,
   fully controlled — an agent never blocks on a network round-trip to a vector DB.

## Kubernetes-Native Read/Write Split

X-Ray separates the read and write paths around a governance gate:

```
┌──────────────────────────────────────────────┐
│                 etcd (K8s)                     │
│        MemoryCell CRs  ←  approved only        │
└───────────────┬────────────────────────────────┘
                │ watch / reconcile
                ▼
┌──────────────────────────────────────────────┐
│             MemoryCell Controller              │
│   seed on start → sync on change → update HNSW │
└───────────────┬────────────────────────────────┘
                │ graph updates (immediate)
                ▼
┌──────────────────────────────────────────────┐
│        In-Memory HNSW Graph (per agent)        │
│   reads: instant  │  writes: local → gate      │
└──────────────────────────────────────────────┘
```

- **Read path** — each agent holds its own graph, but all converge to one state
  through the controller: eventual consistency without distributed locking. This
  is level-triggered semantics applied to memory.
- **Write path** — an agent generates a cell locally → approval gate → only then
  a CR in etcd → other agents' controllers pick it up. In effect, **memory
  consensus through Kubernetes admission.**

The graph rebuilds from the source of truth (etcd) at startup — not a bug but a
feature: the graph is always consistent with what was approved.

## Memory Governance: A Three-Tier Approval Pipeline

Governance is a first-class concern, absent from standard RAG. A model may
"think" freely in local, ephemeral memory, but only verified knowledge enters the
shared base — solving **memory poisoning** in multi-agent systems (one agent's
hallucination must not infect the others).

```
agent local cell
   │
   ▼  L1: Guardrails (OPA/CEL)   fast, deterministic, cheap — schema/confidence/safety
   ▼  L2: Model-as-Judge         semantic validation — "is this even true?"
   ▼  L3: HITL                    only flagged / high-stakes — human decision
   ▼
MemoryCell CR → broadcast to all agents
```

- **L1 Guardrails** — a gate on *form and safety*, not content. Sub-ms,
  deterministic, stateless, no model call. Rejects bad embeddings, out-of-schema
  structures, untrusted sources.
- **L2 Model-as-Judge** — *semantic* validation: factual consistency against the
  existing graph, causal plausibility, novelty-vs-noise, confidence calibration.
  The Judge sees the new cell plus its HNSW neighborhood and returns
  approved/rejected/escalate with reasoning for the audit log. Critically, the
  Judge should not be the same model that generates cells (avoid a
  self-validation loop) — ideally a different size or provider.
- **L3 HITL** — receives only escalated cases (Judge uncertain, or a critical
  domain like an incident root-cause / a new service dependency). The human reads
  a pre-reasoned case with the Judge's verdict, not raw cells — accountability
  without bureaucracy.

This is, in effect, a **memory immune system**: malformed cells → L1; agent
hallucination → L2; adversarial injection → L1+L2; ambiguous causal attribution
→ L3; silent knowledge drift → L2's novelty check.

## Scope: Memory Only

X-Ray stores, indexes and serves memory. It does not run model-driven background
passes: consolidating, summarising and generalising memory (what earlier drafts of
this design called reflection and knowledge loops) is the job of the agent using
X-Ray, which writes the result back through `upsert_memory`, marking what it replaces
with `supersedes`.

The only background work is deterministic retention: items past their `staleAfter`
deadline, and episodic items past `--episodic-ttl`, are deleted by a sweep. It makes no
model calls and its deletes are idempotent, so running it on several replicas is safe.

## Consequences

- **Operational simplicity where K8s already exists** — etcd is the durable
  store, a MemoryCell CR is just a schema, and watch/reconcile gives shared memory
  without bespoke coordination.
- **Safety by construction** — the approval pipeline and soft-delete audit trail
  make memory tamper-evident and recoverable.
- **The open question** is the approval policy mix (OPA/CEL thresholds vs
  model-as-judge vs HITL) per domain — this is the memory-safety layer of the
  system and is tuned per deployment.

## Related

- [ADR-020](020-associative-memory-architecture.md) — Associative Memory
  Architecture: in-memory HNSW with Kubernetes-native persistence (the concrete
  implementation of the read/write split and kind taxonomy).
- [ADR-023](023-hnsw-lock-free-rebuild.md) — HNSW lock-free graph rebuild via
  pointer swap.
