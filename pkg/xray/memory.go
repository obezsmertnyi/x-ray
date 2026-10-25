package xray

import (
	"context"
	"encoding/base64"
	"errors"
	"time"
)

// Memory kinds an agent may write (Write.Kind). Search and List also accept
// KindEpisodic and KindMentalMap as filters.
const (
	KindStableFact         = "stable_fact"
	KindOperationalPattern = "operational_pattern"
	KindActiveConcern      = "active_concern"
	KindWatchItem          = "watch_item"
	KindHypothesis         = "hypothesis"
	KindRecentChange       = "recent_change"
	KindEvent              = "event"
	KindNote               = "note"
	KindSystemIdentity     = "system_identity"
	KindUnknown            = "unknown"
	KindEpisodic           = "episodic"
	KindMentalMap          = "mental_map"
)

// Scopes. A private item is visible only to callers that pass its subject.
const (
	ScopePublic  = "public"
	ScopePrivate = "private"
)

// Origins, as read back on an Item. An item stored without one reads as derived.
const (
	OriginObserved = "observed"
	OriginDerived  = "derived"
	OriginHuman    = "human"
	OriginVerified = "verified"
)

// Item is one memory item.
type Item struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Scope      string     `json:"scope"`
	Subject    string     `json:"subject,omitempty"`
	Title      string     `json:"title"`
	Content    string     `json:"content"`
	Confidence string     `json:"confidence,omitempty"`
	Tags       []string   `json:"tags,omitempty"`
	Status     string     `json:"status"`
	EventDate  string     `json:"eventDate,omitempty"`
	StaleAfter *time.Time `json:"staleAfter,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	Links      []Link     `json:"links,omitempty"`
	// Origin is who wrote the item: observed, derived, human or verified.
	Origin string `json:"origin,omitempty"`
	// EvidenceRefs names the items or events this one was drawn from.
	EvidenceRefs []string `json:"evidenceRefs,omitempty"`
	// Supersedes lists the items this one absorbed.
	Supersedes []string `json:"supersedes,omitempty"`
	// Via is set on a search result that was added by following a link from the
	// result of that id.
	Via string `json:"via,omitempty"`
	// Collapsed counts the near-duplicates folded into a search result.
	Collapsed int `json:"collapsed,omitempty"`
}

// Link is a typed, weighted edge to another item.
type Link struct {
	ToID         string   `json:"toId"`
	Relation     string   `json:"relation"`
	Weight       float32  `json:"weight"`
	EvidenceRefs []string `json:"evidenceRefs,omitempty"`
}

// QueryOption narrows a memory call.
type QueryOption func(*queryOptions)

type queryOptions struct {
	subject string
	kind    string
	limit   int
}

// WithSubject scopes a call to one subject. It is required to reach a private
// item, and it is how a caller names itself.
func WithSubject(subject string) QueryOption { return func(o *queryOptions) { o.subject = subject } }

// WithKind keeps only items of one kind.
func WithKind(kind string) QueryOption { return func(o *queryOptions) { o.kind = kind } }

// WithLimit caps the number of results. The server applies its own ceiling.
func WithLimit(n int) QueryOption { return func(o *queryOptions) { o.limit = n } }

func buildQuery(opts []QueryOption) queryOptions {
	var q queryOptions
	for _, o := range opts {
		o(&q)
	}
	return q
}

func (q queryOptions) args(a map[string]any) map[string]any {
	put(a, "subject", q.subject)
	put(a, "kind", q.kind)
	put(a, "limit", q.limit)
	return a
}

type itemsReply struct {
	Items []Item `json:"items"`
}

// Search ranks items by relevance to query, by keyword and by meaning. Without
// WithSubject only shared items are searched.
func (c *Client) Search(ctx context.Context, query string, opts ...QueryOption) ([]Item, error) {
	var out itemsReply
	args := buildQuery(opts).args(map[string]any{"query": query})
	if err := c.call(ctx, "search_memory", args, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// List browses items by kind and subject, unranked.
func (c *Client) List(ctx context.Context, opts ...QueryOption) ([]Item, error) {
	var out itemsReply
	if err := c.call(ctx, "list_memory", buildQuery(opts).args(map[string]any{}), &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Fetch returns the items with the given ids, in that order. A missing id, or a
// private item the subject does not own, is left out.
func (c *Client) Fetch(ctx context.Context, ids []string, opts ...QueryOption) ([]Item, error) {
	var out itemsReply
	args := map[string]any{"ids": ids}
	put(args, "subject", buildQuery(opts).subject)
	if err := c.call(ctx, "fetch_memory", args, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Get returns one item, or an error satisfying IsNotFound.
func (c *Client) Get(ctx context.Context, id string, opts ...QueryOption) (*Item, error) {
	items, err := c.Fetch(ctx, []string{id}, opts...)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, &ToolError{Tool: "fetch_memory", Message: "not found"}
	}
	return &items[0], nil
}

// Summary is the count of items by kind.
type Summary struct {
	Total  int            `json:"total"`
	ByKind map[string]int `json:"by_kind"`
}

// Summary gives a quick overview of what the server holds.
func (c *Client) Summary(ctx context.Context) (*Summary, error) {
	var out Summary
	if err := c.call(ctx, "get_memory", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Episode is a past-session memory, as Recent returns it.
type Episode struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// Recent returns the newest episodic memories of a subject, newest first.
func (c *Client) Recent(ctx context.Context, opts ...QueryOption) ([]Episode, error) {
	var out struct {
		Items []Episode `json:"items"`
	}
	q := buildQuery(opts)
	args := map[string]any{}
	put(args, "subject", q.subject)
	put(args, "limit", q.limit)
	if err := c.call(ctx, "recent_memory", args, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Write is an item to create or update.
type Write struct {
	// ID updates the item with that id; empty creates a new one.
	ID string
	// Kind is required. A stable_fact needs Confidence "high".
	Kind string
	// Scope is required: ScopePublic or ScopePrivate. A private item needs Subject.
	Scope   string
	Title   string
	Content string
	// Subject is the author or owner.
	Subject    string
	Confidence string
	// EventDate is RFC3339 or YYYY-MM-DD, for a kind=event item.
	EventDate string
	Tags      []string
	// EvidenceRefs names the items or events this one was drawn from, so that
	// Refute can retire it with them.
	EvidenceRefs []string
	// Image is an optional thumbnail, ImageMIME its type (default image/jpeg).
	Image     []byte
	ImageMIME string
}

func (w Write) args() map[string]any {
	a := map[string]any{"kind": w.Kind, "scope": w.Scope, "title": w.Title, "content": w.Content}
	put(a, "id", w.ID)
	put(a, "subject", w.Subject)
	put(a, "confidence", w.Confidence)
	put(a, "event_date", w.EventDate)
	put(a, "tags", w.Tags)
	put(a, "evidence_refs", w.EvidenceRefs)
	if len(w.Image) > 0 {
		a["image_data"] = base64.StdEncoding.EncodeToString(w.Image)
		put(a, "image_mime", w.ImageMIME)
	}
	return a
}

// WriteResult is the outcome of Upsert.
type WriteResult struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Status is "created", "updated" or "merged". Merged means the write restated
	// an item that already existed and landed on it: ID is that item's id.
	Status     string `json:"status"`
	Title      string `json:"title"`
	MergedInto string `json:"merged_into,omitempty"`
}

// Upsert creates an item, or updates the one named by w.ID.
func (c *Client) Upsert(ctx context.Context, w Write) (*WriteResult, error) {
	var out WriteResult
	if err := c.call(ctx, "upsert_memory", w.args(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes an item. A private item needs its subject.
func (c *Client) Delete(ctx context.Context, id string, opts ...QueryOption) error {
	args := map[string]any{"id": id}
	put(args, "subject", buildQuery(opts).subject)
	return c.call(ctx, "delete_memory", args, nil)
}

// RefuteResult is the outcome of Refute.
type RefuteResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// Cascaded lists the model-written items retired because they cited the
	// refuted one.
	Cascaded []string `json:"cascaded"`
}

// Refute retires an item that is wrong, records the reason and evidence on it,
// and retires every active derived item that cites it.
func (c *Client) Refute(ctx context.Context, id, reason string, evidence []string, opts ...QueryOption) (*RefuteResult, error) {
	if reason == "" {
		return nil, errors.New("xray: refute needs a reason")
	}
	args := map[string]any{"id": id, "reason": reason}
	put(args, "evidence", evidence)
	put(args, "subject", buildQuery(opts).subject)
	var out RefuteResult
	if err := c.call(ctx, "refute_memory", args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Expire retires an item that is out of date. Use Refute when it is wrong.
func (c *Client) Expire(ctx context.Context, id string, opts ...QueryOption) error {
	args := map[string]any{"id": id}
	put(args, "subject", buildQuery(opts).subject)
	return c.call(ctx, "expire_memory", args, nil)
}

// SupersedeResult is the outcome of Supersede.
type SupersedeResult struct {
	ID         string `json:"id"`
	Superseded string `json:"superseded"`
	Status     string `json:"status"`
}

// Supersede writes w as the replacement of oldID and retires the old item. w.ID
// names the replacement, or is left empty to have one generated.
func (c *Client) Supersede(ctx context.Context, oldID string, w Write) (*SupersedeResult, error) {
	args := w.args()
	args["old_id"] = oldID
	if id, ok := args["id"]; ok {
		args["new_id"] = id
		delete(args, "id")
	}
	var out SupersedeResult
	if err := c.call(ctx, "supersede_memory", args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ConsolidationGroup is a set of near-duplicates: the item to keep and the
// restatements it absorbs.
type ConsolidationGroup struct {
	Kind       string    `json:"kind"`
	Keep       string    `json:"keep"`
	KeepTitle  string    `json:"keep_title"`
	Superseded []string  `json:"superseded"`
	Scores     []float64 `json:"scores"`
}

// ConsolidationReport is the outcome of Consolidate.
type ConsolidationReport struct {
	Scanned    int                  `json:"scanned"`
	Groups     []ConsolidationGroup `json:"groups"`
	Superseded int                  `json:"superseded"`
	Applied    bool                 `json:"applied"`
}

// Consolidate reports near-duplicate facts. With apply it retires the
// restatements. WithSubject restricts the sweep to one subject.
func (c *Client) Consolidate(ctx context.Context, apply bool, opts ...QueryOption) (*ConsolidationReport, error) {
	args := map[string]any{"apply": apply}
	put(args, "subject", buildQuery(opts).subject)
	var out ConsolidationReport
	if err := c.call(ctx, "consolidate_memory", args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ContextItem is an item reached from an anchor.
type ContextItem struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Subject    string  `json:"subject,omitempty"`
	Title      string  `json:"title"`
	Content    string  `json:"content"`
	Confidence string  `json:"confidence,omitempty"`
	IsAnchor   bool    `json:"is_anchor,omitempty"`
	Score      float32 `json:"score,omitempty"`
}

// Context follows links from an anchor (an id, a subject or part of a title)
// with decaying activation, and returns the anchor items and the neighbourhood
// they reach, by relevance. maxDepth 0 uses the server default (3, at most 8).
func (c *Client) Context(ctx context.Context, anchor string, maxDepth int) ([]ContextItem, error) {
	args := map[string]any{"anchor": anchor}
	put(args, "max_depth", maxDepth)
	var out struct {
		Items []ContextItem `json:"items"`
	}
	if err := c.call(ctx, "get_memory_context", args, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
