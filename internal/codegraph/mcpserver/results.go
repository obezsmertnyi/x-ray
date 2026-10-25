package mcpserver

// Reply shaping shared by the live tools (mcpserver.go), the read-only tools
// (readonly.go) and the direct-call twins (direct.go), so the three surfaces
// cannot drift. The rule throughout: a reply shaped by the map's STRUCTURE says
// so. A doc-only map queried with search_graph is searched for what it holds
// and the reply says that; "count: 1" from a Doc seed fed to get_graph_impact
// is correct and reads as "nothing matched", when the truth is "nothing here
// can ever match" — the same silent-success failure that update_code_graph's
// `note` and `unowned` already refuse to allow.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/index"
)

// searchedMap is one map a search ran against: its label (empty in live mode,
// which serves a single unnamed graph) and its graph.
type searchedMap struct {
	Label string
	Graph *graph.Graph
}

// searchKinds decides what one map is searched for: the tool's own kinds when
// the map holds any of them, else every kind the map has — with a note saying
// so. search_graph promises "code, services, or another domain", and a
// document collection queried through it is that other domain, not a dead
// end; it used to answer a bare count: 0 for exactly that, indistinguishable
// from a miss. The reverse holds for search_graph_docs on a map with no
// documents. An empty map keeps the tool's kinds (there is nothing to widen
// to).
//
// The note names the LEVER, not just the kinds. It used to say only what the
// map holds, and a caller asked for "the most experienced student" in the
// team map read that, took the five Answer parts a plain search ranked
// highest, and answered from one fragment — the whole Student set, with its
// years attribute, was one kind=Student away and nothing said so. A model
// reading the note has to be told that a selection exists and how to ask for
// it, in the reply itself, because the tool description is out of view by the
// time the reply lands.
func searchKinds(tool string, kinds map[string]bool, m searchedMap) (map[string]bool, string) {
	// No scope means nothing to widen: the UI's "all" filter searches every
	// kind already, and reporting that as "holds no  node" (an empty list)
	// told the reader the map lacked something it was never asked for.
	if m.Graph == nil || len(kinds) == 0 {
		return kinds, ""
	}
	st := m.Graph.Stats()
	if st.Nodes == 0 {
		return kinds, ""
	}
	for kind := range kinds {
		if st.ByKind[kind] > 0 {
			return kinds, ""
		}
	}
	name := "this map"
	if m.Label != "" {
		name = fmt.Sprintf("map %q", m.Label)
	}
	return nil, fmt.Sprintf("%s holds no %s node, so %s ranked all of its kinds (%s). "+
		"To read one set whole rather than the few the query ranked highest, pass %s; "+
		"a selection returns every member, ordered by the query or by an attribute (order=<key>:desc), "+
		"and reports its size as \"selected\".",
		name, kindList(kinds), tool, countList(st.ByKind), kindLevers(st.ByKind))
}

// kindLevers renders a map's kinds as the argument that selects each — "kind=Student
// or kind=Answer" — most numerous first, so the note hands the caller something to
// paste rather than a vocabulary to translate.
func kindLevers(m map[string]int) string {
	keys := kindsByCount(m)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = "kind=" + k
	}
	return strings.Join(parts, " or ")
}

// parseSelect reads the predicate the CALLER named — kinds and/or an attribute,
// and an order to sort the set by — from a tool's arguments. Kinds are
// comma-separated and are the map's own vocabulary (Student, Service, Doc), not
// the code/docs grouping a tool scopes itself to; get_graph_stats reports which
// a map holds. An order is "key" or "key:desc"; a malformed one is an error like
// a malformed attr, never a silent fall-back to relevance order, which would
// answer a superlative with whatever the query happened to rank first.
func parseSelect(args map[string]any) (index.Select, error) {
	attr, err := index.ParseAttrFilter(argStr(args, "kind_attr"))
	if err != nil {
		return index.Select{}, err
	}
	if attr.Empty() {
		attr, err = index.ParseAttrFilter(argStr(args, "attr"))
		if err != nil {
			return index.Select{}, err
		}
	}
	order, err := index.ParseOrder(strings.TrimSpace(argStr(args, "order")))
	if err != nil {
		return index.Select{}, err
	}
	sel := index.Select{Attr: attr, Order: order}
	if raw := strings.TrimSpace(argStr(args, "kind")); raw != "" {
		kinds := map[string]bool{}
		for _, k := range strings.Split(raw, ",") {
			if k = strings.TrimSpace(k); k != "" {
				kinds[k] = true
			}
		}
		if len(kinds) > 0 {
			sel.Kinds = kinds
		}
	}
	return sel, nil
}

