# ADR-025: Floating Pinnable Item Panel

**Status:** Implemented  
**Todo:** [floating-panel-followup](../todo/floating-panel-followup.md)  

## Context

The original X-Ray UI showed item detail in a fixed right-side panel that slid in when a node was clicked. This had two problems:

1. The panel was anchored to the viewport edge and blocked the graph, pushing the top bar left to compensate.
2. X-Ray had no knowledge of what the user was actively looking at when a voice session started — it could not proactively elaborate on items the user had open.

## Decision

Replace the fixed right panel with a **floating, draggable, pinnable panel** that appears near the clicked graph node.

### Interaction model

- **Click a node** → panel appears near the click position (offset to avoid clipping viewport edges).
- **Pin** (Pin icon in header) → panel stays on screen, header becomes a drag handle; panel can be moved anywhere.
- **Unpin** → panel reverts to follow-click behaviour on next selection.
- **Close** → panel dismisses; if pinned, ESC does not close (prevents accidental dismissal while typing).
- **Single search match** → graph detects `filteredItems.length === 1` and auto-opens the panel at a default position without requiring a click.
- **Single SSE hit** → when a voice tool call returns exactly one matching item, the panel auto-opens for that item.

### Voice context injection

When a voice session starts, pinned item titles are sent as an initial text message over the WebSocket (`{ text: "..." }`) immediately after the connection is established. The backend's existing `Text` handler forwards this to Gemini as a user turn. X-Ray can then reference the pinned items in its opening response.

Context format:
```
The user currently has these items pinned in the X-Ray view:
- <title 1>
- <title 2>
Feel free to elaborate on them if relevant.
```

## Implementation

| File | Change |
|------|--------|
| `ui/components/memory-item-panel.tsx` | Rewritten: `fixed` positioning, drag-on-pin, Pin/Unpin button, `anchorX/Y` props |
| `ui/components/memory-graph.tsx` | `onSelect` now passes `(id, clientX, clientY)`; new `onFilteredCountChange` prop |
| `ui/app/(xray)/page.tsx` | `anchorPos` state, `pinnedItems` array, SSE single-hit auto-open, pinned count badge, voice context string |
| `ui/hooks/use-voice-session.ts` | Accepts `contextText`; sends as WS text message after connect |

## Consequences

- The graph layout no longer shifts when the panel opens — no `panelWidth` offset needed.
- Multiple items can be pinned simultaneously; all titles are included in voice context.
- Pinned state is ephemeral (in-memory); not persisted across page reloads.
- The backend receives no new API changes — context injection reuses the existing `Text` WebSocket message path.
