package graph

import (
	"testing"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

func TestResolveScriptCall(t *testing.T) {
	fn := func(file, name, parent string) parser.NodeInfo {
		return parser.NodeInfo{Kind: "Func", Name: name, ParentName: parent, FilePath: file}
	}
	cls := func(file, name string) parser.NodeInfo {
		return parser.NodeInfo{Kind: "Class", Name: name, FilePath: file}
	}
	file := func(p string) parser.NodeInfo { return parser.NodeInfo{Kind: "File", Name: p, FilePath: p} }
	imp := func(from, module, binding, imported string) parser.EdgeInfo {
		return parser.EdgeInfo{Kind: "imports", Source: from, Target: module,
			Extra: map[string]string{parser.ExtraScriptImport: "1", parser.ExtraBinding: binding, parser.ExtraImported: imported}}
	}
	call := func(from, src, name string, hints map[string]string) parser.EdgeInfo {
		x := map[string]string{"callName": name, parser.ExtraScriptCall: "1"}
		for k, v := range hints {
			x[k] = v
		}
		return parser.EdgeInfo{Kind: "calls", Source: from + "::" + src, Target: name, File: from, Extra: x}
	}
	const (
		ts     = "web/src/app/svc.ts"
		tsRepo = "web/src/lib/repo.ts"
		tsUtil = "web/src/util/index.ts"
		py     = "svc/app/main.py"
		pySib  = "svc/app/helpers.py"
		pyPkg  = "svc/core/store/__init__.py"
		pyOth  = "other/core/store/__init__.py"
	)
	nodes := []parser.NodeInfo{
		file(ts), file(tsRepo), file(tsUtil), file(py), file(pySib), file(pyPkg), file(pyOth),
		cls(ts, "Svc"), fn(ts, "run", "Svc"), fn(ts, "local", ""),
		cls(tsRepo, "Repo"), fn(tsRepo, "save", "Repo"), fn(tsUtil, "fmt", ""),
		fn(py, "run", ""), cls(pySib, "Helper"), fn(pySib, "build", "Helper"), fn(pySib, "fn", ""),
		fn(pyPkg, "get", ""), fn(pyOth, "get", ""),
	}
	edges := []parser.EdgeInfo{
		imp(ts, "../lib/repo", "Repo", "Repo"), imp(ts, "../util", "u", "*"), imp(ts, "react", "React", "default"),
		call(ts, "Svc.run", "save", map[string]string{"recvType": "Repo"}),
		call(ts, "Svc.run", "find", map[string]string{"recvType": "Repo"}),
		call(ts, "Svc.run", "gone", map[string]string{"recvType": "Svc", parser.ExtraSelfCall: "1"}),
		call(ts, "Svc.run", "local", map[string]string{"bare": "1"}),
		call(ts, "Svc.run", "fmt", map[string]string{"recvVar": "u"}),
		call(ts, "Svc.run", "useState", map[string]string{"recvVar": "React"}),
		imp(py, ".helpers", "H", "Helper"), imp(py, ".helpers", "f2", "fn"), imp(py, "core.store", "store", ""),
		call(py, "run", "build", map[string]string{"recvVar": "H"}),
		call(py, "run", "f2", map[string]string{"bare": "1"}),
		call(py, "run", "get", map[string]string{"recvVar": "store"}),
		call(py, "run", "join", map[string]string{"recvVar": "os.path"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	got := map[string]string{}
	for _, src := range []string{"r//" + ts + "::Svc.run", "r//" + py + "::run"} {
		for _, e := range g.Callees(src, "calls") {
			got[e.Extra["callName"]] = e.Target
		}
	}
	want := map[string]string{
		"save":     "r//" + tsRepo + "::Repo.save",
		"find":     "r//" + tsRepo + "::Repo", // not declared on Repo: the class
		"gone":     "r//gone",                 // this.gone() the class does not declare
		"local":    "r//" + ts + "::local",
		"fmt":      "r//" + tsUtil + "::fmt", // ../util → util/index.ts
		"useState": "r//useState",            // a package, not a repo file
		"build":    "r//" + pySib + "::Helper.build",
		"f2":       "r//" + pySib + "::fn",
		"get":      "r//" + pyPkg + "::get", // core.store: two trees hold it, the importing one wins
		"join":     "r//join",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s -> %q, want %q", k, got[k], v)
		}
	}
	if ls.ResolvedByRule["script"] != 6 || ls.ResolvedByRule["scriptType"] != 1 {
		t.Errorf("by rule = %v", ls.ResolvedByRule)
	}
}

// "@/lib/repo" resolves through the nearest tsconfig's paths; a package stays
// external even with the alias present.
func TestResolveScriptCall_TSConfigPaths(t *testing.T) {
	const (
		page = "web/app/page.tsx"
		repo = "web/src/lib/repo.ts"
	)
	nodes := []parser.NodeInfo{
		{Kind: "File", Name: page, FilePath: page}, {Kind: "File", Name: repo, FilePath: repo},
		{Kind: "Func", Name: "Page", FilePath: page},
		{Kind: "Func", Name: "load", FilePath: repo},
		{Kind: "Config", Name: "compilerOptions.paths.@/*", Body: "./src/*", FilePath: "web/tsconfig.json"},
	}
	imp := func(module, binding, imported string) parser.EdgeInfo {
		return parser.EdgeInfo{Kind: "imports", Source: page, Target: module,
			Extra: map[string]string{parser.ExtraScriptImport: "1", parser.ExtraBinding: binding, parser.ExtraImported: imported}}
	}
	call := func(name string) parser.EdgeInfo {
		return parser.EdgeInfo{Kind: "calls", Source: page + "::Page", Target: name, File: page,
			Extra: map[string]string{"callName": name, "bare": "1", parser.ExtraScriptCall: "1"}}
	}
	g := New()
	g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: []parser.EdgeInfo{
		imp("@/lib/repo", "load", "load"), imp("@tanstack/query", "useQuery", "useQuery"),
		call("load"), call("useQuery"),
	}})
	got := map[string]string{}
	for _, e := range g.Callees("r//"+page+"::Page", "calls") {
		got[e.Extra["callName"]] = e.Target
	}
	if got["load"] != "r//"+repo+"::load" || got["useQuery"] != "r//useQuery" {
		t.Fatalf("got %v", got)
	}
}
