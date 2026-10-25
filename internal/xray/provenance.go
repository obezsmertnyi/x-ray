package xray

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"time"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// Cell provenance (ADR-053), stored in MemoryCellSpec.Origin.
const (
	// OriginObserved: written by code from facts.
	OriginObserved = "observed"
	// OriginDerived: written by a model. A prior claim, never evidence. A cell
	// with no origin is derived.
	OriginDerived = "derived"
	// OriginHuman: written by an identified person (an OAuth-authenticated MCP
	// call, a voice session). Outranks derived.
	OriginHuman = "human"
	// OriginVerified: derived, then passed a check. Set only in code.
	OriginVerified = "verified"
)

// errDerivedReplacesHuman is the message for a derived write onto a human cell.
const errDerivedReplacesHuman = "a derived write cannot replace a human-authored cell"

// refutedDataKey is the Data entry Refute writes: reason, evidence and time
// (JSON, application/json, base64 like every other Data blob).
const refutedDataKey = "refuted"

// resolveOrigin picks the origin a write lands with. existing is the origin of
// the cell it replaces (hasExisting false when there is none). An empty
// incoming origin keeps the existing one, or becomes derived when there is
// none. A derived write onto a human cell is refused (msg non-empty).
func resolveOrigin(existing string, hasExisting bool, incoming string) (origin, msg string) {
	origin = incoming
	if origin == "" {
		if hasExisting && existing != "" {
			origin = existing
		} else {
			origin = OriginDerived
		}
	}
	if hasExisting && existing == OriginHuman && origin == OriginDerived {
		return "", errDerivedReplacesHuman
	}
	return origin, ""
}

// effectiveOrigin is the origin of a cell as read back: a cell stored without
// one is derived.
func effectiveOrigin(o string) string {
	if o == "" {
		return OriginDerived
	}
	return o
}

type writeOriginKey struct{}

// withWriteOrigin stamps the provenance of the caller onto ctx; Upsert reads it.
func withWriteOrigin(ctx context.Context, origin string) context.Context {
	return context.WithValue(ctx, writeOriginKey{}, origin)
}

func writeOriginFrom(ctx context.Context) string {
	o, _ := ctx.Value(writeOriginKey{}).(string)
	return o
}

// refutedRecord is the JSON held under refutedDataKey.
type refutedRecord struct {
	Reason   string   `json:"reason"`
	Evidence []string `json:"evidence"`
	At       string   `json:"at"`
}

// citesItem reports whether c names id in its evidence refs (with or without a
// cell: prefix) or links to it.
func citesItem(c *MemoryItem, id string) bool {
	for _, r := range c.EvidenceRefs {
		if r == id || strings.TrimPrefix(r, "cell:") == id {
			return true
		}
	}
	for _, l := range c.Links {
		if l.ToID == id {
			return true
		}
	}
	return false
}

// Refute marks a cell wrong (ADR-053): it records {reason, evidence, at} under
// Data["refuted"], retires the cell (status stale) and retires every ACTIVE
// derived cell that cites it, one hop only: an evidence ref naming the id or a
// link to it. Observed, human and verified cells are never cascaded. The
// returned items are the changed cells, the refuted one first, already stored
// in the process; a caller backed by the cluster persists each. found is false
// when id names no cell.
func (s *MemoryStore) Refute(ctx context.Context, id, reason string, evidence []string) (changed []*MemoryItem, found bool) {
	it := s.GetItem(strings.TrimSpace(id))
	if it == nil {
		return nil, false
	}
	if evidence == nil {
		evidence = []string{}
	}
	now := time.Now()
	clone := *it
	if raw, err := json.Marshal(refutedRecord{Reason: reason, Evidence: evidence, At: now.UTC().Format(time.RFC3339)}); err == nil {
		data := make(map[string]mcell.DataEntry, len(clone.Data)+1)
		for k, v := range clone.Data {
			data[k] = v
		}
		data[refutedDataKey] = mcell.DataEntry{Bytes: base64.StdEncoding.EncodeToString(raw), Mime: "application/json"}
		clone.Data = data
	}
	changed = append(changed, s.retire(&clone, "stale"))

	var hits []*MemoryItem
	for _, c := range s.FilterItems("", "") {
		if c.ID == clone.ID || effectiveOrigin(c.Origin) != OriginDerived || !citesItem(c, clone.ID) {
			continue
		}
		hits = append(hits, c)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].ID < hits[j].ID })
	for _, c := range hits {
		cc := *c
		changed = append(changed, s.retire(&cc, "stale"))
	}
	return changed, true
}

// retire stores it with status ("stale" or "superseded") and takes it out of
// the lexical and semantic indexes, so no search reaches it. The cell itself is
// kept; a caller backed by the cluster persists the returned item.
func (s *MemoryStore) retire(it *MemoryItem, status string) *MemoryItem {
	it.Status = status
	it.UpdatedAt = time.Now()
	s.mu.Lock()
	s.items[it.ID] = it
	delete(s.seen, it.ID)
	s.mu.Unlock()
	s.kwIdx.remove(it.ID)
	if s.cells != nil {
		_ = s.cells.Delete(context.Background(), it.ID)
	}
	switch {
	case isPrivateScope(it.Scope) && it.Subject != "":
		s.hub.PublishChangeTo(it.Subject, MemoryChangeEvent{Op: "delete", ID: it.ID})
	case !isPrivateItem(it.Kind, it.Scope):
		s.hub.PublishChange(MemoryChangeEvent{Op: "delete", ID: it.ID})
	}
	return it
}
