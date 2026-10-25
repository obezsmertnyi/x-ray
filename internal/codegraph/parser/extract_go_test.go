//go:build cgo

package parser

import "testing"

// callHints parses src as one Go file and returns its calls edges keyed by the
// called name, so a test can read the shape hint each call site was given.
func callHints(t *testing.T, src string) map[string]EdgeInfo {
	t.Helper()
	res, err := New().ParseFile("p/a.go", []byte(src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	out := map[string]EdgeInfo{}
	for _, e := range res.Edges {
		if e.Kind == EdgeCalls {
			out[e.Extra["callName"]] = e
		}
	}
	return out
}

// TestExtractCall_HintNamesTheShape pins the contract documented on EdgeInfo:
// each call site carries exactly one shape hint, and the hint is decided by the
// operand's shape, not by how much of its type the walk could infer. The
// chained forms are the ones that used to collapse into a bare call —
// `d.Resolver.UpdateByPaths()` became `UpdateByPaths()` — and were then
// resolved by the wrong rule (package-local functions), which is how two real
// call sites went missing from an impact query.
func TestExtractCall_HintNamesTheShape(t *testing.T) {
	src := `package p

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/x/repo/internal/recall/engine"
)

type T struct{}

func foo() {}

func (t *T) m() {}

func (t *T) run(d Deps, xs []Item, w http.ResponseWriter, a, b int) {
	foo()
	fmt.Println("x")
	t.m()
	d.Resolver.UpdateByPaths(nil, nil)
	t.Status().Update(nil, nil)
	os.Stdout.Write(nil)
	xs[0].Do()
	(d).Close()
	d.
		Multi().
		Line()
	d.Run()
	w.Write(nil)
	w.Header().Set("k", "v")
	fs := flag.NewFlagSet("x", 0)
	fs.String("n", "", "")
	var reg *engine.Registry
	reg.Resolve(nil, "", "")
	ixr := engine.NewIndexer(nil)
	ixr.Update(nil, nil)
	local := NewT()
	local.Go()
	a.Add(b)
}
`
	hints := callHints(t, src)
	cases := []struct{ name, key, want, pkg, target string }{
		{"foo", "bare", "1", "", "foo"},
		{"Println", "pkg", "fmt", "", "fmt.Println"},
		{"m", "recvType", "T", "", "T.m"},
		{"UpdateByPaths", "recvVar", "d.Resolver", "", "d.Resolver.UpdateByPaths"},
		{"Update", "recvVar", "ixr", "github.com/x/repo/internal/recall/engine", "ixr.Update"},
		{"Write", "recvType", "ResponseWriter", "net/http", "net/http.ResponseWriter.Write"},
		{"Do", "recvType", "Item", "", "Item.Do"}, // xs []Item: an element of a declared slice is typed
		{"Close", "recvVar", "(d)", "", "(d).Close"},
		{"Line", "recvVar", "d.Multi()", "", "d.Multi().Line"},
		{"Run", "recvType", "Deps", "", "Deps.Run"},
		{"Set", "recvVar", "w.Header()", "", "w.Header().Set"},
		{"String", "recvVar", "fs", "flag", "fs.String"},
		{"Resolve", "recvType", "Registry", "github.com/x/repo/internal/recall/engine", "github.com/x/repo/internal/recall/engine.Registry.Resolve"},
		{"Go", "recvVar", "local", "", "local.Go"}, // local := NewT(): typed by NewT's result at link time
		{"Add", "recvType", "int", "", "int.Add"},
	}
	// os.Stdout.Write and w.Write share a callName; the map keeps the last one
	// emitted (w.Write), so check the chain rooted in an import separately.
	// w.Header().Set carries its evidence as a chain from the bound root.
	if e := hints["Set"]; e.Extra["chain"] != "Header()" || e.Extra["rootType"] != "ResponseWriter" || e.Extra["rootPkg"] != "net/http" {
		t.Errorf("Set: chain evidence = %v, want chain=Header() rootType=ResponseWriter rootPkg=net/http", e.Extra)
	}
	if e := hints["Go"]; e.Extra["rootFunc"] != "NewT" || e.Extra["chain"] != "" {
		t.Errorf("Go: a value from a local function is rooted in it, got %v", e.Extra)
	}
	shapeKeys := []string{"bare", "pkg", "recvType", "recvVar"}
	for _, c := range cases {
		e, ok := hints[c.name]
		if !ok {
			t.Errorf("%s: no calls edge emitted", c.name)
			continue
		}
		if got := e.Extra[c.key]; got != c.want {
			t.Errorf("%s: %s=%q, want %q (extra=%v)", c.name, c.key, got, c.want, e.Extra)
		}
		if got := e.Extra["recvPkg"]; got != c.pkg {
			t.Errorf("%s: recvPkg=%q, want %q (extra=%v)", c.name, got, c.pkg, e.Extra)
		}
		if e.Target != c.target {
			t.Errorf("%s: provisional target %q, want %q", c.name, e.Target, c.target)
		}
		n := 0
		for _, k := range shapeKeys {
			if e.Extra[k] != "" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: %d shape hints, want exactly 1: %v", c.name, n, e.Extra)
		}
		if e.Source != "p/a.go::T.run" {
			t.Errorf("%s: attributed to %q, want p/a.go::T.run", c.name, e.Source)
		}
	}
}

// TestExtractCall_ChainRootedInImportIsExternal covers the shape the map above
// cannot hold twice: os.Stdout.Write(nil), a chain rooted in an imported
// package, is external by construction like pkg.Func().
func TestExtractCall_ChainRootedInImportIsExternal(t *testing.T) {
	src := "package p\n\nimport \"os\"\n\nfunc f() { os.Stdout.Write(nil) }\n"
	e, ok := callHints(t, src)["Write"]
	if !ok {
		t.Fatal("no calls edge for os.Stdout.Write")
	}
	if e.Extra["pkg"] != "os" || e.Target != "os.Stdout.Write" || e.Extra["recvVar"] != "" {
		t.Fatalf("chain rooted in an import should carry pkg=os, got extra=%v target=%q", e.Extra, e.Target)
	}
}

// TestExtract_DeclaredTypesAndChains pins the evidence the chain rule runs on:
// a struct's field types, a function's single result type, and the chain hint
// a call site carries when its root variable is bound — including inside a
// closure, and after a multi-value `x, err :=` from another package.
func TestExtract_DeclaredTypesAndChains(t *testing.T) {
	src := `package p

import (
	"net/http"

	"github.com/x/repo/internal/recall/index"
)

type Server struct {
	memory *MemoryStore
	http.Handler
	a, b int
	idx  *index.Index
	m    map[string]int
	nodes   []*Node
	byLabel map[*Entry]*Runner
	fn      func() int
}

func (s *Server) Nodes() []*Node { return s.nodes }

func (s *Server) Index() *index.Index { return s.idx }
func (s *Server) Pair() (*Graph, error) { return nil, nil }
func (s *Server) Triple() (int, string, error) { return 0, "", nil }
func (s *Server) Named() (g *Graph)  { return nil }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.memory.Upsert(nil)
	s.Index().Search(nil)
	req, err := http.NewRequest("GET", "/", nil)
	req.Header.Set("k", "v")
	err.Error()
	h := func(rw http.ResponseWriter, q *http.Request) {
		rw.WriteHeader(200)
		s.memory.List()
	}
	h(w, r)
	xs[0].m()
	g, err := s.Pair()
	g.Stats()
	mem := s.memory
	mem.Get("id")
	deeper := mem.Index()
	deeper.Search(nil)
	for _, n := range s.nodes {
		n.Touch()
	}
	for k, v := range s.byLabel {
		k.Label()
		v.Run()
	}
	items := []*Item{}
	for i, it := range items {
		it.Do()
		i.Nope()
	}
	items[0].Done()
	for _, n := range s.Nodes() {
		n.Visit()
	}
	reg, err := registryWithRepo(nil)
	reg.UpdateByPaths(nil)
	reg.memory.Flush()
}
`
	res, err := New().ParseFile("p/a.go", []byte(src))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	byQN := map[string]NodeInfo{}
	for _, n := range res.Nodes {
		byQN[n.Qualified()] = n
	}
	idx := "github.com/x/repo/internal/recall/index"

	srv := byQN["p/a.go::Server"]
	wantFields := map[string]TypeRef{
		"memory": {Name: "MemoryStore"}, "Handler": {Name: "Handler", Pkg: "net/http"},
		"a": {Name: "int"}, "b": {Name: "int"}, "idx": {Name: "Index", Pkg: idx},
		"m":       {Name: "int", Coll: "map", Key: &TypeRef{Name: "string"}},
		"nodes":   {Name: "Node", Coll: "slice"},
		"byLabel": {Name: "Runner", Coll: "map", Key: &TypeRef{Name: "Entry"}},
		// fn: a func type is nothing a chain can be followed on
	}
	if len(srv.Fields) != len(wantFields) {
		t.Fatalf("Server.Fields = %v, want %v", srv.Fields, wantFields)
	}
	for k, v := range wantFields {
		if got := srv.Fields[k]; !sameRef(got, v) {
			t.Errorf("Server.Fields[%s] = %v, want %v", k, got, v)
		}
	}
	for qn, want := range map[string]TypeRef{
		"p/a.go::Server.Index":  {Name: "Index", Pkg: idx},
		"p/a.go::Server.Pair":   {Name: "Graph"}, // (T, error) is a T
		"p/a.go::Server.Triple": {},
		"p/a.go::Server.Named":  {Name: "Graph"},
		"p/a.go::Server.Nodes":  {Name: "Node", Coll: "slice"},
	} {
		if got := byQN[qn].Result; !sameRef(got, want) {
			t.Errorf("%s.Result = %v, want %v", qn, got, want)
		}
	}

	hints := callHints(t, src)
	cases := []struct {
		name  string
		extra map[string]string
	}{
		{"Upsert", map[string]string{"recvVar": "s.memory", "chain": "memory", "rootType": "Server"}},
		{"Set", map[string]string{"recvVar": "req.Header", "chain": "Header", "rootPkg": "net/http"}},
		{"Error", map[string]string{"recvVar": "err", "recvPkg": "net/http"}},
		{"WriteHeader", map[string]string{"recvType": "ResponseWriter", "recvPkg": "net/http"}},
		{"List", map[string]string{"recvVar": "s.memory", "chain": "memory", "rootType": "Server"}},
		{"m", map[string]string{"recvVar": "xs[0]"}},
		// Values derived from bound variables keep their provenance as a chain.
		{"Stats", map[string]string{"recvVar": "g", "chain": "Pair()", "rootType": "Server"}},
		{"Get", map[string]string{"recvVar": "mem", "chain": "memory", "rootType": "Server"}},
		{"Search", map[string]string{"recvVar": "deeper", "chain": "memory.Index()", "rootType": "Server"}},
		// Range variables and index expressions over declared collections.
		{"Touch", map[string]string{"recvVar": "n", "chain": "nodes.[]", "rootType": "Server"}},
		{"Label", map[string]string{"recvVar": "k", "chain": "byLabel.[k]", "rootType": "Server"}},
		{"Run", map[string]string{"recvVar": "v", "chain": "byLabel.[]", "rootType": "Server"}},
		{"Do", map[string]string{"recvType": "Item"}},   // it := range items ([]*Item)
		{"Nope", map[string]string{"recvVar": "i"}},     // a slice index is an int: unbound
		{"Done", map[string]string{"recvType": "Item"}}, // items[0]
		{"Visit", map[string]string{"recvVar": "n", "chain": "Nodes().[]", "rootType": "Server"}},
		// A local helper's return value is rooted in the helper, not in a name convention.
		{"UpdateByPaths", map[string]string{"recvVar": "reg", "rootFunc": "registryWithRepo"}},
		{"Flush", map[string]string{"recvVar": "reg.memory", "rootFunc": "registryWithRepo", "chain": "memory"}},
	}
	for _, c := range cases {
		e, ok := hints[c.name]
		if !ok {
			t.Errorf("%s: no calls edge emitted", c.name)
			continue
		}
		c.extra["callName"] = c.name
		if len(e.Extra) != len(c.extra) {
			t.Errorf("%s: extra = %v, want exactly %v", c.name, e.Extra, c.extra)
			continue
		}
		for k, v := range c.extra {
			if e.Extra[k] != v {
				t.Errorf("%s: extra[%s] = %q, want %q (extra=%v)", c.name, k, e.Extra[k], v, e.Extra)
			}
		}
	}
}

// sameRef compares TypeRefs by value, following Key.
func sameRef(a, b TypeRef) bool {
	if a.Name != b.Name || a.Pkg != b.Pkg || a.Coll != b.Coll || (a.Key == nil) != (b.Key == nil) {
		return false
	}
	return a.Key == nil || *a.Key == *b.Key
}
