package graph

import (
	"path"
	"strings"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/sqlddl"
)

// sqlTable indexes a repo's SQL schema objects by bare name, for resolving the
// references edges sqlddl emits by object name.
//
// A SQL name is not a Go call: a table's name IS its identity in the database,
// so naming it is the declared evidence, and the question is only which
// declaration of it is meant. Release-script repos declare the same table again
// in every release directory, so the rule narrows by locality and refuses
// ambiguity rather than picking one:
//
//  1. declared in the referencing script itself
//  2. exactly one declaration in the same directory (the same release)
//  3. exactly one declaration in the whole repo
//
// Anything else — two releases' copies and no local one, or a name that is not
// a schema object at all (EXTRACT(month FROM ts) captures "ts") — gets no edge.
// Unlike an unresolved call, an unresolved SQL reference is dropped, not kept as
// an external target: its placeholder names a guess by the line scan, not a
// symbol anyone declared.
type sqlTable struct {
	byBare map[string][]sqlObj
}

type sqlObj struct {
	qn, name, file, kind string
}

func buildSQLTable(nodes []parser.NodeInfo, repo string) *sqlTable {
	t := &sqlTable{byBare: map[string][]sqlObj{}}
	for _, n := range nodes {
		if n.Language != "sql" {
			continue
		}
		b := sqlddl.BareName(n.Name)
		t.byBare[b] = append(t.byBare[b], sqlObj{
			qn: repoQN(repo, n.Qualified()), name: n.Name, file: n.FilePath, kind: n.Kind,
		})
	}
	return t
}

// resolve returns the qualified node a SQL object name refers to from file, or
// ok=false when no single declaration can be chosen.
func (t *sqlTable) resolve(ref, refKind, file string) (string, bool) {
	var cands []sqlObj
	for _, o := range t.byBare[sqlddl.BareName(ref)] {
		if !kindFits(o.kind, refKind) || !schemaFits(o.name, ref) {
			continue
		}
		cands = append(cands, o)
	}
	for _, scope := range []func(sqlObj) bool{
		func(o sqlObj) bool { return o.file == file },
		func(o sqlObj) bool { return path.Dir(o.file) == path.Dir(file) },
		func(sqlObj) bool { return true },
	} {
		in := map[string]bool{} // by qn: a name declared twice in one file is one node
		found := ""
		for _, o := range cands {
			if scope(o) {
				in[o.qn], found = true, o.qn
			}
		}
		switch len(in) {
		case 1:
			return found, true
		case 0:
			continue
		}
		// Several already: each wider scope only adds candidates, so none of
		// them can be unique either.
		return "", false
	}
	return "", false
}

func kindFits(kind, refKind string) bool {
	if refKind == sqlddl.RefRoutine {
		return kind == sqlddl.KindRoutine
	}
	return kind == sqlddl.KindTable || kind == sqlddl.KindView
}

// schemaFits compares two names segment by segment from the right, so a bare
// "store" matches "s1.store" (search_path) but "s2.store" does not match
// "s1.store".
func schemaFits(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i, j := len(as)-1, len(bs)-1; i >= 0 && j >= 0; i, j = i-1, j-1 {
		if as[i] != bs[j] {
			return false
		}
	}
	return true
}
