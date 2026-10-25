// Package sqlddl ingests SQL schema scripts (migrations, release scripts, DDL
// dumps) into the code graph. Each CREATE of a table, view, function/procedure,
// trigger or sequence becomes one node; foreign keys, the relations a view or
// routine reads and writes, and a trigger's table and function become
// references edges, so the graph can answer "what depends on this table".
//
// Scripts are NOT Tree-sitter parsed. There is no grammar for SQL in the set
// this binary vendors, dialects diverge exactly where schema scripts live
// (PL/pgSQL $$ bodies, PARTITION OF, Oracle packages), and one statement a
// strict parser rejects would cost the whole file. Instead the source is split
// into statements — respecting quotes, comments and dollar quotes — and each
// statement's head is matched against the DDL shapes that name an object. It is
// pure Go, so the static build indexes SQL too.
//
// What is deliberately NOT a node:
//   - PARTITION OF tables. A partitioned fact table's release script created
//     7,611 monthly partitions in one repo; as nodes they would outnumber every
//     real table and each would be embedded for nothing.
//   - Indexes and ALTERs. They describe an existing table, so they fold into it
//     (index and added columns into its text when it is in the same file) and
//     contribute foreign-key edges. As nodes, one repo's 37k ALTERs would have
//     drowned its 8k tables.
package sqlddl

