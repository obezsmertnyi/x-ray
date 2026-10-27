package memstore

import (
	"fmt"
	"strings"
)

// Vocabulary is the host's kinds. The zero value restricts nothing: every kind
// may be written, every kind may be named in a filter, and no kind is hidden
// from an unfiltered search.
type Vocabulary struct {
	// Rejected lists kinds a model-authored write may not use, such as kinds with
	// their own writers.
	Rejected []string
	// Writable lists the kinds an outside writer (a model, a voice session, an
	// API caller) may write through ValidateWrite. Empty means any. Kinds the host
	// writes itself stay out of it.
	Writable []string
	// Searchable lists the kinds a kind filter may name. Empty means any.
	Searchable []string
	// NonSearchable lists kinds left out of an unfiltered search but valid as an
	// explicit filter or a fetch by id (prompt and identity documents, say).
	NonSearchable []string
	// Coerce rewrites an item's kind before it is validated, for a host that
	// reserves a kind for its own writers and turns a model's claim to it into
	// another kind. It runs with no lock held.
	Coerce func(item *Item)
	// FactConfidenceOptional turns off the store's own rule that a stable fact
	// carries high confidence, for a host that enforces it where outside writers
	// arrive (ValidateWrite) and lets its own code write facts as it likes.
	FactConfidenceOptional bool
	// DataKinds reports kinds the host keeps out of the working set and reads on
	// demand through SearchConfig.Hydrate.
	DataKinds func(kind string) bool
}

func inKinds(list []string, kind string) bool {
	for _, k := range list {
		if k == kind {
			return true
		}
	}
	return false
}

// IsRejected reports whether a model-authored write of kind is refused.
func (v Vocabulary) IsRejected(kind string) bool { return inKinds(v.Rejected, kind) }

// IsSearchable reports whether kind may be named in a kind filter. The empty
// string (no filter) is deliberately not searchable, so an unset kind cannot
// pass a check written the wrong way round; a vocabulary with no list accepts
// any non-empty kind.
func (v Vocabulary) IsSearchable(kind string) bool {
	if kind == "" {
		return false
	}
	return len(v.Searchable) == 0 || inKinds(v.Searchable, kind)
}

// IsNonSearchable reports whether kind is left out of an unfiltered search.
func (v Vocabulary) IsNonSearchable(kind string) bool { return inKinds(v.NonSearchable, kind) }

// IsData reports whether the host keeps kind out of the working set.
func (v Vocabulary) IsData(kind string) bool { return v.DataKinds != nil && v.DataKinds(kind) }

// ValidateWrite checks what an outside writer may put in a cell: a kind from the
// writable list, and high confidence on a stable fact (a fact that is stable is
// one the writer is sure of). "" means valid.
func (v Vocabulary) ValidateWrite(kind, confidence string) string {
	if kind == "" {
		return "kind is required"
	}
	if len(v.Writable) > 0 && !inKinds(v.Writable, kind) {
		return fmt.Sprintf("kind %q is not one of: %s", kind, strings.Join(v.Writable, ", "))
	}
	if kind == KindStableFact && confidence != "high" {
		return "stable_fact items must have confidence=high"
	}
	return ""
}

// ValidateSearchKind rejects a kind filter that names no searchable kind, which
// would otherwise return zero results indistinguishable from "nothing matches".
// An empty filter means every kind and is valid.
func (v Vocabulary) ValidateSearchKind(kind string) string {
	if kind == "" || len(v.Searchable) == 0 || inKinds(v.Searchable, kind) {
		return ""
	}
	return fmt.Sprintf("kind %q is not a searchable kind; use one of: %s", kind, strings.Join(v.Searchable, ", "))
}

// BaseVocabulary is the kinds X-Ray's own server uses: what an outside writer
// may write, what a filter may name, and the kinds the server writes itself
// (episodic, mental_map), which are searchable but not writable. A cell of kind
// system_configuration is never indexed, so it is left out of the searchable
// list on purpose: a filter on it could only return nothing.
func BaseVocabulary() Vocabulary {
	writable := []string{
		"stable_fact", "recent_change", "system_identity", "event", "note",
		"active_concern", "operational_pattern", "watch_item", "hypothesis", "unknown",
	}
	return Vocabulary{
		Writable:   writable,
		Searchable: append(append([]string(nil), writable...), "episodic", "mental_map"),
	}
}
