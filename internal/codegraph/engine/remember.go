package engine

// Memory notes: what an agent asked the server to keep across sessions.
//
// A voice session ends when the user hangs up, and the next one starts blank;
// "what did we discuss last time" then has no answer, even though the server
// answered every question in it. A note is the smallest unit that fixes that:
// one Memory node, written at runtime into a map that persists like any other,
// read back with the same search tools (kind=Memory order=at:desc is "the last
// things remembered"). The write path is Put, not UpdateByPaths: a note has no
// file to re-parse.
//
// Notes also expire. A memory that never forgets accumulates every session's
// running notes, and "what did we decide" then ranks a note from March beside
// one from today with nothing to tell them apart. Each note carries an
// "expires" attribute — its own ttl, or the registry's default — and expired
// notes are swept out on every load of a notes map (so no reader ever sees
// one) and on a timer (so a map nobody reads still shrinks). Forget is the
// same removal on demand, by qualified name. A note written before expiry
// existed carries no "expires" and is kept forever: the migration must not
// silently delete what an agent was told was remembered.

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/mcpserver"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// Note and Remembered are the mcpserver types under their engine names: the
// seam is declared over there because engine imports mcpserver (Registry
// implements Resolver), and an alias keeps one definition instead of a copy
// that could drift.
type Note = mcpserver.Note

// Remembered reports where a note landed.
type Remembered = mcpserver.Remembered

// Forgotten reports a note removed by Forget.
type Forgotten = mcpserver.Forgotten

// memoryKind is the node kind every note carries. A caller selects the set by
// it (kind=Memory) and the widening note names it, so it is a constant, not a
// per-call choice.
const memoryKind = "Memory"

// memorySummary is the router description a memory map is created with. It is
// explicit rather than derived from stats so a bare "what did we decide last
// time" routes here without the caller naming the map.
func memorySummary(label string) string {
	return label + " — memory: notes an agent chose to remember across sessions (what was asked, answered, decided)"
}

// noteResult shapes one note as a one-node parser.Result under its own file.
// One file per note, so a later note never rewrites an earlier one's nodes and
// the vector cache is hit for everything but the new text. The name is the
// timestamp — unique and sortable, so a list of names reads as a timeline —
// with the title's slug appended when there is one.
//
// expires is when the note dies; zero means never, and then no "expires"
// attribute is written at all (see expiresAt).
func noteResult(n Note, at, expires time.Time) (rel string, res parser.Result, name string) {
	name = at.UTC().Format("20060102T150405.000Z")
	if s := slug(n.Title); s != "" {
		name += "-" + s
	}
	rel = "memory/" + name + ".md"
	attrs := make(map[string]string, len(n.Attrs)+2)
	for k, v := range n.Attrs {
		attrs[k] = v
	}
	// Fixed-width milliseconds, not RFC3339Nano: "at" is sorted as a string
	// (order=at:desc), and Nano trims trailing zeros, so ".86Z" would sort
	// after ".862Z". Fixed width keeps lexical order equal to time order, and
	// the value still parses as RFC3339.
	attrs["at"] = at.UTC().Format(atFormat)
	if !expires.IsZero() {
		attrs[expiresAttr] = expires.UTC().Format(atFormat)
	}
	if n.Title != "" {
		attrs["title"] = n.Title
	}
	res = parser.Result{Nodes: []parser.NodeInfo{{
		Kind:     memoryKind,
		Name:     name,
		FilePath: rel,
		Language: "memory",
		Content:  n.Text,
		Body:     n.Text,
		Attrs:    attrs,
	}}}
	return rel, res, name
}

// atFormat is the "at" and "expires" attributes' layout: UTC, fixed-width
// milliseconds.
const atFormat = "2006-01-02T15:04:05.000Z07:00"

// expiresAttr is the attribute holding a note's expiry in atFormat. Absent
// on a note that never expires — including every note written before expiry
// existed, which is the migration: an old note is kept, not silently swept.
const expiresAttr = "expires"

// SetMemoryTTL sets the lifetime of a note that names none. Zero restores
// the built-in default (mcpserver.DefaultMemoryTTL, a week); a negative
// duration (mcpserver.TTLNever) makes notes permanent unless they say
// otherwise.
func (r *Registry) SetMemoryTTL(d time.Duration) {
	if d == 0 {
		d = mcpserver.DefaultMemoryTTL
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.memoryTTL = d
}

// expiresAt resolves a note's expiry: its own TTL, else the registry default;
// negative (never) yields the zero time, which noteResult writes as no
// attribute.
func (r *Registry) expiresAt(n Note, at time.Time) time.Time {
	ttl := n.TTL
	if ttl == 0 {
		r.mu.RLock()
		ttl = r.memoryTTL
		r.mu.RUnlock()
	}
	if ttl < 0 {
		return time.Time{}
	}
	return at.Add(ttl)
}

// expired reports whether a node's "expires" attribute is at or before now.
// A missing or unreadable value is "never": the sweep must only remove what
// it can prove is dead.
func expired(n *graph.Node, now time.Time) bool {
	v := n.Attrs[expiresAttr]
	if v == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return false
	}
	return !t.After(now)
}