import (
	"regexp"
	"strings"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// Node kinds for SQL objects.
const (
	KindTable    = "Table"
	KindView     = "View"
	KindRoutine  = "Routine" // FUNCTION and PROCEDURE
	KindTrigger  = "Trigger"
	KindSequence = "Sequence"
)

// Kinds lists every node kind this package emits.
var Kinds = []string{KindTable, KindView, KindRoutine, KindTrigger, KindSequence}

// Edge hint keys. A SQL reference names an object, not a qualified node, and
// that object is often declared in another script, so resolution happens at
// link time (graph.Build) against every SQL node in the repo:
//
//	ExtraRef      the referenced object's normalized name
//	ExtraRefKind  RefRelation (a table or view) or RefRoutine
//	ExtraFrom     the referencing object's name, set only when it is not
//	              declared in this file (ALTER TABLE x ADD FOREIGN KEY) and so
//	              the edge Source is empty and must be resolved the same way
const (
	ExtraRef     = "sqlRef"
	ExtraRefKind = "sqlRefKind"
	ExtraFrom    = "sqlFrom"
	RefRelation  = "relation"
	RefRoutine   = "routine"
)

const (
	maxBodyChars    = 8192 // matches the code extractors' body cap
	maxContentChars = 600  // embedding text: name, columns, then refs
)

const ident = `(?:"[^"]+"|[A-Za-z_][A-Za-z0-9_$#]*)`
const name = ident + `(?:\s*\.\s*` + ident + `){0,2}`

var (
	reCreateTable = regexp.MustCompile(`(?is)^create\s+(?:or\s+replace\s+)?(?:(?:global|local)\s+)?(?:temp|temporary|unlogged)?\s*table\s+(?:if\s+not\s+exists\s+)?(` + name + `)`)
	rePartitionOf = regexp.MustCompile(`(?is)^\s*partition\s+of\b`)
	reCreateView  = regexp.MustCompile(`(?is)^create\s+(?:or\s+replace\s+)?(?:(?:temp|temporary|recursive)\s+)?(materialized\s+)?view\s+(?:if\s+not\s+exists\s+)?(` + name + `)`)
	reCreateRtn   = regexp.MustCompile(`(?is)^create\s+(?:or\s+replace\s+)?(?:editionable\s+|noneditionable\s+)?(function|procedure)\s+(` + name + `)`)
	reCreateTrg   = regexp.MustCompile(`(?is)^create\s+(?:or\s+replace\s+)?(?:constraint\s+)?trigger\s+(` + name + `)`)
	reCreateSeq   = regexp.MustCompile(`(?is)^create\s+(?:temp\s+|temporary\s+)?sequence\s+(?:if\s+not\s+exists\s+)?(` + name + `)`)
	reCreateIndex = regexp.MustCompile(`(?is)^create\s+(?:unique\s+)?index\s+(?:concurrently\s+)?(?:if\s+not\s+exists\s+)?(` + name + `)?\s*on\s+(?:only\s+)?(` + name + `)\s*(?:using\s+\w+\s*)?(\([^;]*\))?`)
	reAlterTable  = regexp.MustCompile(`(?is)^alter\s+table\s+(?:if\s+exists\s+)?(?:only\s+)?(` + name + `)\s+(.*)$`)
	reAddColumn   = regexp.MustCompile(`(?is)\badd\s+(?:column\s+)?(?:if\s+not\s+exists\s+)?(` + ident + `)\s+([A-Za-z][^,]*)`)

	reReferences = regexp.MustCompile(`(?is)\breferences\s+(` + name + `)`)
	// Relations a query reads or writes. After FROM or JOIN a name followed by
	// "(" is a function call (FROM generate_series(…)), not a relation; after
	// INTO it is a column list (INSERT INTO t (a, b)) and the name is kept.
	reRelUse = regexp.MustCompile(`(?is)\b(from|join|into|update|table)\s+(?:only\s+)?(` + name + `)(\s*\()?`)
	// EXTRACT(month FROM ts) and friends put a column after FROM; so does
	// substring(x FROM 1). Those never resolve to a relation and the linker
	// drops what does not resolve, so no special case is needed here.
	reTrgOn      = regexp.MustCompile(`(?is)\bon\s+(` + name + `)`)
	reTrgExecute = regexp.MustCompile(`(?is)\bexecute\s+(?:function|procedure)\s+(` + name + `)`)
	reRtnArgs    = regexp.MustCompile(`(?s)^\s*(\([^)]*\))`)
	reReturns    = regexp.MustCompile(`(?is)^\s*returns\s+(setof\s+)?(` + name + `(?:\s*\([^)]*\))?(?:\[\])?)`)
	reSpace      = regexp.MustCompile(`\s+`)
)

// sqlKeywords are words reRelUse can capture that are never relation names —
// INSERT INTO … SELECT, UPDATE … SET, "FROM (SELECT", DELETE FROM ONLY.
var sqlKeywords = map[string]bool{
	"select": true, "set": true, "values": true, "only": true, "lateral": true,
	"unnest": true, "strict": true, "temp": true, "temporary": true, "unlogged": true,
	"table": true, "with": true, "if": true, "exists": true, "not": true, "the": true,
}

// Parse turns one SQL script into schema-object nodes + reference edges.
func Parse(path string, source []byte) parser.Result {
	res := parser.Result{}
	byName := map[string]int{}    // normalized name -> index into res.Nodes, this file only
	indexes := map[int][]string{} // table node -> its indexes, for the embedding text
	materialized := map[int]bool{}
	type pending struct {
		from string // "" means the source is res.Nodes[node]
		node int
		ref  string
		kind string
		line int
	}
	var refs []pending

	addRef := func(from string, node int, ref, kind string, line int) {
		if ref == "" || sqlKeywords[ref] {
			return
		}
		refs = append(refs, pending{from: from, node: node, ref: ref, kind: kind, line: line})
	}
	addNode := func(kind, objName string, st statement) int {
		n := parser.NodeInfo{
			Kind:      kind,
			Name:      objName,
			FilePath:  path,
			Language:  "sql",
			LineStart: st.line,
			LineEnd:   st.endLine,
			Body:      capString(st.text, maxBodyChars),
		}
		res.Nodes = append(res.Nodes, n)
		i := len(res.Nodes) - 1
		if _, dup := byName[objName]; !dup {
			byName[objName] = i // first declaration wins, as in graph.Build
		}
		return i
	}

	for _, st := range splitStatements(string(source)) {
		text := st.text
		switch {
		case reCreateTable.MatchString(text):
			m := reCreateTable.FindStringSubmatchIndex(text)
			tbl := normName(text[m[2]:m[3]])
			rest := text[m[1]:]
			if rePartitionOf.MatchString(rest) {
				continue
			}
			cols, _ := columnList(rest)
			i := addNode(KindTable, tbl, st)
			res.Nodes[i].Params = strings.Join(cols, ", ")
			for _, r := range reReferences.FindAllStringSubmatch(rest, -1) {
				addRef("", i, normName(r[1]), RefRelation, st.line)
			}
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(rest)), "as") {
				for _, r := range relUses(rest) {
					addRef("", i, r, RefRelation, st.line)
				}
			}

		case reCreateView.MatchString(text):
			m := reCreateView.FindStringSubmatch(text)
			loc := reCreateView.FindStringIndex(text)
			i := addNode(KindView, normName(m[2]), st)
			materialized[i] = m[1] != ""
			for _, r := range relUses(text[loc[1]:]) {
				addRef("", i, r, RefRelation, st.line)
			}

		case reCreateRtn.MatchString(text):
			m := reCreateRtn.FindStringSubmatch(text)
			loc := reCreateRtn.FindStringIndex(text)
			rest := text[loc[1]:]
			i := addNode(KindRoutine, normName(m[2]), st)
			if a := reRtnArgs.FindStringSubmatch(rest); a != nil {
				res.Nodes[i].Params = reSpace.ReplaceAllString(a[1], " ")
				rest = rest[len(a[0]):]
				if r := reReturns.FindStringSubmatch(rest); r != nil {
					res.Nodes[i].ReturnType = reSpace.ReplaceAllString(strings.TrimSpace(r[1]+r[2]), " ")
				}
			}
			for _, r := range relUses(rest) {
				addRef("", i, r, RefRelation, st.line)
			}

		case reCreateTrg.MatchString(text):
			m := reCreateTrg.FindStringSubmatch(text)
			loc := reCreateTrg.FindStringIndex(text)
			rest := text[loc[1]:]
			i := addNode(KindTrigger, normName(m[1]), st)
			if on := reTrgOn.FindStringSubmatch(rest); on != nil {
				addRef("", i, normName(on[1]), RefRelation, st.line)
			}
			if ex := reTrgExecute.FindStringSubmatch(rest); ex != nil {
				addRef("", i, normName(ex[1]), RefRoutine, st.line)
			}

		case reCreateSeq.MatchString(text):
			m := reCreateSeq.FindStringSubmatch(text)
			addNode(KindSequence, normName(m[1]), st)

		case reCreateIndex.MatchString(text):
			m := reCreateIndex.FindStringSubmatch(text)
			tbl := normName(m[2])
			if i, ok := byName[tbl]; ok && res.Nodes[i].Kind == KindTable {
				idx := "index"
				if m[1] != "" {
					idx += " " + normName(m[1])
				}
				res.Nodes[i].Body = capString(res.Nodes[i].Body+";\n"+text, maxBodyChars)
				indexes[i] = append(indexes[i], idx+" "+reSpace.ReplaceAllString(m[3], " "))
			}

		case reAlterTable.MatchString(text):
			m := reAlterTable.FindStringSubmatch(text)
			tbl, action := normName(m[1]), m[2]
			i, local := byName[tbl]
			local = local && res.Nodes[i].Kind == KindTable
			for _, r := range reReferences.FindAllStringSubmatch(action, -1) {
				if local {
					addRef("", i, normName(r[1]), RefRelation, st.line)
				} else {
					addRef(tbl, -1, normName(r[1]), RefRelation, st.line)
				}
			}
			if local {
				for _, c := range reAddColumn.FindAllStringSubmatch(action, -1) {
					if w := strings.ToLower(c[1]); w == "constraint" || w == "primary" || w == "foreign" || w == "unique" || w == "check" {
						continue
					}
					col := normName(c[1]) + " " + firstWord(c[2])
					res.Nodes[i].Params = joinNonEmpty(res.Nodes[i].Params, col)
				}
			}
		}
	}

	// Content is built last so indexes and ALTER ADD COLUMNs seen after the
	// CREATE are in the embedding text too.
	for i := range res.Nodes {
		res.Nodes[i].Content = content(res.Nodes[i], materialized[i], indexes[i])
	}

	for _, r := range refs {
		e := parser.EdgeInfo{
			Kind:  parser.EdgeReferences,
			File:  path,
			Line:  r.line,
			Extra: map[string]string{ExtraRef: r.ref, ExtraRefKind: r.kind},
		}
		if r.from == "" {
			src := res.Nodes[r.node]
			if src.Name == r.ref {
				continue // a self-referencing FK (parent_id → same table) adds no dependency
			}
			e.Source = src.Qualified()
		} else {
			e.Extra[ExtraFrom] = r.from
		}
		e.Target = "sql:" + r.ref // placeholder; graph.Build resolves or drops it
		res.Edges = append(res.Edges, e)
	}
	return res
}

