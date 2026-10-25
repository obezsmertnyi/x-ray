# ADR-033: Mobile UI — Graph Launcher, Item Overlay, Context-Aware Search

**Status:** Implemented  
## Context

The mobile experience (viewport `< 768px`, or a `mobile-*` hostname — see `useMobile`) rendered a `MobileSwipeLayout` of five swipeable content tabs (Blog, Projects, News, Team, Events). It diverged from the desktop X-Ray in ways that hurt it:

- The Voronoi memory graph — the centerpiece of the desktop experience — was **never shown on mobile**. Mobile users landed straight on a list.
- Tapping a card navigated to the `/item` route, which renders **outside** the swipe shell (no bottom tab bar), stranding the user with only an in-page back link — and that link pointed at a dead `/memory` route.
- `MemoryCard` mapped every `system_identity` item to a `project` badge, so the Team tab labeled contributors "project". Card previews and the `/item` page rendered raw markdown (`##`, `**`) because neither stripped nor parsed it (the desktop floating panel did, via `react-markdown`).
- There was no search on mobile (desktop has a graph search bar), no way to discover the long-press-to-talk voice affordance, and horizontal swipes had no axis-lock, so a diagonal scroll flick could flip tabs mid-read.

The goal: bring the graph to mobile as the primary surface, keep users inside the swipe shell, and reach parity on search and content rendering — **without changing the desktop UI**, which shares `MemoryCard` and `MemoryGraph`.

## Decision

Rework the mobile layout around a **centered graph launcher**, an **in-shell item overlay**, and a **context-aware search bar**. Desktop behavior is preserved by gating new behavior behind opt-in props.

### Bottom navigation

Seven slots with the graph as a distinct centered button:

```
News · Blog · Projects · [AI] · Team · Events · Notes
```

- **AI** (center, index 3) is the graph "Home" view, rendered as a raised shimmer-gradient icon button (no text label) that stands out from the flat section tabs. The app lands on it.
  - **Tap** → show the graph.
  - **Long-press (500ms)** → toggle voice mode (moved here from the old News-tab long-press). Voice state recolors/animates the button (breathing when active, pulse when connecting).
- **Notes** is a new tab listing the user's personal `agent_note` items (via `fetchUserNotes`), previously only visible as graph nodes.

### Graph as launcher

The Home tab renders `MemoryGraph` full-bleed and centered, reusing the desktop graph's kind colors, sectors, synapses and SSE hit animations. On mobile the graph runs in a distinct interaction mode:

- **`showKindLabels={false}`** — suppress the large per-kind watermark text (redundant noise on a small screen); per-kind item counts remain for orientation.
- **`tapShowsTooltip`** — touch devices have no hover, so a node tap shows the peek tooltip (tap the same node again to dismiss) instead of opening a panel/route. The tooltip itself is made tappable in this mode and calls `onSelect` to open the item overlay ("Tap to open →").

### In-shell item overlay

`MobileItemOverlay` is a full-screen detail view layered **above** the swipe shell (no route navigation), so closing returns to the same tab and scroll position. It renders markdown via `react-markdown` + `remark-gfm`, mirroring the desktop floating panel's prose styling. Body scroll is locked while open.

Gestures (converged after iteration):

- **Swipe right** → close (back). Drag follows the finger; past a threshold it dismisses.
- **Up/down** → native content scroll (an earlier item/category pager was removed as over-complex).

### Context-aware search

A search bar lives in the top header on every tab. Its scope follows the active tab:

- On the **AI/graph Home** → searches **all** memories by feeding `MemoryGraph`'s `query` prop (filters/highlights the graph, with the graph's existing server-side fallback).
- On a **content tab** → filters **only that category's** items locally over title/subject/content/tags (e.g. Team by contributor name).

The query clears on tab switch and is applied only to the active tab (`q = idx === active ? query : ""`).

### Shared-component fixes (affect desktop too — these were bugs)

- `MemoryCard` derives an effective kind from the `contributor` tag → distinct `contributor` badge; previews are stripped to plain text (`plainPreview`).
- The `/item` page renders markdown via `react-markdown` and its dead `/memory` back-link fallbacks now point to `/`.

## Implementation

| File | Change |
|------|--------|
| `ui/components/mobile-swipe-layout.tsx` | Reordered 7-tab set with centered AI button (tap=graph, long-press=voice); Notes section; graph Home view; per-tab context-aware search bar; axis-locked swipe; voice hint |
| `ui/components/mobile-item-overlay.tsx` | New: full-screen markdown detail overlay above the shell; swipe-right to close |
| `ui/components/memory-graph.tsx` | New opt-in props `showKindLabels` (default true) and `tapShowsTooltip` (default false); tooltip becomes tappable→`onSelect` in tap mode. Desktop unchanged. |
| `ui/components/memory-card.tsx` | `contributor` badge/label; `plainPreview` markdown strip; optional `onOpen` (mobile opens overlay instead of `<Link>`) |
| `ui/app/(xray)/item/page.tsx` | Render markdown via `react-markdown`/`remark-gfm`; repoint dead `/memory` links to `/` |
| `ui/app/globals.css` | `overlay-in` keyframe; `no-scrollbar` utility |

## Consequences

- Desktop X-Ray is untouched: the new graph behaviors are opt-in props defaulting to the prior values, and `MemoryCard` without `onOpen` still routes via `<Link>`.
- The mobile graph and the desktop graph share one component; future graph changes apply to both.
- The item overlay fetches a single item by id/kind (`useMemoryItem`); list-to-list paging within the overlay is intentionally not supported.
- Per-tab search is client-side over already-fetched items (except the graph's all-memory search, which keeps the server fallback). Large categories filter in-memory.
- The contributor-badge and markdown fixes were latent bugs visible on both surfaces; fixing them in the shared components corrects desktop as well.
