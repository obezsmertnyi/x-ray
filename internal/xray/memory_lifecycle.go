package xray

import (
	"context"
	"sort"
	"strings"
)

// Expire retires a cell without a verdict on it: it is out of date, not wrong
// (Refute is for wrong). The cell is kept with status stale and leaves every
// index. The returned item is already stored in the process; a caller backed by
// the cluster persists it. nil when id names no cell.
func (s *MemoryStore) Expire(id string) *MemoryItem {
	it := s.GetItem(strings.TrimSpace(id))
	if it == nil {
		return nil
	}
	clone := *it
	return s.retire(&clone, "stale")
}

// Supersede replaces the cell oldID with a new one: the new cell is written
// (as Upsert writes it, so origin, dedup-free id handling and size caps apply)
// and lists oldID in Supersedes, and the old cell is retired with status
// superseded. A derived write cannot supersede a human cell. The returned items
// are the new cell then the old one, both already stored in the process.
func (s *MemoryStore) Supersede(ctx context.Context, oldID, newID, kind, scope, title, content, subject, confidence string, tags []string, opts ...UpsertOption) (changed []*MemoryItem, errMsg string) {
	old := s.GetItem(strings.TrimSpace(oldID))
	if old == nil {
		return nil, "not_found"
	}
	if newID == old.ID {
		return nil, "the replacement must have a different id from the cell it supersedes"
	}
	origin := writeOriginFrom(ctx)
	if origin == "" {
		origin = OriginDerived
	}
	if effectiveOrigin(old.Origin) == OriginHuman && origin == OriginDerived {
		return nil, errDerivedReplacesHuman
	}
	// Dedup is off for this write: the caller named the cell being replaced, so a
	// near-duplicate of that same cell must not swallow its replacement.
	res, errMsg := s.upsertNoDedup(ctx, newID, kind, scope, title, content, subject, confidence, tags, append(opts, withSupersedes([]string{old.ID}))...)
	if errMsg != "" {
		return nil, errMsg
	}
	clone := *old
	return []*MemoryItem{res.Item, s.retire(&clone, "superseded")}, ""
}

// upsertNoDedup is Upsert with write-side dedup suspended for this one call.
func (s *MemoryStore) upsertNoDedup(ctx context.Context, id, kind, scope, title, content, subject, confidence string, tags []string, opts ...UpsertOption) (UpsertResult, string) {
	return s.upsertWithStaleAfter(ctx, id, kind, scope, title, content, subject, confidence, "", nil, tags, nil, append(opts, func(o *upsertOpts) { o.noDedup = true })...)
}

// ScoredItem is a cell reached by Context and how strongly.
type ScoredItem struct {
	Item  *MemoryItem
	Score float32
}

// resolveAnchors maps an anchor string to cell ids: an exact id first, then every
// cell whose subject is the anchor, then every cell whose subject or title
// contains it. Private cells never resolve.
func resolveAnchors(anchor string, items []*MemoryItem) []string {
	anchor = strings.ToLower(strings.TrimSpace(anchor))
	if anchor == "" {
		return nil
	}
	for _, it := range items {
		if strings.ToLower(it.ID) == anchor {
			return []string{it.ID}
		}
	}
	var ids []string
	for _, it := range items {
		if strings.ToLower(it.Subject) == anchor {
			ids = append(ids, it.ID)
		}
	}
	if len(ids) > 0 {
		return ids
	}
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.Subject), anchor) || strings.Contains(strings.ToLower(it.Title), anchor) {
			ids = append(ids, it.ID)
		}
	}
	return ids
}

// Context returns the cells an anchor activates: the anchor cells themselves and
// everything reachable over ThoughtLinks, by spreading activation. Each hop
// multiplies the score by decay and the link's weight, a cell keeps the best
// score any path gave it, and at most maxDepth hops are followed (default 3, at
// most 8). Only shared, active cells take part: the traversal carries no caller
// identity to test a private cell's owner against.
func (s *MemoryStore) Context(anchor string, maxDepth int) (anchors []*MemoryItem, activated []ScoredItem) {
	const decay = 0.6
	if maxDepth <= 0 {
		maxDepth = 3
	}
	if maxDepth > 8 {
		maxDepth = 8
	}
	var pool []*MemoryItem
	byID := map[string]*MemoryItem{}
	for _, it := range s.FilterItems("", "") {
		if isPrivateItem(it.Kind, it.Scope) {
			continue
		}
		pool = append(pool, it)
		byID[it.ID] = it
	}
	anchorIDs := resolveAnchors(anchor, pool)
	if len(anchorIDs) == 0 {
		return nil, nil
	}

	type act struct {
		score float32
		depth int
	}
	reached := make(map[string]act, len(anchorIDs))
	isAnchor := make(map[string]bool, len(anchorIDs))
	queue := make([]string, 0, len(anchorIDs))
	for _, id := range anchorIDs {
		reached[id] = act{score: 1, depth: 0}
		isAnchor[id] = true
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		from := queue[0]
		queue = queue[1:]
		cur := reached[from]
		if cur.depth >= maxDepth {
			continue
		}
		src, ok := byID[from]
		if !ok {
			continue
		}
		for _, l := range src.Links {
			if l.ToID == "" {
				continue
			}
			score := cur.score * decay * l.Weight
			if prev, seen := reached[l.ToID]; seen && score <= prev.score {
				continue
			}
			reached[l.ToID] = act{score: score, depth: cur.depth + 1}
			queue = append(queue, l.ToID)
		}
	}
	for _, id := range anchorIDs {
		anchors = append(anchors, byID[id])
	}
	for id, a := range reached {
		if isAnchor[id] {
			continue
		}
		if it, ok := byID[id]; ok {
			activated = append(activated, ScoredItem{Item: it, Score: a.score})
		}
	}
	sort.Slice(activated, func(i, j int) bool {
		if activated[i].Score != activated[j].Score {
			return activated[i].Score > activated[j].Score
		}
		return activated[i].Item.ID < activated[j].Item.ID
	})
	return anchors, activated
}