// isNotesMap reports whether a graph holds notes and nothing else. Only such
// a map is swept: the check is one pass at load time, cached on the entry, so
// the hot load path of a code map never scans its nodes for expiry.
func isNotesMap(g *graph.Graph) bool {
	by := g.Stats().ByKind
	return by[memoryKind] > 0 && len(by) == 1
}

// slugMaxChars bounds a title's contribution to a node name; the timestamp in
// front of it already makes the name unique.
const slugMaxChars = 40

// slug lowercases s and keeps [a-z0-9] runs joined by "-", capped at
// slugMaxChars, so a spoken title becomes a name that survives a URL, a shell
// and a qualified name.
func slug(s string) string {
	var sb strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		switch {
		case ok:
			if dash && sb.Len() > 0 {
				sb.WriteByte('-')
			}
			dash = false
			sb.WriteRune(r)
		default:
			dash = true
		}
		if sb.Len() >= slugMaxChars {
			break
		}
	}
	return strings.TrimRight(sb.String(), "-")
}

// SetSnapshotDir names the directory a memory map created at runtime is
// written into. Registered snapshots each know their own path; a map that does
// not exist yet has nowhere to go without this.
func (r *Registry) SetSnapshotDir(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshotDir = dir
}

// Remember writes one note into the map named label, creating the map when it
// does not exist, and flushes the snapshot at once. The immediate flush is
// deliberate: with an idle TTL of 0 the only other flush is shutdown, and a
// crash in between would lose every note since; a memory map is small, so the
// write costs milliseconds.
func (r *Registry) Remember(ctx context.Context, label string, n Note) (Remembered, error) {
	if !r.writable {
		return Remembered{}, fmt.Errorf("snapshot dir is read-only (served from an immutable OCI image); run with -writable on a writable dir to remember notes")
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return Remembered{}, fmt.Errorf("map label is required")
	}
	if strings.TrimSpace(n.Text) == "" {
		return Remembered{}, fmt.Errorf("text is required: a note with nothing in it cannot be found again")
	}

	e := r.entry(label)
	if e == nil {
		created, err := r.createMemoryMap(ctx, label)
		if err != nil {
			return Remembered{}, err
		}
		e = created
	} else if _, err := r.load(ctx, e); err != nil { // ensure resident before writing
		return Remembered{}, err
	}
	e.mu.Lock()
	ixr := e.ixr
	e.mu.Unlock()
	if ixr == nil {
		return Remembered{}, fmt.Errorf("map %q is not loaded", label)
	}
	if held := foreignKinds(ixr.Graph()); held != "" {
		// Defence in depth behind the tool surface, which no longer takes a map
		// at all: a note in a code or catalog map is memory poisoning — a later
		// search there ranks the agent's own sentence beside the parsed truth
		// with nothing to tell them apart. Only an empty map or one holding
		// nothing but notes may receive one.
		return Remembered{}, fmt.Errorf("map %q is not a notes map (it holds %s); remember writes only to the agent-notes map", e.label, held)
	}

	// Re-asserted on every write, not only at creation: a notes map reloaded
	// by a build that did not carry the explicit summary through load (see
	// Registry.loadEntry) flushed a stats-derived one over it, and the file
	// on disk may still say so. Setting it here heals that snapshot on its
	// next flush instead of leaving it to a rebuild that never comes.
	ixr.SetExplicitSummary(memorySummary(e.label))

	at := time.Now()
	expires := r.expiresAt(n, at)
	rel, res, name := noteResult(n, at, expires)
	if err := ixr.Put(ctx, rel, res); err != nil {
		return Remembered{}, fmt.Errorf("remember into %s: %w", e.label, err)
	}
	e.mu.Lock()
	e.dirty = true
	e.notesOnly = true // it held nothing or only notes, and now holds this one
	e.lastAccess = at
	e.mu.Unlock()
	if err := r.flush(ctx, e); err != nil {
		return Remembered{}, err
	}
	return Remembered{
		Map:     e.label,
		QN:      repoRelPath(e.label, rel+"::"+name),
		At:      at,
		Expires: expires,
		Notes:   ixr.Graph().Stats().ByKind[memoryKind],
	}, nil
}

