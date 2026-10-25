# What a voice turn actually costs — X-Ray's new cost model

X-Ray caps per-user daily voice spend. The story is making that cap **fair *and*
verifiable** — and the wrong turn on the way.

## Tokens aren't fungible

Vertex bills Gemini Live as **five SKUs**, not "tokens":

| SKU | $ / 1M | vs text-in |
| --- | --- | --- |
| Text Input | ~$0.50 | 1× |
| Audio Input | ~$3 | **6×** |
| Audio Output | ~$12 | **25×** |
| Text Output | ~$0.50 | 1× |
| Input Caching | ~$0.04 | — |

Same token count, up to **25× the cost**. So a raw-token cap bounds volume, not money.

## Step 1 — see where it goes (ADR-040)

Gemini already returns a per-modality split each turn; we surfaced it in traces,
metrics, and the live UI. A real spoken turn, and the bill:

| Turn | text | audio |
| --- | --- | --- |
| in | 1047 | 723 |
| out | 9 | 0 |

| Spend (sample bill) | $ | share |
| --- | --- | --- |
| Audio (in + out) | $0.46 | **82%** |
| All text | $0.10 | 18% |

Audio is the cost. Everything text is rounding noise.

## Step 2 — the clever wrong turn (ADR-041)

Meter a **cost-weighted** sum (`audioOut·25 + audioIn·6 + text·1`) and cap that.
Fairer than raw tokens — so we shipped it, then couldn't use it:

| Cap unit | Real? | In billing? | In UI? | Verifiable? |
| --- | --- | --- | --- | --- |
| Weighted tokens | ✗ synthetic | ✗ | ✗ | ✗ |

A number you can't reconcile against the bill or show a user isn't a budget — it's
a black box.

## Step 3 — cap what's real (ADR-042)

Audio **is** the cost, so cap audio directly: `metered = audioIn + audioOut`.

| Cap unit | Real? | In billing? | In UI? | Covers cost |
| --- | --- | --- | --- | --- |
| Audio tokens | ✓ | ✓ Live Audio SKUs | ✓ `audio quota` row | ~82% |

Same quantity in three places — the cap, the on-screen `audio quota` row, and the
Vertex bill. We don't re-weight audio in:out (out-volume is tiny, so 1:1 stays
honest). The weighted machinery was removed.

## Takeaway

A *fair* cap and a *verifiable* cap aren't the same thing. We built the clever one
first; the honest one — count the real, billable, on-screen number — was both
simpler and better.

*Roadmap: per-customer attribution needs the BigQuery billing export or per-tenant
projects — the console can't split spend by user.*
