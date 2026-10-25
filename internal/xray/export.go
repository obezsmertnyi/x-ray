package xray

import (
	"context"
	"time"
)

// This file is the surface the public library (pkg/memory) builds on. It keeps
// the rules the MCP server applies at its boundary (what may be written, who a
// write is attributed to) reachable by a program that embeds the store.

// ValidateWrite checks what an external writer may put in an item: a kind from
// the vocabulary, and high confidence on a stable_fact. "" means valid.
func ValidateWrite(kind, confidence string) string { return validateWrite(kind, confidence) }

// ValidateSearchKind rejects a kind filter that names no searchable kind. An
// empty filter means every kind and is valid.
func ValidateSearchKind(kind string) string { return validateSearchKind(kind) }

// WithOrigin returns ctx carrying the provenance of the caller. Upsert reads it:
// a write made as OriginHuman is never replaced by a derived one.
func WithOrigin(ctx context.Context, origin string) context.Context {
	return withWriteOrigin(ctx, origin)
}

// ExportAll returns a copy of every item the in-process store holds, private
// and retired ones included, for a program that persists the store itself.
func (s *MemoryStore) ExportAll() []*MemoryItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*MemoryItem, 0, len(s.items))
	for _, it := range s.items {
		cp := *it
		out = append(out, &cp)
	}
	return out
}

// Restore puts an item back as it was exported: ids, origin, status, links,
// timestamps and deadline are kept as given. An active shared item is indexed
// again (keyword and, with an embedder, semantic); a retired or private one is
// held and left out of the shared indexes, exactly as Upsert would leave it.
func (s *MemoryStore) Restore(ctx context.Context, it *MemoryItem) {
	cp := *it
	if cp.Status == "" {
		cp.Status = "active"
	}
	s.mu.Lock()
	s.items[cp.ID] = &cp
	s.mu.Unlock()
	if !isActive(cp.Status) || isPrivateItem(cp.Kind, cp.Scope) {
		return
	}
	if s.cells != nil {
		embedCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if _, err := s.cells.Index(embedCtx, itemToDoc(&cp)); err != nil {
			s.logger.Warn().Err(err).Str("id", cp.ID).Msg("restore: embed failed; the item is kept for keyword search")
		}
	}
	s.kwIdx.index(cp.ID, cp.Title, cp.Content)
	s.markSeen(&cp)
}

// ValidEventDate reports whether s is an event date the store can render back:
// RFC3339 or a bare YYYY-MM-DD date.
func ValidEventDate(s string) bool {
	_, ok := parseEventDate(s)
	return ok
}
