package xray

import (
	"context"
	"time"
)

// Cells reach a pod two ways: written by it (Upsert, Delete, retire), or written
// elsewhere (another replica, kubectl, GitOps) and seen through the cluster
// watch. A pod that only tracked its own writes would never index a cell another
// replica created, keep serving one another replica deleted, or tell its live
// clients about either. markSeen, applyRemote and forget close that: every pod
// converges on the cluster's cells and publishes the same change events.

// markSeen records it as the last shared state this pod published, so the watch
// event that follows its own write is recognised and not published again.
func (s *MemoryStore) markSeen(it *MemoryItem) {
	cp := *it
	s.mu.Lock()
	s.seen[it.ID] = &cp
	s.mu.Unlock()
}

// sameShared reports whether a and b are the same shared state, ignoring what
// differs between an in-process item and the cell stored for it: the timestamps
// and the status spelling (indexed in this pod, active in the cell).
func sameShared(a, b *MemoryItem) bool {
	x, y := *a, *b
	x.Status, y.Status = cellStatus(x.Status), cellStatus(y.Status)
	// A stored deadline has second precision; the in-process one has more.
	x.StaleAfter, y.StaleAfter = truncSecond(x.StaleAfter), truncSecond(y.StaleAfter)
	return itemsSemanticallyEqual(&x, &y)
}

func truncSecond(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.Truncate(time.Second)
	return &v
}

// applyRemote folds a shared cell seen through the watch into this pod: the
// keyword index follows it, and when its state is new to this pod a change event
// goes to live clients. A cell that is no longer active is forgotten instead.
// Private and system cells are not shared state and are left to their own paths.
func (s *MemoryStore) applyRemote(it *MemoryItem) {
	if isPrivateItem(it.Kind, it.Scope) {
		return
	}
	if !isActive(it.Status) {
		s.forget(it.ID, false)
		return
	}
	s.mu.Lock()
	prev := s.seen[it.ID]
	cp := *it
	s.seen[it.ID] = &cp
	s.mu.Unlock()
	if prev != nil && sameShared(prev, it) {
		return
	}
	s.kwIdx.index(it.ID, it.Title, it.Content)
	s.hub.PublishChange(MemoryChangeEvent{Op: "upsert", Item: it})
}

// forget drops a cell that is gone or retired elsewhere from this pod's keyword
// and vector indexes and, when this pod had published it, tells live clients it
// is deleted. removeItem also drops it from the in-process map, for a cell whose
// resource was deleted (a retired cell is kept there with its status).
func (s *MemoryStore) forget(id string, removeItem bool) {
	s.mu.Lock()
	prev := s.seen[id]
	delete(s.seen, id)
	if removeItem {
		delete(s.items, id)
	}
	s.mu.Unlock()
	s.kwIdx.remove(id)
	if s.cells != nil {
		_ = s.cells.Delete(context.Background(), id)
	}
	if prev != nil {
		s.hub.PublishChange(MemoryChangeEvent{Op: "delete", ID: id})
	}
}
