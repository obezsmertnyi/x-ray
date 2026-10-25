package xray

import "time"

// eventDateLayouts are the input formats a MemoryItem.EventDate string is
// accepted in, matched in order: RFC3339 first (the format the field's own
// doc comment specifies), then a bare calendar date. upsert_memory's
// event_date argument validates against this list on write, and
// eventTimeAnnotation parses against it on read, so a value that's accepted
// on write is guaranteed to render on read.
var eventDateLayouts = []string{time.RFC3339, "2006-01-02"}

// parseEventDate parses s against eventDateLayouts, reporting whether it
// parsed. Empty input never parses — callers check for "" separately, since
// "no event_date" (silently omit) and "bad event_date" (reject the write /
// skip the annotation) need different handling.
func parseEventDate(s string) (time.Time, bool) {
	for _, layout := range eventDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// eventTimeAnnotation renders a compact reader-facing suffix for a memory
// item's EventDate, mirroring Zep's LoCoMo template (`fact (event_time:
// ...)`) — see docs/todo/graphiti-comparison-refinement.md item #1. A caller
// consuming search results as an LLM's context needs to see WHEN a
// remembered item happened without parsing a separate structured field out
// of the response envelope. Returns "" when eventDate is empty or fails to
// parse under either accepted layout, so a bad/legacy value degrades to "no
// annotation" rather than rendering garbage.
//
// Renders the date part only ("2023-05-30") when the parsed time is exactly
// UTC midnight — the shape a bare YYYY-MM-DD write produces — and the full
// RFC3339 timestamp otherwise.
func eventTimeAnnotation(eventDate string) string {
	if eventDate == "" {
		return ""
	}
	t, ok := parseEventDate(eventDate)
	if !ok {
		return ""
	}
	t = t.UTC()
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
		return " (event_time: " + t.Format("2006-01-02") + ")"
	}
	return " (event_time: " + t.Format(time.RFC3339) + ")"
}

// annotateEventTimes returns items with eventTimeAnnotation appended to the
// Title of every item that has a (parseable) EventDate — used by the MCP
// search_memory tool, whose result is the full JSON-serialized MemoryItem
// (unlike voice's headroom-compressed row projection, so the annotation has
// to be composed here rather than into a narrower per-tool row type).
// Appends to Title rather than Content because Title is never lossy-
// truncated downstream, so the annotation can never be cut off. Items
// without a usable EventDate are passed through by reference (no clone);
// items that get an annotation are shallow-copied first — GetItem/Search can
// return a pointer aliasing live store state (the ephemeral-map path), so
// mutating Title in place would corrupt the stored item.
func annotateEventTimes(items []*MemoryItem) []*MemoryItem {
	out := make([]*MemoryItem, len(items))
	for i, it := range items {
		ann := eventTimeAnnotation(it.EventDate)
		if ann == "" {
			out[i] = it
			continue
		}
		clone := *it
		clone.Title = clone.Title + ann
		out[i] = &clone
	}
	return out
}