// describeSelect renders the named set for a reply note.
func describeSelect(sel index.Select) string {
	var parts []string
	if len(sel.Kinds) > 0 {
		parts = append(parts, "kind "+kindList(sel.Kinds))
	}
	if !sel.Attr.Empty() {
		parts = append(parts, fmt.Sprintf("attr %s=%s", sel.Attr.Key, sel.Attr.Value))
	}
	if !sel.Order.Empty() {
		dir := "asc"
		if sel.Order.Desc {
			dir = "desc"
		}
		parts = append(parts, fmt.Sprintf("order %s %s", sel.Order.Key, dir))
	}
	return strings.Join(parts, " + ")
}

// selectionNote explains a selection the reply cap cut short, naming the set's
// real size. Silence here is the failure it prevents: a caller that named a set
// cannot tell "these are all of them" from "these are the first few", and will
// reason over a partial set as if it were whole. Empty when nothing was selected
// or nothing was cut.
func selectionNote(sel index.Select, total, returned int) string {
	if sel.Empty() || total <= returned {
		return ""
	}
	return fmt.Sprintf("%s selects %d nodes; this reply carries %d of them. Raise limit (max %d) to read the rest.",
		describeSelect(sel), total, returned, maxSelectionLimit)
}

// emptySelectionNote explains a selection that matched nothing because the map
// does not use the names the caller asked for — the one case where an empty
// reply is a vocabulary mismatch rather than an absent answer, and where saying
// which kinds the map DOES hold turns a dead end into the next query.
func emptySelectionNote(sel index.Select, g *graph.Graph) string {
	if len(sel.Kinds) == 0 || g == nil {
		return ""
	}
	st := g.Stats()
	for k := range sel.Kinds {
		if st.ByKind[k] > 0 {
			return ""
		}
	}
	return fmt.Sprintf("this map holds no %s node (it has %s); those are the kinds it can be selected by.",
		kindList(sel.Kinds), countList(st.ByKind))
}

// attrVocabularyNote explains an order or attr that names an attribute the
// scope does not use, or a value none of its nodes has. Silence here is the
// failure it prevents: the team map was asked for kind=Student
// order=experience:desc, no Student carries "experience", every node fell into
// orderByAttr's "no attribute, sorts last" bucket and the set came back in
// cosine order under a note that read as healthy ("selects 37 nodes; this reply
// carries 5") — the caller took the top hit's 8 years for the most experienced
// when the true answer was 30. The follow-up attr=experience=30 returned 0 with
// no note. Both are vocabulary mismatches, the same class emptySelectionNote
// already names for kinds, so the reply names the keys (or values) the scope's
// nodes DO carry and the caller's next call can use them. The results stand:
// the set is still the right set. Empty when neither order nor attr is set,
// when the scope is empty (an unknown kind is reported by emptySelectionNote),
// or when the vocabulary matched.
func attrVocabularyNote(ix *index.Index, kinds map[string]bool, repoFilter string, sel index.Select, label string) string {
	if ix == nil || (sel.Order.Empty() && sel.Attr.Empty()) {
		return ""
	}
	var notes []string
	if !sel.Order.Empty() {
		v := ix.AttrVocabulary(kinds, repoFilter, sel, "")
		if v.Nodes > 0 && v.Keys[sel.Order.Key] == 0 {
			notes = append(notes, fmt.Sprintf("no %s carries attribute %q, so this order did not apply and the set is in query order; %s",
				scopeNoun(sel, label, false), sel.Order.Key, attrKeysClause(sel, label, v.Keys)))
		}
	}
	if !sel.Attr.Empty() {
		v := ix.AttrVocabulary(kinds, repoFilter, sel, sel.Attr.Key)
		switch {
		case v.Nodes == 0:
		case v.Keys[sel.Attr.Key] == 0:
			notes = append(notes, fmt.Sprintf("no %s carries attribute %q; %s",
				scopeNoun(sel, label, false), sel.Attr.Key, attrKeysClause(sel, label, v.Keys)))
			if n := titledNodeNote(ix, kinds, repoFilter, sel); n != "" {
				notes = append(notes, n)
			}
		case v.Values[sel.Attr.Value] == 0:
			notes = append(notes, fmt.Sprintf("no %s has %s=%s; %s",
				scopeNoun(sel, label, false), sel.Attr.Key, sel.Attr.Value, attrValuesClause(v.Values)))
		}
	}
	// Each note is a sentence; the reply joins notes with a space, so without
	// the period this one runs straight into the selection count after it.
	for i := range notes {
		notes[i] += "."
	}
	return strings.Join(notes, " ")
}