// Forget removes the note qn from the map label and flushes at once. It is
// remember's undo and nothing wider: the qn must name a Memory node, in a map
// holding only notes, and label must be that map — so a caller cannot reach a
// code or catalog node through it, and a qn from another map is refused by
// name rather than silently not found.
func (r *Registry) Forget(ctx context.Context, label, qn string) (Forgotten, error) {
	if !r.writable {
		return Forgotten{}, fmt.Errorf("snapshot dir is read-only (served from an immutable OCI image); run with -writable on a writable dir to forget notes")
	}
	label = strings.TrimSpace(label)
	qn = strings.TrimSpace(qn)
	if label == "" {
		return Forgotten{}, fmt.Errorf("map label is required")
	}
	if qn == "" {
		return Forgotten{}, fmt.Errorf("qn is required")
	}
	if i := strings.Index(qn, "//"); i > 0 && !strings.EqualFold(qn[:i], label) {
		return Forgotten{}, fmt.Errorf("forget removes only agent notes in map %q; %q is in map %q", label, qn, qn[:i])
	}
	e := r.entry(label)
	if e == nil {
		return Forgotten{}, fmt.Errorf("no map named %q; registered maps: %s", label, strings.Join(r.Labels(), ", "))
	}
	res, err := r.load(ctx, e)
	if err != nil {
		return Forgotten{}, err
	}
	e.mu.RLock()
	ixr := e.ixr
	e.mu.RUnlock()
	if ixr == nil {
		return Forgotten{}, fmt.Errorf("map %q is not loaded", label)
	}
	if held := foreignKinds(res.Graph); held != "" {
		return Forgotten{}, fmt.Errorf("map %q is not a notes map (it holds %s); forget removes only agent notes", e.label, held)
	}
	n, ok := res.Graph.Get(qn)
	if !ok {
		return Forgotten{}, fmt.Errorf("no note %q in map %q (it may have expired or been forgotten already; recall lists what is there)", qn, e.label)
	}
	if n.Kind != memoryKind {
		return Forgotten{}, fmt.Errorf("forget removes only agent notes; %q is Kind=%s in map %q", qn, n.Kind, e.label)
	}
	ixr.SetExplicitSummary(memorySummary(e.label)) // same heal as Remember
	if _, err := ixr.Remove(ctx, []string{n.FilePath}); err != nil {
		return Forgotten{}, fmt.Errorf("forget %s: %w", qn, err)
	}
	e.mu.Lock()
	e.dirty = true
	e.lastAccess = time.Now()
	e.mu.Unlock()
	if err := r.flush(ctx, e); err != nil {
		return Forgotten{}, err
	}
	return Forgotten{Map: e.label, QN: qn, Notes: ixr.Graph().Stats().ByKind[memoryKind]}, nil
}

// SweepMemory removes every note in the map label whose expiry is at or
// before now and flushes when anything went. label must be resident-able; a
// map that is not a notes map is left alone and reports zero.
func (r *Registry) SweepMemory(ctx context.Context, label string, now time.Time) (int, error) {
	e := r.entry(strings.TrimSpace(label))
	if e == nil {
		return 0, fmt.Errorf("no map named %q", label)
	}
	if _, err := r.loadEntry(ctx, e); err != nil {
		return 0, err
	}
	return r.sweepExpired(ctx, e, now)
}

// sweepExpired is the sweep on one entry: a scan for dead notes, then — only
// when there are any — one Remove, one flush. Serialized by sweepMu so two
// readers who found the same dead note do not both rebuild; the second scan
// finds nothing. Read-only registries never reach here (load gates on
// writable), because a sweep is a write to the snapshot.
func (r *Registry) sweepExpired(ctx context.Context, e *projectEntry, now time.Time) (int, error) {
	e.mu.RLock()
	ixr, notes := e.ixr, e.notesOnly
	e.mu.RUnlock()
	if ixr == nil || !notes {
		return 0, nil
	}
	dead := expiredRels(ixr.Graph(), now)
	if len(dead) == 0 {
		return 0, nil
	}
	r.sweepMu.Lock()
	defer r.sweepMu.Unlock()
	dead = expiredRels(ixr.Graph(), now) // re-scan under the lock: a concurrent sweep may have taken them
	if len(dead) == 0 {
		return 0, nil
	}
	removed, err := ixr.Remove(ctx, dead)
	if err != nil {
		return removed, fmt.Errorf("sweep %s: %w", e.label, err)
	}
	e.mu.Lock()
	e.dirty = true
	e.mu.Unlock()
	if err := r.flush(ctx, e); err != nil {
		return removed, err
	}
	return removed, nil
}

