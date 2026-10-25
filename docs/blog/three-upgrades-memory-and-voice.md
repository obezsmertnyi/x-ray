# Three upgrades to X-Ray's memory and voice stack

A short tour of three changes that landed recently — how X-Ray embeds memory,
how it spends tokens on search, and how the voice persona now converses.

---

## 1. Local embeddings

Semantic memory now runs on a **local embedding model** instead of Vertex AI. An
in-pod sidecar (`llama.cpp`) serves **nomic-embed-text**, with vectors reduced to
**256 dimensions** via Matryoshka truncation.

The question that mattered: can a local model match Vertex on **quality**, at a
smaller **vector size**, and lower **latency**? Same 6-query retrieval battery,
same corpus, run through X-Ray's full hybrid search path:

| Config | Dims | top-1 | top-3 | Warm embed latency |
| --- | --- | --- | --- | --- |
| Vertex `gemini-embedding-001` (cloud) | 128 | 5/6 | 6/6 | ~187 ms |
| nomic (local, llama.cpp) | 128 | 4/6 | 6/6 | **~10 ms** |
| **nomic (local, llama.cpp)** | **256** | **5/6** | **6/6** | **~10 ms** |
| nomic (local, llama.cpp) | 768 | 5/6 | 6/6 | ~10 ms |

**Findings:**

- **256d is the knee** — 128→256 recovers the one query 128d lost; 256→768 buys
  nothing on this corpus.
- **256d local nomic ties cloud Vertex on quality** (5/6 top-1, 6/6 top-3) at a
  third the vector size — a smaller HNSW graph, embeddings cached in the cell
  records for free.
- **~18× faster embed call** (~187 ms → ~10 ms warm). Since the embed call was
  ~83% of a semantic search, this turns the round-trip from the bottleneck into a
  rounding error.
- **Private** — embeddings are computed on-cluster; no memory-cell text leaves
  the pod, no external dependency.

---

## 2. Headroom token compression

The biggest token cost in a voice turn isn't the audio — it's **memory-cell
content injected into the model context as search results**. Ten full Markdown
bodies per `search_memory` call is a lot of mostly-redundant text.

We added a pure-Go **Headroom** layer (ADR-038) that compresses search results
before they reach the model:

- **Lossy body trimming** — long content is shortened (full text stays
  retrievable by `id`).
- **Field factoring** — uniform fields are hoisted into a shared block instead of
  repeating per row.
- **No rows dropped** — every candidate still reaches the model, which matters
  when the voice layer mishears and needs alternatives to disambiguate.

Measured reduction (live `search_memory` on the production corpus, and synthetic
long-body tests):

| Content | Reduction |
| --- | --- |
| Typical cells (~370 chars) | **13–23%** |
| Long bodies, 500-char cap | **~57%** |
| Long bodies, 200-char cap | **~80%** |

**Takeaway:** the win scales with body length — low double digits on short cells,
up to ~80% on long ones — deterministic, with no extra model call.

---

## 3. Native-audio conversation modes

X-Ray's voice runs on Gemini Live's **native-audio** model. Two of its
conversation-quality features are now enabled (ADR-039):

- **Affective dialog** — the model detects emotional tone in your voice and adapts
  its responses accordingly.
- **Proactive audio** — the model may stay **silent** on background or
  out-of-context speech instead of replying to everything it hears.

**Takeaway:** conversations feel more natural — though proactive audio also taught
us a lesson, briefly silencing the session greeting because the model judged a
bare "hello" as not worth answering. "Knows when *not* to talk" is a feature with
edges of its own.

---

*Still on the roadmap (ADR-039): graceful session longevity — GoAway handling,
transparent resumption, and context-window compression — so long conversations
survive Gemini Live's session limits without dropping.*