// titledNodeMax caps how many title matches a note lists; past a few the
// caller should be searching, not reading a list.
const titledNodeMax = 3

// titledNodeNote follows an attr whose KEY is unknown with the node(s) whose
// title its VALUE names, when there are any. A caller reaching for
// attr=github=octocat42 wanted the Student called octocat42; "github" was a
// guess at how a title is stored, and four 0-result replies never said the
// handle was one get_graph_node away. Empty when no scope node bears the name.
func titledNodeNote(ix *index.Index, kinds map[string]bool, repoFilter string, sel index.Select) string {
	nodes := ix.TitledNodes(kinds, repoFilter, sel, sel.Attr.Value, titledNodeMax)
	if len(nodes) == 0 {
		return ""
	}
	refs := make([]string, len(nodes))
	for i, n := range nodes {
		refs[i] = fmt.Sprintf("%s node %s", n.Kind, n.QN)
	}
	return fmt.Sprintf("%q is the title of %s — read it with get_graph_node, or put the handle in the query",
		sel.Attr.Value, strings.Join(refs, ", "))
}

// scopeNoun names the set a vocabulary note speaks about: "Student node" when
// the caller selected kinds (the selection IS the scope), else the map, else a
// bare "node". Plural for the clause that lists what the scope carries.
func scopeNoun(sel index.Select, label string, plural bool) string {
	noun := "node"
	if plural {
		noun = "nodes"
	}
	switch {
	case len(sel.Kinds) > 0:
		return kindList(sel.Kinds) + " " + noun
	case label != "":
		return fmt.Sprintf("%s in map %q", noun, label)
	}
	return noun
}

// attrKeysClause renders what the scope's nodes carry, or that they carry
// nothing — a code map ordered by an attribute has no keys to offer, and saying
// so is the answer.
func attrKeysClause(sel index.Select, label string, keys map[string]int) string {
	if len(keys) == 0 {
		return scopeNoun(sel, label, true) + " carry no attributes"
	}
	return scopeNoun(sel, label, true) + " carry: " + attrKeyList(keys)
}

// attrKeyCollapseAbove is the key count past which namespaced flag keys are
// folded into their prefix. Team Student nodes carry dozens of "level:…",
// "tool:…", "fw:…" flags beside a handful of plain keys; listed one by one they
// would bury the two the caller most likely wants (years, role) in a paragraph.
const attrKeyCollapseAbove = 12

// attrKeyList renders attribute keys as "years=37 role=37 …", most carried
// first, ties by name — countList's shape, so it reads like the kinds list
// beside it. Past attrKeyCollapseAbove keys, the ones namespaced with ':' are
// grouped as "level:* (12 keys)" after the plain keys, so the note stays a line
// or two and the plain keys stay visible.
func attrKeyList(keys map[string]int) string {
	if len(keys) <= attrKeyCollapseAbove {
		return countList(keys)
	}
	plain := map[string]int{}
	groups := map[string]int{}
	for k, n := range keys {
		if prefix, _, ok := strings.Cut(k, ":"); ok && prefix != "" {
			groups[prefix]++
			continue
		}
		plain[k] = n
	}
	var parts []string
	if len(plain) > 0 {
		parts = append(parts, countList(plain))
	}
	for _, prefix := range kindsByCount(groups) {
		parts = append(parts, fmt.Sprintf("%s:* (%d keys)", prefix, groups[prefix]))
	}
	return strings.Join(parts, " ")
}

// attrValuesListAbove is the distinct-value count past which a note reports
// only how many values there are; a list of 40 names helps nobody.
const attrValuesListAbove = 20

// attrValuesClause renders a key's distinct values, numerically ordered when
// every one parses as a number (years: "3 4 … 30", not "10 11 12 3"), as strings
// otherwise, quoted only when a value has whitespace. Past attrValuesListAbove
// distinct values it gives the count instead.
func attrValuesClause(values map[string]int) string {
	if len(values) > attrValuesListAbove {
		return fmt.Sprintf("values: %d distinct", len(values))
	}
	vals := make([]string, 0, len(values))
	nums := make(map[string]float64, len(values))
	allNum := true
	for v := range values {
		vals = append(vals, v)
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			nums[v] = f
		} else {
			allNum = false
		}
	}
	sort.Slice(vals, func(i, j int) bool {
		if allNum && nums[vals[i]] != nums[vals[j]] {
			return nums[vals[i]] < nums[vals[j]]
		}
		return vals[i] < vals[j]
	})
	for i, v := range vals {
		if strings.ContainsAny(v, " \t") {
			vals[i] = strconv.Quote(v)
		}
	}
	return "values: " + strings.Join(vals, " ")
}