// content is the embeddable text for a SQL object node: what it is, its name,
// its shape (columns or signature), where it lives, and what it references.
func content(n parser.NodeInfo, materialized bool, indexes []string) string {
	var b strings.Builder
	switch n.Kind {
	case KindTable:
		b.WriteString("SQL table ")
	case KindView:
		if materialized {
			b.WriteString("SQL materialized view ")
		} else {
			b.WriteString("SQL view ")
		}
	case KindRoutine:
		b.WriteString("SQL function ")
	case KindTrigger:
		b.WriteString("SQL trigger ")
	case KindSequence:
		b.WriteString("SQL sequence ")
	}
	b.WriteString(n.Name)
	switch n.Kind {
	case KindTable:
		if n.Params != "" {
			b.WriteString(" (" + n.Params + ")")
		}
		if len(indexes) > 0 {
			b.WriteString("; " + strings.Join(indexes, ", "))
		}
	case KindRoutine:
		b.WriteString(n.Params)
		if n.ReturnType != "" {
			b.WriteString(" returns " + n.ReturnType)
		}
	}
	b.WriteString(" in " + n.FilePath)
	return capString(b.String(), maxContentChars)
}

// relUses returns the distinct relation names a query body reads or writes.
func relUses(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reRelUse.FindAllStringSubmatch(body, -1) {
		if kw := strings.ToLower(m[1]); m[3] != "" && (kw == "from" || kw == "join") {
			continue // FROM name( — a function call
		}
		r := normName(m[2])
		if sqlKeywords[r] || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// columnList reads the parenthesized column list that opens rest and returns
// "name type" per column, skipping table constraints. ok is false when rest
// does not open with "(" (CREATE TABLE … AS SELECT).
func columnList(rest string) (cols []string, ok bool) {
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "(") {
		return nil, false
	}
	depth, start := 0, 1
	var parts []string
	for i, c := range rest {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parts = append(parts, rest[start:i])
				goto done
			}
		case ',':
			if depth == 1 {
				parts = append(parts, rest[start:i])
				start = i + 1
			}
		}
	}
