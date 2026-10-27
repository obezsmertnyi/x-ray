package memstore

import (
	"time"

	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// Item is one memory note. It embeds the MemoryCell spec, so a stored item is
// the cell's own data rather than a copy converted at a boundary; the fields
// below are in-process only and never reach a cell.
type Item struct {
	memoryv1.MemoryCellSpec

	// NoEmbed keeps the item's cell out of the vector index: the writer labels
	// the cell so it is never embedded. For records read back by id, never by
	// meaning.
	NoEmbed bool

	// CellOriginated marks an item loaded from an existing cell (a restart or
	// rebuild seed) rather than authored in memory. It lets a writer tell "the
	// cell existed at load time and was deleted since, on purpose" from "a
	// brand-new item that never had a cell", and refuse to resurrect the
	// former. It is a sibling of the spec, so converting the item back to a
	// spec cannot leak it into a cell; json:"-" is a second guard.
	CellOriginated bool `json:"-"`

	// LinksSet marks Links as the caller's authoritative set for this write,
	// even when empty, so Upsert does not fall back to keeping the links the
	// stored item already had. Off by default: an ordinary content edit must
	// never wipe a link added earlier through AddLink. A writer that recomputes
	// its whole link set on every refresh sets it, so a link can drop back to
	// zero just like content and tags. Never persisted.
	LinksSet bool `json:"-"`

	// lex is the item's precomputed lexical-search cache (see lexicalCache). It
	// is replaced whole, never mutated in place, so a shallow copy shares it
	// safely, and it never reaches a cell.
	lex lexicalCache
}

// Clone is a copy that shares no slice with i.
func (i Item) Clone() Item {
	out := i
	if len(i.EvidenceRefs) > 0 {
		out.EvidenceRefs = append([]string(nil), i.EvidenceRefs...)
	}
	if len(i.Tags) > 0 {
		out.Tags = append([]string(nil), i.Tags...)
	}
	if len(i.Supersedes) > 0 {
		out.Supersedes = append([]string(nil), i.Supersedes...)
	}
	if len(i.Links) > 0 {
		out.Links = make([]memoryv1.ThoughtLink, len(i.Links))
		for j, l := range i.Links {
			lc := l
			if len(l.EvidenceRefs) > 0 {
				lc.EvidenceRefs = append([]string(nil), l.EvidenceRefs...)
			}
			out.Links[j] = lc
		}
	}
	return out
}

// IsExpired reports whether the item has a StaleAfter deadline that has passed.
func (i Item) IsExpired() bool {
	return i.StaleAfter != nil && !i.StaleAfter.IsZero() && time.Now().After(i.StaleAfter.Time)
}

// IsActive reports whether the item is active and not yet expired.
func (i Item) IsActive() bool {
	return i.Status == "active" && !i.IsExpired()
}
