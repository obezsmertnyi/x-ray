package xray

import "github.com/triageagent-dev/x-ray/v2/pkg/memstore"

// annotateEventTimes returns items with the event-time suffix appended to the
// Title of every item that has a (parseable) EventDate. The MCP search_memory
// result is the full JSON item, so the annotation is composed here rather than
// into a narrower per-tool row type. It goes on the Title, never the Content,
// because the Title is never truncated downstream. Items without a usable
// EventDate are passed through by reference; items that get an annotation are
// copied first.
func annotateEventTimes(items []*MemoryItem) []*MemoryItem {
	out := make([]*MemoryItem, len(items))
	for i, it := range items {
		ann := memstore.EventTimeAnnotation(it.EventDate)
		if ann == "" {
			out[i] = it
			continue
		}
		clone := *it
		clone.Title += ann
		out[i] = &clone
	}
	return out
}