done:
	for _, p := range parts {
		f := strings.Fields(p)
		if len(f) == 0 {
			continue
		}
		switch strings.ToLower(f[0]) {
		case "constraint", "primary", "foreign", "unique", "check", "exclude", "like":
			continue
		}
		col := normName(f[0])
		if len(f) > 1 {
			col += " " + strings.ToLower(f[1])
		}
		cols = append(cols, col)
	}
	return cols, true
}

// normName lowercases an unquoted identifier (SQL folds those), keeps a quoted
// one's case, and drops the quotes and any whitespace around the dots.
func normName(s string) string {
	var parts []string
	for _, p := range strings.Split(s, ".") {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, `"`) && strings.HasSuffix(p, `"`) && len(p) >= 2 {
			parts = append(parts, p[1:len(p)-1])
		} else {
			parts = append(parts, strings.ToLower(p))
		}
	}
	return strings.Join(parts, ".")
}

// BareName is the last dotted segment of a normalized name: "s1.store" →
// "store". The linker falls back to it because one script qualifies a table by
// schema and the next relies on search_path.
func BareName(n string) string {
	if i := strings.LastIndexByte(n, '.'); i >= 0 {
		return n[i+1:]
	}
	return n
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return strings.ToLower(f[0])
	}
	return ""
}

func joinNonEmpty(a, b string) string {
	if a == "" {
		return b
	}
	return a + ", " + b
}

func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// statement is one ;-terminated SQL statement with comments stripped, and the
// 1-based lines it spans in the file.
type statement struct {
	text    string
	line    int
	endLine int
}

// splitStatements splits a script on semicolons that are outside string
// literals, quoted identifiers, comments and dollar-quoted bodies. Comments are
// dropped from the text (so a commented-out CREATE is not a node); the body of
// a $$ … $$ function stays intact, semicolons and all.
func splitStatements(src string) []statement {
	var out []statement
	var b strings.Builder
	line, startLine := 1, 0
	flush := func(end int) {
		t := strings.TrimSpace(b.String())
		if t != "" {
			out = append(out, statement{text: t, line: startLine, endLine: end})
		}
		b.Reset()
		startLine = 0
	}
	mark := func() {
		if startLine == 0 {
			startLine = line
		}
	}
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\n':
			line++
			b.WriteByte(c)
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			i-- // let the newline be counted
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			j := strings.Index(src[i+2:], "*/")
			end := len(src)
			if j >= 0 {
				end = i + 2 + j + 2
			}
			line += strings.Count(src[i:end], "\n")
			b.WriteByte(' ')
			i = end - 1
		case c == '\'' || c == '"':
			mark()
			j := i + 1
			for j < len(src) {
				if src[j] == c {
					if j+1 < len(src) && src[j+1] == c { // doubled quote escape
						j += 2
						continue
					}
					break
				}
				j++
			}
			end := min(j+1, len(src))
			line += strings.Count(src[i:end], "\n")
			b.WriteString(src[i:end])
			i = end - 1
		case c == '$':
			if tag, ok := dollarTag(src[i:]); ok {
				mark()
				j := strings.Index(src[i+len(tag):], tag)
				end := len(src)
				if j >= 0 {
					end = i + len(tag) + j + len(tag)
				}
				line += strings.Count(src[i:end], "\n")
				b.WriteString(src[i:end])
				i = end - 1
				continue
			}
			mark()
			b.WriteByte(c)
		case c == ';':
			flush(line)
		default:
			if c != ' ' && c != '\t' && c != '\r' {
				mark()
			}
			b.WriteByte(c)
		}
	}
	flush(line)
	return out
}

// dollarTag reports whether s opens a PostgreSQL dollar quote ($$ or $tag$)
// and returns the full tag. A positional parameter ($1) is not one.
func dollarTag(s string) (string, bool) {
	for j := 1; j < len(s); j++ {
		c := s[j]
		if c == '$' {
			return s[:j+1], true
		}
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || (j > 1 && c >= '0' && c <= '9')) {
			return "", false
		}
	}
	return "", false
}
