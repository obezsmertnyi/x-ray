package xray

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
	"github.com/triageagent-dev/x-ray/v2/pkg/memstore"
)

// MemoryItem is the shape of a memory cell on the server's wire: the REST API,
// the MCP tools and the live stream. The engine's Item is the cell's own spec;
// this is what a client reads, with the owner of a private item in Subject and
// the status spelled active, stale or superseded.
type MemoryItem struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Scope      string   `json:"scope"`
	Subject    string   `json:"subject,omitempty"`
	Title      string   `json:"title"`
	Content    string   `json:"content"`
	Confidence string   `json:"confidence,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Status     string   `json:"status"`
	// EventDate is only set for kind=event items. RFC3339.
	EventDate string `json:"eventDate,omitempty"`
	// Data holds arbitrary named blobs, e.g. Data["image"] for thumbnails.
	Data map[string]mcell.DataEntry `json:"data,omitempty"`
	// ImageURL is set server-side to /v1/memory/{id}/image when Data["image"] is present.
	ImageURL string `json:"imageUrl,omitempty"`
	// StaleAfter is the item's expiry deadline (decay by default). nil means no
	// deadline: the item never expires on its own.
	StaleAfter *time.Time `json:"staleAfter,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	// Links are the item's ThoughtLinks.
	Links []mcell.ThoughtLink `json:"links,omitempty"`
	// Origin is the cell's provenance: observed, derived, human or verified. A
	// cell stored without one reads as derived.
	Origin string `json:"origin,omitempty"`
	// EvidenceRefs names the cells or events this item was drawn from. Refute
	// follows them.
	EvidenceRefs []string `json:"evidenceRefs,omitempty"`
	// Supersedes lists the ids of cells this one absorbed.
	Supersedes []string `json:"supersedes,omitempty"`
	// Via is set only on a search result that was reached through a link: the id
	// of the higher-ranked result whose link surfaced it.
	Via string `json:"via,omitempty"`
	// Collapsed is set only on a search result that stands for near-duplicates
	// folded into it.
	Collapsed int `json:"collapsed,omitempty"`
}

// MemoryItemSummary is the light row the item lists return.
type MemoryItemSummary struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Scope     string    `json:"scope"`
	Subject   string    `json:"subject,omitempty"`
	Title     string    `json:"title"`
	Tags      []string  `json:"tags,omitempty"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updatedAt"`
	HitCount  int64     `json:"hitCount"`
}

// viewOf converts an engine item to its wire shape. The owner of a private item
// is reported as its Subject, which is where clients have always read it.
func viewOf(it memstore.Item) *MemoryItem {
	v := &MemoryItem{
		ID:           it.ID,
		Kind:         it.Kind,
		Scope:        it.Scope,
		Subject:      it.Subject,
		Title:        it.Title,
		Content:      it.Content,
		Confidence:   it.Confidence,
		Tags:         append([]string(nil), it.Tags...),
		Status:       viewStatus(it.Status),
		EventDate:    it.EventDate,
		StaleAfter:   metaTimeToTimePtr(it.StaleAfter),
		CreatedAt:    it.CreatedAt.Time,
		UpdatedAt:    it.UpdatedAt.Time,
		Origin:       it.Origin,
		EvidenceRefs: append([]string(nil), it.EvidenceRefs...),
		Supersedes:   append([]string(nil), it.Supersedes...),
	}
	if len(it.Tags) == 0 {
		v.Tags = nil
	}
	if len(it.EvidenceRefs) == 0 {
		v.EvidenceRefs = nil
	}
	if len(it.Supersedes) == 0 {
		v.Supersedes = nil
	}
	if len(it.Data) > 0 {
		v.Data = make(map[string]mcell.DataEntry, len(it.Data))
		for k, e := range it.Data {
			v.Data[k] = e
		}
	}
	if len(it.Links) > 0 {
		v.Links = append([]mcell.ThoughtLink(nil), it.Links...)
	}
	if memstore.IsPrivateItem(it) {
		v.Subject = memstore.PrivateOwner(it)
	}
	return v
}

// viewStatus is the status a client reads: a cell that is not retired is active.
func viewStatus(status string) string {
	if status == "stale" || status == "superseded" {
		return status
	}
	return "active"
}

func views(items []memstore.Item) []*MemoryItem {
	out := make([]*MemoryItem, len(items))
	for i, it := range items {
		out[i] = viewOf(it)
	}
	return out
}

// specOf converts a wire item to a cell spec: a private item's owner moves from
// Subject to the user: tag, and Subject becomes the identity of this instance.
func specOf(v *MemoryItem, instance string) mcell.MemoryCellSpec {
	spec := mcell.MemoryCellSpec{
		ID:           v.ID,
		Kind:         v.Kind,
		Scope:        v.Scope,
		Subject:      v.Subject,
		Title:        v.Title,
		Content:      v.Content,
		Confidence:   v.Confidence,
		EventDate:    v.EventDate,
		Tags:         v.Tags,
		Data:         v.Data,
		Status:       cellStatus(v.Status),
		StaleAfter:   timePtrToMetaTime(v.StaleAfter),
		Origin:       v.Origin,
		EvidenceRefs: v.EvidenceRefs,
		Supersedes:   v.Supersedes,
		Links:        v.Links,
	}
	if !v.CreatedAt.IsZero() {
		spec.CreatedAt = metav1.NewTime(v.CreatedAt)
	}
	if !v.UpdatedAt.IsZero() {
		spec.UpdatedAt = metav1.NewTime(v.UpdatedAt)
	}
	if v.Scope == memstore.ScopePrivate && v.Subject != "" {
		owner := v.Subject
		spec.Subject = instance
		spec.Tags = append([]string(nil), v.Tags...)
		if !hasTag(spec.Tags, memstore.PrivateOwnerTagPrefix+owner) {
			spec.Tags = append(spec.Tags, memstore.PrivateOwnerTagPrefix+owner)
		}
	}
	return spec
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// itemOfView converts a wire item to an engine item.
func itemOfView(v *MemoryItem, instance string) memstore.Item {
	return memstore.Item{MemoryCellSpec: specOf(v, instance)}
}

// itemOfCell converts a stored cell to an engine item. The embedding stays with
// the cell: the vector lives in the cell index, not in the item. The item is
// marked as loaded from a cell.
func itemOfCell(cell *mcell.MemoryCell) memstore.Item {
	spec := cell.Spec
	spec.Embedding = ""
	if spec.Status == "" || spec.Status == "indexed" {
		spec.Status = "active"
	}
	return memstore.Item{MemoryCellSpec: spec, CellOriginated: true}
}

func metaTimeToTimePtr(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.Time
	return &v
}

func timePtrToMetaTime(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	v := metav1.NewTime(*t)
	return &v
}

// cellStatus is the status stored on a cell: retired cells are kept, everything
// else is stored active.
func cellStatus(status string) string {
	if status == "stale" || status == "superseded" {
		return status
	}
	return "active"
}