// expiredRels lists the note files (one per note) whose node has expired.
func expiredRels(g *graph.Graph, now time.Time) []string {
	var rels []string
	for _, n := range g.Nodes() {
		if n.Kind == memoryKind && expired(n, now) {
			rels = append(rels, n.FilePath)
		}
	}
	return rels
}

// StartMemorySweeper sweeps every resident notes map on a timer until ctx is
// done. The load-time sweep already keeps readers honest; this one is for a
// map nobody has read in a while, so the snapshot on disk (and what a fresh
// process would register) shrinks without waiting for a search. No-op when
// every <= 0 or the registry is read-only.
//
// It returns a wait func that blocks until the sweeper has stopped and any
// sweep already in flight has finished. A sweep removes the notes from the
// graph and only then flushes the snapshot (sweepExpired), so "the graph holds
// no expired note" does not mean the writing is over: a caller that stops
// watching there races the flush. The server ignores this — its sweeper lives
// as long as the process — but a test whose snapshot dir is removed when it
// returns must wait, or RemoveAll deletes the file under os.Create and fails
// with the directory not empty.
func (r *Registry) StartMemorySweeper(ctx context.Context, every time.Duration) (wait func()) {
	if every <= 0 || !r.writable {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				r.mu.RLock()
				entries := make([]*projectEntry, 0, len(r.entries))
				for _, e := range r.entries {
					entries = append(entries, e)
				}
				r.mu.RUnlock()
				for _, e := range entries {
					n, err := r.sweepExpired(ctx, e, now)
					if err != nil {
						r.logger.Warn().Err(err).Str("map", e.label).Msg("memory: timed expiry sweep failed")
					} else if n > 0 {
						r.logger.Info().Str("map", e.label).Int("removed", n).Msg("memory: swept expired notes")
					}
				}
			}
		}
	}()
	return func() { <-done }
}

// foreignKinds renders the kinds a graph holds other than Memory, most
// numerous first, as "Kind=N Kind=N"; empty when the graph is empty or holds
// notes only. This is what the refusal above names, so the caller can see
// what map they hit.
func foreignKinds(g *graph.Graph) string {
	by := g.Stats().ByKind
	kinds := make([]string, 0, len(by))
	for k := range by {
		if k != memoryKind {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) == 0 {
		return ""
	}
	sort.Slice(kinds, func(i, j int) bool {
		if by[kinds[i]] != by[kinds[j]] {
			return by[kinds[i]] > by[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = fmt.Sprintf("%s=%d", k, by[k])
	}
	return strings.Join(parts, " ")
}

// createMemoryMap registers an empty, resident, writable map under label. It
// is routable from the start: the explicit summary is embedded into the entry's
// routing vector the same way AddSnapshotBytes synthesizes one for a gob that
// ships without it.
func (r *Registry) createMemoryMap(ctx context.Context, label string) (*projectEntry, error) {
	r.mu.RLock()
	dir := r.snapshotDir
	r.mu.RUnlock()
	if dir == "" {
		return nil, fmt.Errorf("no map named %q and no snapshot dir to create it in; registered maps: %s", label, strings.Join(r.Labels(), ", "))
	}

	ixr, err := NewIndexerFromResults(ctx, label, "memory://"+label, nil, r.embedder, r.logger)
	if err != nil {
		return nil, fmt.Errorf("create memory map %s: %w", label, err)
	}
	summary := memorySummary(label)
	ixr.SetExplicitSummary(summary)
	ixr.Index().UseStats(r.searchStats)
	summaryVec, err := r.embedder.EmbedQuery(ctx, summary)
	if err != nil {
		return nil, fmt.Errorf("embed summary for memory map %s: %w", label, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[label]; e != nil { // lost the race with a concurrent create or a watcher registration
		return e, nil
	}
	e := &projectEntry{
		label:         label,
		repoPath:      "memory://" + label,
		snapshotPath:  filepath.Join(dir, label+".graph.gob.gz"),
		summary:       summary,
		summarySource: "explicit",
		summaryVec:    summaryVec,
		ixr:           ixr,
		loaded:        true,
		notesOnly:     true, // created for notes; empty until the first lands
		lastAccess:    time.Now(),
	}
	r.entries[label] = e
	r.stats.recordLoad()
	r.logger.Info().Str("map", label).Str("path", e.snapshotPath).Msg("created memory map")
	return e, nil
}
