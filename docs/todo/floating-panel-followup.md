# TODO: Floating Panel Follow-up

ADR: `docs/adr/025-floating-pinnable-item-panel.md`

## Overview

Potential improvements after the initial floating panel ships and is tested in production.

## Items

1. **Persist pinned state across page reloads** — store `pinnedIds` in `localStorage`; restore on mount by fetching titles from `/v1/memory/items?id=...`. Low complexity, good UX win.

2. **Touch / mobile drag support** — current drag implementation uses mouse events only. Add `touchstart`/`touchmove`/`touchend` handlers using the same offset logic for tablet users.

3. **Multiple panels** — advanced: allow several items to be open simultaneously (each pinned independently). Requires refactoring `selectedId: string | null` to `openPanels: Map<string, PanelState>`. Defer until single-panel UX is validated.

4. **Voice context depth** — currently sends item titles only. Could enrich with a brief content excerpt (first 100 chars) so X-Ray has richer context without a tool call. Backend change not required; just expand the context string built in `page.tsx`.

5. **Panel snap zones** — when dragging near viewport edges, snap to a corner or edge. Improves precision on small screens.

6. **Highlight pinned nodes** — add a persistent visual indicator (ring or glow) on graph nodes whose items are pinned. Requires passing `pinnedIds` set down to `MemoryGraph` and applying a CSS class or D3 attribute.

## Notes

- Pinned state drives voice context — any UX change to how pinning works should keep the `pinnedItems[]` → `voiceContext` string pipeline intact.
- The `onFilteredCountChange` callback in `MemoryGraph` fires on every render when `filteredItems` changes. If performance issues arise, debounce it or gate behind a minimum query length.