// attrStats shapes get_graph_stats' "attrs" field — kind → the attribute keys
// its nodes carry, rendered by attrKeyList so it is the same line a search note
// prints. Nil when no kind carries an attribute, and the callers omit the field
// then, so a code map's stats keep their exact shape. Shared by every stats
// surface (mapStatsEntry, which registerStatsRO and StatsDirect both call) so
// the shapes cannot drift; the vocabulary is what a caller needs BEFORE
// filtering or ordering a catalog map by attribute, and the tool descriptions
// point here for it.
func attrStats(g *graph.Graph) map[string]string {
	if g == nil {
		return nil
	}
	byKind := g.AttrKeysByKind()
	if len(byKind) == 0 {
		return nil
	}
	out := make(map[string]string, len(byKind))
	for kind, keys := range byKind {
		out[kind] = attrKeyList(keys)
	}
	return out
}

// rememberResult shapes remember's reply. The note tells the caller how to
// get the note BACK — the map, the kind and the ordering — because a written
// memory that the writer does not know how to read is as good as lost.
func rememberResult(r Remembered) map[string]any {
	expires := "never"
	if !r.Expires.IsZero() {
		expires = r.Expires.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"map":     r.Map,
		"qn":      r.QN,
		"at":      r.At.UTC().Format(time.RFC3339),
		"expires": expires,
		"notes":   r.Notes,
		"note":    fmt.Sprintf("read it back with recall (map %s); forget qn=<qn> removes it early", r.Map),
	}
}

// recallFraming is the sentence every recall reply ends with, empty or not. A
// voice agent was told to add "GitOps" to a student's course expectations and
// wrote a remember note; from then on every question about that student's
// expectations — including "what are the FULL expectations" — was answered
// from recall alone ("I only have one note"), and the team map holding the
// real Answer nodes was never searched again. The note had become the record
// in the agent's head, and nothing in the reply said it was not. This does.
const recallFraming = "These are conversation notes — what people said — not the record. Facts about a named person, service or thing live in the map that holds them: search that map and add these notes on top, saying which is which."

// recallResult shapes recall's reply. Each note carries its FULL text: a note
// is one or two sentences and IS the payload, so the search preview's
// truncation would cut the very thing asked for. Caller facets come back as
// attrs; at/title/expires are lifted into fields. note is always present: a
// status sentence (why the reply is empty, or how it is ordered) followed by
// recallFraming, so silence is never misread as "the server has no memory"
// and a hit is never misread as the whole truth.
func recallResult(label, query string, notes []*graph.Node, note string) map[string]any {
	items := make([]map[string]any, 0, len(notes))
	for _, n := range notes {
		item := map[string]any{"qn": n.QN, "at": n.Attrs["at"], "text": n.Body}
		if item["text"] == "" {
			item["text"] = n.Content
		}
		if t := n.Attrs["title"]; t != "" {
			item["title"] = t
		}
		if e := n.Attrs["expires"]; e != "" {
			item["expires"] = e
		}
		attrs := map[string]string{}
		for k, v := range n.Attrs {
			if !recallAttrs[k] {
				attrs[k] = v
			}
		}
		if len(attrs) > 0 {
			item["attrs"] = attrs
		}
		items = append(items, item)
	}
	out := map[string]any{"map": label, "count": len(items), "notes": items}
	if query != "" {
		out["query"] = query
	}
	var status string
	switch {
	case note != "":
		status = note
	case len(items) == 0 && query != "":
		status = fmt.Sprintf("no note in %q matches %q; recall with no query lists the newest notes", label, query)
	case len(items) == 0:
		status = fmt.Sprintf("nothing has been remembered in %q yet (or every note has expired); remember writes one", label)
	default:
		status = "newest first unless a query ranked them; forget qn=<qn> removes one"
	}
	// The status sentence stays verbatim and first (it names the map or the
	// miss the caller has to act on); a period separates it from the framing
	// so the two read as sentences, not one run-on clause.
	out["note"] = status + ". " + recallFraming
	return out
}

// forgetResult shapes forget's reply: the note that is gone and how many are
// left, so a caller clearing a session can see the count reach zero.
func forgetResult(f Forgotten) map[string]any {
	return map[string]any{
		"map":   f.Map,
		"qn":    f.QN,
		"notes": f.Notes,
	}
}

