package mcpserver

import (
	"strings"
	"testing"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/docs"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/graph"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// catalogGraph is the shape of a catalog map: an entry whose fields are their
// own nodes, joined to it by contains.
func catalogGraph() *graph.Graph {
	nodes := []parser.NodeInfo{
		{Kind: "Entry", Name: "alpha", FilePath: "c.json",
			Content: "Alpha. Lead. 20 years.", Attrs: map[string]string{"years": "20"}},
		{Kind: "Field", Name: "note", ParentName: "alpha", FilePath: "c.json",
			Content: "Alpha. Note: did not take the course."},
	}
	edges := []parser.EdgeInfo{{
		Kind: parser.EdgeContains, Source: "c.json::alpha", Target: "c.json::alpha.note", File: "c.json",
	}}
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "m", Nodes: nodes, Edges: edges})
	return g
}

// A search can land on a FIELD of an entry, which carries only its own text. The
// reply must name the entry that holds it, or the caller has read one fact and
// has no way to reach the rest — it reports the entry's other facts as unknown
// while they sit one hop away.
func TestNodeJSON_AChildNamesItsParent(t *testing.T) {
	g := catalogGraph()
	out, ok := NodeJSON(g, "m//c.json::alpha.note")
	if !ok {
		t.Fatalf("child node not found")
	}
	parents, _ := out["contained_by"].([]string)
	if len(parents) != 1 || parents[0] != "m//c.json::alpha" {
		t.Fatalf("child must name its parent entry, got contained_by=%v (keys %v)", out["contained_by"], keysOf(out))
	}
	// Following that link must reach the facts the child does not carry.
	parent, ok := NodeJSON(g, parents[0])
	if !ok {
		t.Fatalf("parent qn from contained_by does not resolve: %q", parents[0])
	}
	if body, _ := parent["content"].(string); body == "" {
		t.Fatalf("parent carries no content to reach")
	}
}

// The outgoing direction stays unreported: a file's symbols or an entry's fields
// run to hundreds, and nodeContent already reassembles what a parent holds.
func TestNodeJSON_AParentDoesNotListItsChildren(t *testing.T) {
	out, ok := NodeJSON(catalogGraph(), "m//c.json::alpha")
	if !ok {
		t.Fatalf("entry not found")
	}
	for _, k := range []string{"contains", "contains_by"} {
		if _, present := out[k]; present {
			t.Errorf("reply must not carry %q: %v", k, keysOf(out))
		}
	}
	if _, present := out["contained_by"]; present {
		t.Errorf("a top-level entry has no parent, so contained_by must be absent")
	}
}

// The same dead end exists for documents: a section found by search must name
// the document it belongs to.
func TestNodeJSON_ASectionNamesItsDoc(t *testing.T) {
	res := docs.Parse("notes/a.md", []byte("# Title\n\nIntro.\n\n## One\n\nBody.\n"))
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "m", Nodes: res.Nodes, Edges: res.Edges})

	var section string
	for _, n := range g.Nodes() {
		if n.Kind == docs.KindSection && n.Name == "One" {
			section = n.QN
		}
	}
	if section == "" {
		t.Fatalf("fixture has no section named One")
	}
	out, _ := NodeJSON(g, section)
	parents, _ := out["contained_by"].([]string)
	if len(parents) != 1 {
		t.Fatalf("a section must name its doc, got contained_by=%v", out["contained_by"])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A File node has no body. It lists its symbols, with the qn to fetch each by,
// so a caller that asks for the file is not handed an empty string.
func TestNodeJSON_AFileListsItsSymbols(t *testing.T) {
	nodes := []parser.NodeInfo{
		{Kind: "File", Name: "web.go", FilePath: "web.go"},
		{Kind: "Func", Name: "Handler", FilePath: "web.go", LineStart: 10, LineEnd: 30, Content: "func Handler()"},
	}
	edges := []parser.EdgeInfo{{Kind: parser.EdgeContains, Source: "web.go", Target: "web.go::Handler", File: "web.go"}}
	g := graph.New()
	g.Build(graph.RepoGraph{Repo: "m", Nodes: nodes, Edges: edges})

	var file string
	for _, n := range nodes {
		if n.Kind == "File" {
			file = "m//" + n.FilePath
		}
	}
	out, ok := NodeJSON(g, file)
	if !ok {
		t.Fatalf("file node not found")
	}
	body, _ := out["content"].(string)
	if !strings.Contains(body, "Handler") || !strings.Contains(body, "qn=m//web.go::Handler") || !strings.Contains(body, "lines 10-30") {
		t.Fatalf("file content should list its symbols, got %q", body)
	}
}
