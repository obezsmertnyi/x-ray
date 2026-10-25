package xray

import (
	"fmt"
	"strings"
)

// writableKinds is the vocabulary an agent or a voice session may write through
// upsert (ADR-020). Server-written kinds (episodic, system_configuration,
// mental_map) are not in it.
var writableKinds = []string{
	"stable_fact", "recent_change", "system_identity", "event", "note",
	"active_concern", "operational_pattern", "watch_item", "hypothesis", "unknown",
}

// searchableKinds is writableKinds plus the server-written kinds that are
// searchable. system_configuration is left out on purpose: it is never indexed,
// so a kind filter on it is guaranteed to return nothing.
var searchableKinds = append(append([]string(nil), writableKinds...), "episodic", "mental_map")

func inList(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}

// validateWrite checks what an external writer may put in a cell: a kind from
// the vocabulary, and high confidence on a stable_fact (a fact that is stable is
// one the writer is sure of). "" means valid.
func validateWrite(kind, confidence string) string {
	if kind == "" {
		return "kind is required"
	}
	if !inList(writableKinds, kind) {
		return fmt.Sprintf("kind %q is not one of: %s", kind, strings.Join(writableKinds, ", "))
	}
	if kind == "stable_fact" && confidence != "high" {
		return "stable_fact items must have confidence=high"
	}
	return ""
}

// validateSearchKind rejects a kind filter that names no searchable kind, which
// would otherwise return zero results indistinguishable from "nothing matches".
// An empty filter means every kind and is valid.
func validateSearchKind(kind string) string {
	if kind == "" || inList(searchableKinds, kind) {
		return ""
	}
	return fmt.Sprintf("kind %q is not a searchable kind; use one of: %s", kind, strings.Join(searchableKinds, ", "))
}