// impactResult shapes get_graph_impact's reply. Beyond the traversal it reports
// what a bare count cannot: seeds that are not nodes of this map (they are
// dropped, not traversed), and WHY a result is only the seeds — a map with no
// dependency edges at all, or seeds nothing in the map calls or is called by.
func impactResult(g *graph.Graph, seeds []string, depth, maxNodes int) map[string]any {
	var known, unknown []string
	for _, s := range seeds {
		if _, ok := g.Get(s); ok {
			known = append(known, s)
		} else {
			unknown = append(unknown, s)
		}
	}
	impacted := []graph.ImpactNode{}
	if len(known) > 0 {
		impacted = g.ImpactRadius(known, depth, maxNodes)
	}
	out := map[string]any{"seeds": seeds, "depth": depth, "count": len(impacted), "impacted": impacted}

	var notes []string
	if len(unknown) > 0 {
		out["unknown"] = unknown
		notes = append(notes, "unknown seeds are not nodes of this map and were not traversed; "+
			"check the qn with search_graph or get_graph_node.")
	}
	if len(known) > 0 && len(impacted) <= len(known) {
		st := g.Stats()
		if st.DependencyEdges() == 0 {
			notes = append(notes, "this map has no dependency edges at all (it holds "+countList(st.ByEdge)+
				"): it is a document collection or another structure-only map, and a blast radius is not "+
				"defined for it. contains edges are structural and never traversed; use get_graph_node to "+
				"read a node and what it contains.")
		} else {
			notes = append(notes, "no dependency edge (calls/imports/inherits/implements/references) reaches "+
				"beyond the seeds: nothing in this map calls or is called by them. contains edges are "+
				"structural and never traversed, so a File, Doc or Section seed always comes back alone — "+
				"use get_graph_node for its members.")
		}
	}
	if len(notes) > 0 {
		out["note"] = strings.Join(notes, " ")
	}
	return out
}

// countList renders a kind→count map as "Doc=236 Section=332", largest first,
// ties by name — the same shape get_graph_stats prints.
func countList(m map[string]int) string {
	if len(m) == 0 {
		return "nothing"
	}
	keys := kindsByCount(m)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, m[k])
	}
	return strings.Join(parts, " ")
}

// kindsByCount lists a count map's keys most numerous first, ties by name, so
// every rendering of a map's kinds leads with the one a caller most likely wants.
func kindsByCount(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

// kindList renders a kind set as "Class/File/Func/…", sorted.
func kindList(kinds map[string]bool) string {
	keys := make([]string, 0, len(kinds))
	for k := range kinds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "/")
}

// Argument descriptions shared by the live (mcpserver.go) and read-only
// (readonly.go) search tools, so the two surfaces describe one contract. They
// are the only teaching a caller gets before its first reply, and a caller that
// asked "the most experienced student" of the team map never learnt from
// them that a set could be selected and sorted — hence order, and the
// superlative wording on it.
const (
	searchQueryDesc = "Natural-language description of what you are looking for — a function, a service, a catalog entry, a document"
	searchLimitDesc = "Max results (default 5, max 20 — or up to 100 when kind, attr or order selects a set, so the whole set can be read)"
	searchKindDesc  = "Select only nodes of these kinds, comma-separated (e.g. \"Student\"). These are the MAP'S OWN kinds — get_graph_stats lists which a map holds — not the code/docs grouping this tool searches by default. Selecting returns every match ranked by query, and the reply reports the set size as \"selected\"; use it to read a whole set rather than the few entries a query happened to rank highest. Add order to sort the set by an attribute instead."
	searchAttrDesc  = "Select the entries carrying an exact attribute, as \"key=value\" — every match is returned (ranked by query, capped by limit), and the reply reports the set size as \"selected\". Catalog maps only; parsed code nodes have no attributes. get_graph_stats lists each kind's attribute keys (\"attrs\"); a key or value the set does not carry is reported in the reply note together with the ones it does."
	searchOrderDesc = "Sort a selection by an attribute instead of by query similarity, as \"key\" or \"key:desc\" (e.g. \"years:desc\"). Use it for superlatives — most experienced, oldest, largest — which a relevance rank cannot answer; combine with kind or attr to pick the set, and limit to take the top. Numeric values compare as numbers; nodes without the attribute sort last. get_graph_stats lists each kind's attribute keys (\"attrs\"); a key the set does not carry leaves it in query order and is reported in the reply note."
)
