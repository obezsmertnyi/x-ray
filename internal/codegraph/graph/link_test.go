package graph

import (
	"testing"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// edge builds a calls EdgeInfo with the given hints.
func callEdge(src, provisional string, extra map[string]string) parser.EdgeInfo {
	return parser.EdgeInfo{Kind: "calls", Source: src, Target: provisional, Extra: extra}
}

func TestBuild_ResolvesReceiverMethodCall(t *testing.T) {
	// voice.go declares VoiceSession.handleToolCall and a method that calls
	// s.handleToolCall(); the linker should connect them.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "handleToolCall", FilePath: "voice.go", ParentName: "VoiceSession"},
		{Kind: "Func", Name: "handleGeminiMessage", FilePath: "voice.go", ParentName: "VoiceSession"},
	}
	edges := []parser.EdgeInfo{
		callEdge("voice.go::VoiceSession.handleGeminiMessage", "VoiceSession.handleToolCall",
			map[string]string{"callName": "handleToolCall", "recvType": "VoiceSession"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedCalls != 1 {
		t.Fatalf("expected 1 resolved call, got %d (external=%d)", ls.ResolvedCalls, ls.ExternalCalls)
	}
	callers := g.Callers("r//voice.go::VoiceSession.handleToolCall", "calls")
	if len(callers) != 1 {
		t.Fatalf("expected 1 caller of handleToolCall, got %d", len(callers))
	}
	if got := callers[0].Source; got != "r//voice.go::VoiceSession.handleGeminiMessage" {
		t.Fatalf("unexpected caller source: %s", got)
	}
}

func TestBuild_ResolvesBareFuncCallAcrossFiles(t *testing.T) {
	// a.go calls newThing(); b.go defines func newThing(). Cross-file resolution.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "doWork", FilePath: "a.go"},
		{Kind: "Func", Name: "newThing", FilePath: "b.go"},
	}
	edges := []parser.EdgeInfo{
		callEdge("a.go::doWork", "newThing",
			map[string]string{"callName": "newThing", "bare": "1"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedCalls != 1 {
		t.Fatalf("expected 1 resolved call, got %d", ls.ResolvedCalls)
	}
	callees := g.Callees("r//a.go::doWork", "calls")
	if len(callees) != 1 || callees[0].Target != "r//b.go::newThing" {
		t.Fatalf("expected doWork -> b.go::newThing, got %+v", callees)
	}
}

func TestBuild_AmbiguousBareFuncStaysExternal(t *testing.T) {
	// Two funcs named handle() in different files: a bare handle() call is
	// ambiguous and must NOT be resolved (avoid wrong edges).
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "caller", FilePath: "a.go"},
		{Kind: "Func", Name: "handle", FilePath: "a.go"},
		{Kind: "Func", Name: "handle", FilePath: "b.go"},
	}
	edges := []parser.EdgeInfo{
		callEdge("a.go::caller", "handle",
			map[string]string{"callName": "handle", "bare": "1"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedCalls != 0 || ls.ExternalCalls != 1 {
		t.Fatalf("ambiguous bare call should stay external: resolved=%d external=%d", ls.ResolvedCalls, ls.ExternalCalls)
	}
}

func TestBuild_PkgCallStaysExternal(t *testing.T) {
	// fmt.Errorf() must never resolve to a local node.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "caller", FilePath: "a.go"},
		{Kind: "Func", Name: "Errorf", FilePath: "a.go"}, // decoy local with same name
	}
	edges := []parser.EdgeInfo{
		callEdge("a.go::caller", "fmt.Errorf",
			map[string]string{"callName": "Errorf", "pkg": "fmt"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedCalls != 0 || ls.ExternalCalls != 1 {
		t.Fatalf("pkg call must stay external: resolved=%d external=%d", ls.ResolvedCalls, ls.ExternalCalls)
	}
}

func TestBuild_GoBuiltinCallStaysExternal(t *testing.T) {
	// append() is a Go builtin, not a user function. A same-named symbol
	// elsewhere in the repo (e.g. a JS file's own "append") must not absorb
	// every append() call site in the codebase as a false edge.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "caller", FilePath: "a.go"},
		{Kind: "Func", Name: "append", FilePath: "vendor.js"}, // decoy, same bare name
	}
	edges := []parser.EdgeInfo{
		callEdge("a.go::caller", "append", map[string]string{"callName": "append", "bare": "1"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedCalls != 0 || ls.ExternalCalls != 1 {
		t.Fatalf("append() builtin must stay external: resolved=%d external=%d", ls.ResolvedCalls, ls.ExternalCalls)
	}
	callers := g.Callers("r//vendor.js::append", "calls")
	if len(callers) != 0 {
		t.Fatalf("vendor.js's append must not gain a false caller edge from the builtin call, got %+v", callers)
	}
}

func TestImpactRadius_DoesNotBleedThroughUnresolvedHub(t *testing.T) {
	// Two unrelated functions both call something named Get() that doesn't
	// resolve to a node. The unresolved "Get" target must NOT become a hub that
	// connects them in the blast radius.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "alpha", FilePath: "a.go"},
		{Kind: "Func", Name: "beta", FilePath: "b.go"},
	}
	edges := []parser.EdgeInfo{
		callEdge("a.go::alpha", "Get", map[string]string{"callName": "Get"}),
		callEdge("b.go::beta", "Get", map[string]string{"callName": "Get"}),
	}
	g := New()
	g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	impacted := g.ImpactRadius([]string{"r//a.go::alpha"}, 3, 100)
	for _, im := range impacted {
		if im.QN == "r//b.go::beta" {
			t.Fatalf("beta must NOT be in alpha's impact radius (bled through unresolved Get hub)")
		}
		if im.QN == "r//Get" {
			t.Fatalf("unresolved target Get must not appear as an impacted node")
		}
	}
}

func TestImpactRadius_DoesNotBleedThroughFileContainsHub(t *testing.T) {
	// alpha and unrelated are both declared in a.go (linked to it only via
	// "contains") but never call each other. The file must not become a hub
	// that pulls every sibling symbol into alpha's blast radius.
	nodes := []parser.NodeInfo{
		{Kind: "File", Name: "a.go", FilePath: "a.go"},
		{Kind: "Func", Name: "alpha", FilePath: "a.go"},
		{Kind: "Func", Name: "unrelated", FilePath: "a.go"},
	}
	edges := []parser.EdgeInfo{
		{Kind: parser.EdgeContains, Source: "a.go", Target: "a.go::alpha"},
		{Kind: parser.EdgeContains, Source: "a.go", Target: "a.go::unrelated"},
	}
	g := New()
	g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	impacted := g.ImpactRadius([]string{"r//a.go::alpha"}, 2, 100)
	for _, im := range impacted {
		if im.QN == "r//a.go::unrelated" {
			t.Fatalf("unrelated must NOT be in alpha's impact radius (bled through the file's contains hub)")
		}
		if im.QN == "r//a.go" {
			t.Fatalf("the File node itself must not appear in the impact radius (contains is structural, not a dependency edge)")
		}
	}
}

func TestBuild_UnknownReceiverStaysExternal(t *testing.T) {
	// x.persist() where x's type is unknown, and only one persist() method
	// exists in the repo. This USED to resolve as a last resort — "the only
	// method of that name" — and that rule is gone: measured on this repo it
	// put req.Header.Set (net/http) on a local SecretStore.Set 56 times. No
	// evidence, no edge.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "caller", FilePath: "a.go"},
		{Kind: "Func", Name: "persist", FilePath: "store.go", ParentName: "Store"},
	}
	edges := []parser.EdgeInfo{
		callEdge("a.go::caller", "x.persist",
			map[string]string{"callName": "persist", "recvVar": "x"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedCalls != 0 || ls.ExternalCalls != 1 {
		t.Fatalf("untyped receiver must stay external, got resolved=%d external=%d", ls.ResolvedCalls, ls.ExternalCalls)
	}
}

// --- evidence scoping -------------------------------------------------------
//
// Every rule resolves where its evidence points: the caller's own package, the
// package an import names, the package a receiver's type came from, or the
// declared types along a chain. The tests below are the shapes that motivated
// it, each named after the file that exhibited it.

// importEdge builds the imports edge the Go extractor emits for one import.
func importEdge(file, path string) parser.EdgeInfo {
	return parser.EdgeInfo{Kind: parser.EdgeImports, Source: file, Target: path, File: file}
}

func TestBuild_UnknownReceiverInUnimportedPackageStaysExternal(t *testing.T) {
	// internal/xray/cellsync_test.go calls syncer.Update(ctx, cell) on a
	// *CellSyncer whose Update is promoted from the embedded k8s client — not
	// declared anywhere in this repo. The ONLY Update declared is
	// internal/recall/engine's Indexer.Update, and internal/xray never imports
	// engine. Before visibility scoping the linker matched on the bare name
	// across the whole repo and put three cellsync tests into every blast
	// radius that reached Indexer.Update.
	nodes := []parser.NodeInfo{
		{Kind: "Test", Name: "TestPersist", FilePath: "internal/xray/cellsync_test.go"},
		{Kind: "Func", Name: "Update", FilePath: "internal/recall/engine/update.go", ParentName: "Indexer"},
		{Kind: "Func", Name: "New", FilePath: "internal/recall/parser/walk.go"},
	}
	edges := []parser.EdgeInfo{
		importEdge("internal/xray/cellsync_test.go", "sigs.k8s.io/controller-runtime/pkg/client"),
		importEdge("internal/recall/engine/update.go", "github.com/x/repo/internal/recall/parser"),
		callEdge("internal/xray/cellsync_test.go::TestPersist", "syncer.Update",
			map[string]string{"callName": "Update", "recvVar": "syncer"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedCalls != 0 || ls.ExternalCalls != 1 {
		t.Fatalf("a method in a package the caller does not import must stay external: resolved=%d external=%d", ls.ResolvedCalls, ls.ExternalCalls)
	}
	if callers := g.Callers("r//internal/recall/engine/update.go::Indexer.Update", "calls"); len(callers) != 0 {
		t.Fatalf("Indexer.Update must not gain a caller from internal/xray, got %+v", callers)
	}
}

func TestBuild_UnknownReceiverUniqueInImportedPackageStillExternal(t *testing.T) {
	// cmd/recall/serve.go imports engine and calls ixr.Update(...) on a
	// value the walk could not bind. Update is unique among the packages that
	// file imports — and that is still not evidence. The same call WITH its
	// origin (ixr := engine.NewIndexer(...)) resolves: see
	// TestBuild_ReceiverFromRepoPackageResolvesThere.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "registerAdminEndpoints", FilePath: "cmd/recall/serve.go"},
		{Kind: "Func", Name: "Update", FilePath: "internal/recall/engine/update.go", ParentName: "Indexer"},
	}
	edges := []parser.EdgeInfo{
		importEdge("cmd/recall/serve.go", "github.com/x/repo/internal/recall/engine"),
		callEdge("cmd/recall/serve.go::registerAdminEndpoints", "ixr.Update",
			map[string]string{"callName": "Update", "recvVar": "ixr"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedCalls != 0 {
		t.Fatalf("an untyped receiver must not resolve by name uniqueness, got resolved=%d", ls.ResolvedCalls)
	}
}

func TestBuild_ChainFollowsDeclaredFieldTypes(t *testing.T) {
	// s.memory.Upsert(...) with s *Server and Server{memory *MemoryStore}: the
	// field's declared type names the receiver, so Upsert resolves to
	// MemoryStore.Upsert in the same package — by evidence, not by name. A
	// decoy Upsert on another local type shows the exact match matters.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "handle", FilePath: "internal/xray/server.go", ParentName: "Server"},
		{Kind: "Class", Name: "Server", FilePath: "internal/xray/server.go",
			Fields: map[string]parser.TypeRef{"memory": {Name: "MemoryStore"}}},
		{Kind: "Func", Name: "Upsert", FilePath: "internal/xray/memory.go", ParentName: "MemoryStore"},
		{Kind: "Func", Name: "Upsert", FilePath: "internal/xray/cache.go", ParentName: "Cache"}, // decoy
	}
	edges := []parser.EdgeInfo{
		callEdge("internal/xray/server.go::Server.handle", "s.memory.Upsert",
			map[string]string{"callName": "Upsert", "recvVar": "s.memory", "chain": "memory", "rootType": "Server"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedByRule[ruleChain] != 1 {
		t.Fatalf("field chain should resolve by the chain rule, byRule=%v", ls.ResolvedByRule)
	}
	callees := g.Callees("r//internal/xray/server.go::Server.handle", "calls")
	if len(callees) != 1 || callees[0].Target != "r//internal/xray/memory.go::MemoryStore.Upsert" {
		t.Fatalf("expected handle -> MemoryStore.Upsert, got %+v", callees)
	}
}

func TestBuild_ChainFollowsMethodResultsAcrossPackages(t *testing.T) {
	// r.Index().Search(...) where Resolved.Index() returns *index.Index from
	// another package: the method's declared result names both the type and
	// the package, so the walk crosses into internal/recall/index on evidence.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "run", FilePath: "internal/recall/mcpserver/readonly.go"},
		{Kind: "Class", Name: "Resolved", FilePath: "internal/recall/mcpserver/mcpserver.go"},
		{Kind: "Func", Name: "Index", FilePath: "internal/recall/mcpserver/mcpserver.go", ParentName: "Resolved",
			Result: parser.TypeRef{Name: "Index", Pkg: "github.com/x/repo/internal/recall/index"}},
		{Kind: "Class", Name: "Index", FilePath: "internal/recall/index/index.go"},
		{Kind: "Func", Name: "Search", FilePath: "internal/recall/index/index.go", ParentName: "Index"},
		{Kind: "Func", Name: "Search", FilePath: "internal/recall/mcpserver/local.go", ParentName: "Local"}, // decoy, own package
	}
	edges := []parser.EdgeInfo{
		importEdge("internal/recall/mcpserver/mcpserver.go", "github.com/x/repo/internal/recall/index"),
		callEdge("internal/recall/mcpserver/readonly.go::run", "r.Index().Search",
			map[string]string{"callName": "Search", "recvVar": "r.Index()", "chain": "Index()", "rootType": "Resolved"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedByRule[ruleChain] != 1 {
		t.Fatalf("result chain should resolve by the chain rule, byRule=%v", ls.ResolvedByRule)
	}
	callees := g.Callees("r//internal/recall/mcpserver/readonly.go::run", "calls")
	if len(callees) != 1 || callees[0].Target != "r//internal/recall/index/index.go::Index.Search" {
		t.Fatalf("expected run -> index.Index.Search, got %+v", callees)
	}
}

func TestBuild_ChainStopsWithoutEvidence(t *testing.T) {
	// Four chains that must NOT resolve, each with a same-named local decoy:
	// a field of interface type (its methods are not nodes), a field promoted
	// from an embedded foreign type, a method with no single named result, and
	// a root of foreign origin (req.Header.Set with req from net/http — the
	// 56-edge defect).
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "f", FilePath: "srv/a.go"},
		{Kind: "Class", Name: "Deps", FilePath: "srv/deps.go",
			Fields: map[string]parser.TypeRef{"Resolver": {Name: "Resolver"}}},
		{Kind: "Class", Name: "Resolver", FilePath: "srv/deps.go"}, // interface: no Fields, no method nodes
		{Kind: "Func", Name: "UpdateByPaths", FilePath: "srv/registry.go", ParentName: "Registry"},
		{Kind: "Class", Name: "Syncer", FilePath: "srv/sync.go",
			Fields: map[string]parser.TypeRef{"Client": {Name: "Client", Pkg: "sigs.k8s.io/controller-runtime/pkg/client"}}},
		{Kind: "Func", Name: "Status", FilePath: "srv/sync.go", ParentName: "Syncer"}, // returns an interface value: no Result
		{Kind: "Func", Name: "Update", FilePath: "srv/indexer.go", ParentName: "Indexer"},
		{Kind: "Func", Name: "Set", FilePath: "srv/secrets.go", ParentName: "SecretStore"},
	}
	edges := []parser.EdgeInfo{
		callEdge("srv/a.go::f", "d.Resolver.UpdateByPaths",
			map[string]string{"callName": "UpdateByPaths", "recvVar": "d.Resolver", "chain": "Resolver", "rootType": "Deps"}),
		callEdge("srv/a.go::f", "s.Client.Update",
			map[string]string{"callName": "Update", "recvVar": "s.Client", "chain": "Client", "rootType": "Syncer"}),
		callEdge("srv/a.go::f", "s.Status().Update",
			map[string]string{"callName": "Update", "recvVar": "s.Status()", "chain": "Status()", "rootType": "Syncer"}),
		callEdge("srv/a.go::f", "req.Header.Set",
			map[string]string{"callName": "Set", "recvVar": "req.Header", "chain": "Header", "rootPkg": "net/http"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedCalls != 0 || ls.ExternalCalls != 4 {
		t.Fatalf("chains without evidence must stay external: resolved=%d external=%d byRule=%v", ls.ResolvedCalls, ls.ExternalCalls, ls.ResolvedByRule)
	}
	for _, qn := range []string{"r//srv/registry.go::Registry.UpdateByPaths", "r//srv/indexer.go::Indexer.Update", "r//srv/secrets.go::SecretStore.Set"} {
		if callers := g.Callers(qn, "calls"); len(callers) != 0 {
			t.Fatalf("%s must not gain a caller from an evidence-free chain, got %+v", qn, callers)
		}
	}
}

func TestBuild_BareFuncNeverCrossesPackage(t *testing.T) {
	// An unqualified helper() is package-local in Go. cmd/tool declares no
	// helper, so the call stays external even though internal/util's helper is
	// the only one in the repo — and internal/util must not gain a false caller.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "main", FilePath: "cmd/tool/main.go"},
		{Kind: "Func", Name: "helper", FilePath: "internal/util/util.go"},
	}
	edges := []parser.EdgeInfo{
		callEdge("cmd/tool/main.go::main", "helper", map[string]string{"callName": "helper", "bare": "1"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	if ls.ResolvedCalls != 0 {
		t.Fatalf("bare call must not resolve into another package, got resolved=%d", ls.ResolvedCalls)
	}

	// The flip side: a helper in the caller's own package wins outright even
	// when another package declares one too — repo-wide, that used to be
	// "ambiguous → external".
	nodes = append(nodes, parser.NodeInfo{Kind: "Func", Name: "helper", FilePath: "cmd/tool/util.go"})
	g = New()
	ls = g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	callees := g.Callees("r//cmd/tool/main.go::main", "calls")
	if ls.ResolvedCalls != 1 || len(callees) != 1 || callees[0].Target != "r//cmd/tool/util.go::helper" {
		t.Fatalf("bare call should resolve to the own-package helper, got resolved=%d callees=%+v", ls.ResolvedCalls, callees)
	}
}

func TestBuild_RepoLocalPkgCallResolves(t *testing.T) {
	// engine.NewRegistry() from cmd/xray/main.go: pkg.Func() used to be
	// "external by construction" even when pkg is one of the repo's own
	// packages, which left every cross-package call in the repo unresolved. The
	// module path is inferred from the repo's own imports (no go.mod in sight).
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "main", FilePath: "cmd/xray/main.go"},
		{Kind: "Func", Name: "NewRegistry", FilePath: "internal/recall/engine/registry.go"},
		{Kind: "Func", Name: "New", FilePath: "internal/recall/parser/walk.go"},
		{Kind: "Func", Name: "Errorf", FilePath: "cmd/xray/decoy.go"}, // same name as fmt.Errorf, own package
	}
	edges := []parser.EdgeInfo{
		importEdge("cmd/xray/main.go", "github.com/x/repo/internal/recall/engine"),
		importEdge("cmd/xray/main.go", "github.com/x/repo/internal/recall/parser"),
		importEdge("cmd/xray/main.go", "fmt"),
		callEdge("cmd/xray/main.go::main", "github.com/x/repo/internal/recall/engine.NewRegistry",
			map[string]string{"callName": "NewRegistry", "pkg": "github.com/x/repo/internal/recall/engine"}),
		callEdge("cmd/xray/main.go::main", "fmt.Errorf",
			map[string]string{"callName": "Errorf", "pkg": "fmt"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedCalls != 1 || ls.ExternalCalls != 1 {
		t.Fatalf("expected the intra-repo pkg call to resolve and fmt.Errorf to stay external: resolved=%d external=%d", ls.ResolvedCalls, ls.ExternalCalls)
	}
	callees := g.Callees("r//cmd/xray/main.go::main", "calls")
	var targets []string
	for _, e := range callees {
		targets = append(targets, e.Target)
	}
	if len(targets) != 2 || targets[0] != "r//internal/recall/engine/registry.go::NewRegistry" || targets[1] != "r//fmt.Errorf" {
		t.Fatalf("unexpected callees: %v", targets)
	}
}

func TestBuild_ModuleInferenceNeedsAClearWinner(t *testing.T) {
	// One intra-repo import and one third-party path that happens to end in
	// the same directory name tie at a vote each: the evidence cannot tell the
	// module from the coincidence, so nothing resolves through pkg.Func().
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "main", FilePath: "cmd/tool/main.go"},
		{Kind: "Func", Name: "Run", FilePath: "engine/run.go"},
	}
	edges := []parser.EdgeInfo{
		importEdge("cmd/tool/main.go", "github.com/x/repo/engine"),
		importEdge("cmd/tool/main.go", "github.com/other/lib/engine"),
		callEdge("cmd/tool/main.go::main", "github.com/other/lib/engine.Run",
			map[string]string{"callName": "Run", "pkg": "github.com/other/lib/engine"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	if ls.ResolvedCalls != 0 {
		t.Fatalf("a tied module vote must resolve nothing, got resolved=%d", ls.ResolvedCalls)
	}

	// A second intra-repo import breaks the tie, and the third-party call
	// still stays external: only the winning prefix resolves.
	nodes = append(nodes, parser.NodeInfo{Kind: "Func", Name: "Load", FilePath: "store/store.go"})
	edges = append(edges, importEdge("cmd/tool/main.go", "github.com/x/repo/store"))
	g = New()
	ls = g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	if ls.ResolvedCalls != 0 {
		t.Fatalf("third-party engine.Run must stay external once the module is known, got resolved=%d", ls.ResolvedCalls)
	}
}

func TestBuild_ReceiverFromExternalPackageStaysExternal(t *testing.T) {
	// fs := flag.NewFlagSet(...); fs.String(...) in cmd/recall/serve.go.
	// The same directory declares summaryFlag.String — the only String among
	// the packages that file can see. Without the receiver's origin the
	// last-resort rule would take that lone visible String as the callee; with
	// it, a value from a package outside the repo is external, full stop.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "runServe", FilePath: "cmd/recall/serve.go"},
		{Kind: "Func", Name: "String", FilePath: "cmd/recall/snapshot.go", ParentName: "summaryFlag"},
	}
	edges := []parser.EdgeInfo{
		importEdge("cmd/recall/serve.go", "flag"),
		callEdge("cmd/recall/serve.go::runServe", "fs.String",
			map[string]string{"callName": "String", "recvVar": "fs", "recvPkg": "flag"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedCalls != 0 {
		t.Fatalf("a receiver from outside the repo must stay external, got resolved=%d", ls.ResolvedCalls)
	}
	if callers := g.Callers("r//cmd/recall/snapshot.go::summaryFlag.String", "calls"); len(callers) != 0 {
		t.Fatalf("summaryFlag.String must not gain flag's callers, got %+v", callers)
	}
}

func TestBuild_ReceiverFromRepoPackageResolvesThere(t *testing.T) {
	// ixr := engine.NewIndexer(...); ixr.Update(...) — the type is unknown but
	// the value came from engine, so Update is looked up in engine and nowhere
	// else. And reg *engine.Registry (typed AND qualified) resolves by exact
	// method in that package.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "runServe", FilePath: "cmd/recall/serve.go"},
		{Kind: "Func", Name: "Update", FilePath: "internal/recall/engine/update.go", ParentName: "Indexer"},
		{Kind: "Func", Name: "UpdateByPaths", FilePath: "internal/recall/engine/update.go", ParentName: "Registry"},
		{Kind: "Func", Name: "Update", FilePath: "cmd/xray/decoy.go", ParentName: "Local"}, // own-package decoy
	}
	edges := []parser.EdgeInfo{
		importEdge("cmd/recall/serve.go", "github.com/x/repo/internal/recall/engine"),
		callEdge("cmd/recall/serve.go::runServe", "ixr.Update",
			map[string]string{"callName": "Update", "recvVar": "ixr", "recvPkg": "github.com/x/repo/internal/recall/engine"}),
		callEdge("cmd/recall/serve.go::runServe", "github.com/x/repo/internal/recall/engine.Registry.UpdateByPaths",
			map[string]string{"callName": "UpdateByPaths", "recvType": "Registry", "recvPkg": "github.com/x/repo/internal/recall/engine"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedCalls != 2 || ls.ResolvedByRule[ruleRecvPkg] != 2 {
		t.Fatalf("both origin-qualified calls should resolve by recvPkg: resolved=%d byRule=%v", ls.ResolvedCalls, ls.ResolvedByRule)
	}
	var targets []string
	for _, e := range g.Callees("r//cmd/recall/serve.go::runServe", "calls") {
		targets = append(targets, e.Target)
	}
	want := []string{
		"r//internal/recall/engine/update.go::Indexer.Update",
		"r//internal/recall/engine/update.go::Registry.UpdateByPaths",
	}
	if len(targets) != 2 || targets[0] != want[0] || targets[1] != want[1] {
		t.Fatalf("callees = %v, want %v", targets, want)
	}
}

func TestBuild_LocalTypeNeverMatchesAnotherPackage(t *testing.T) {
	// s *Server in cmd/api: an unqualified type name is package-local, so
	// s.Start() cannot be internal/httpd's Server.Start even though cmd/api
	// imports httpd and declares no Server.Start of its own.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "main", FilePath: "cmd/api/main.go"},
		{Kind: "Func", Name: "Start", FilePath: "internal/httpd/server.go", ParentName: "Server"},
	}
	edges := []parser.EdgeInfo{
		importEdge("cmd/api/main.go", "github.com/x/repo/internal/httpd"),
		callEdge("cmd/api/main.go::main", "Server.Start",
			map[string]string{"callName": "Start", "recvType": "Server"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	if ls.ResolvedCalls != 0 {
		t.Fatalf("unqualified local type must not resolve into an imported package, got resolved=%d", ls.ResolvedCalls)
	}

	// Declared in the same package, it resolves — by the typed rule.
	nodes = append(nodes, parser.NodeInfo{Kind: "Func", Name: "Start", FilePath: "cmd/api/server.go", ParentName: "Server"})
	g = New()
	ls = g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})
	if ls.ResolvedByRule[ruleRecvType] != 1 {
		t.Fatalf("own-package Server.Start should resolve by recvType, byRule=%v", ls.ResolvedByRule)
	}
}

func TestBuild_ChainTakesCollectionElementsAndKeys(t *testing.T) {
	// for _, n := range g.Nodes() { n.Touch() } — Nodes() is declared to return
	// []*Node, so the range value is a Node and Touch resolves to Node.Touch.
	// for k := range r.byLabel { k.Label() } — a map[*Entry]*Runner field: the
	// key is an Entry. And a method step on the collection itself, or an
	// element step on a plain type, is no evidence and stays external.
	nodes := []parser.NodeInfo{
		{Kind: "Func", Name: "walk", FilePath: "g/walk.go"},
		{Kind: "Class", Name: "Graph", FilePath: "g/graph.go"},
		{Kind: "Func", Name: "Nodes", FilePath: "g/graph.go", ParentName: "Graph",
			Result: parser.TypeRef{Name: "Node", Coll: "slice"}},
		{Kind: "Class", Name: "Node", FilePath: "g/node.go"},
		{Kind: "Func", Name: "Touch", FilePath: "g/node.go", ParentName: "Node"},
		{Kind: "Class", Name: "Registry", FilePath: "g/registry.go",
			Fields: map[string]parser.TypeRef{"byLabel": {Name: "Runner", Coll: "map", Key: &parser.TypeRef{Name: "Entry"}}}},
		{Kind: "Class", Name: "Entry", FilePath: "g/entry.go"},
		{Kind: "Func", Name: "Label", FilePath: "g/entry.go", ParentName: "Entry"},
		{Kind: "Func", Name: "Label", FilePath: "g/runner.go", ParentName: "Runner"}, // decoy: the VALUE type
	}
	edges := []parser.EdgeInfo{
		callEdge("g/walk.go::walk", "n.Touch",
			map[string]string{"callName": "Touch", "recvVar": "n", "chain": "Nodes().[]", "rootType": "Graph"}),
		callEdge("g/walk.go::walk", "k.Label",
			map[string]string{"callName": "Label", "recvVar": "k", "chain": "byLabel.[k]", "rootType": "Registry"}),
		callEdge("g/walk.go::walk", "g.Nodes().Touch", // a slice has no methods
			map[string]string{"callName": "Touch", "recvVar": "g.Nodes()", "chain": "Nodes()", "rootType": "Graph"}),
		callEdge("g/walk.go::walk", "n.Touch", // an element step on a plain Node
			map[string]string{"callName": "Touch", "recvVar": "n", "chain": "Nodes().[].[]", "rootType": "Graph"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedByRule[ruleChain] != 2 || ls.ExternalCalls != 2 {
		t.Fatalf("expected the two collection chains to resolve and the two malformed ones to stay external: byRule=%v external=%d", ls.ResolvedByRule, ls.ExternalCalls)
	}
	var targets []string
	for _, e := range g.Callees("r//g/walk.go::walk", "calls") {
		if _, ok := g.Get(e.Target); ok {
			targets = append(targets, e.Target)
		}
	}
	want := []string{"r//g/node.go::Node.Touch", "r//g/entry.go::Entry.Label"}
	if len(targets) != 2 || targets[0] != want[0] || targets[1] != want[1] {
		t.Fatalf("resolved callees = %v, want %v", targets, want)
	}
}

func TestBuild_ChainRootedInLocalFunctionResult(t *testing.T) {
	// reg := registryWithRepo(t); reg.UpdateByPaths(...) — the test helper is
	// not a New* constructor, but its declared result (*Registry) types reg.
	// A same-named helper in another package is not the root (a bare call is
	// package-local), and a helper with no single result gives nothing.
	nodes := []parser.NodeInfo{
		{Kind: "Test", Name: "TestUpdate", FilePath: "engine/update_paths_test.go"},
		{Kind: "Func", Name: "registryWithRepo", FilePath: "engine/update_paths_test.go",
			Result: parser.TypeRef{Name: "Registry"}},
		{Kind: "Func", Name: "UpdateByPaths", FilePath: "engine/update.go", ParentName: "Registry"},
		{Kind: "Func", Name: "registryWithRepo", FilePath: "other/helpers.go", Result: parser.TypeRef{Name: "Registry"}}, // other package
		{Kind: "Func", Name: "UpdateByPaths", FilePath: "other/registry.go", ParentName: "Registry"},                     // decoy
		{Kind: "Func", Name: "pair", FilePath: "engine/update_paths_test.go"},                                            // no Result
		{Kind: "Class", Name: "Registry", FilePath: "engine/registry.go",
			Fields: map[string]parser.TypeRef{"ixrs": {Name: "Indexer", Coll: "slice"}}},
		{Kind: "Func", Name: "Update", FilePath: "engine/update.go", ParentName: "Indexer"},
	}
	edges := []parser.EdgeInfo{
		callEdge("engine/update_paths_test.go::TestUpdate", "reg.UpdateByPaths",
			map[string]string{"callName": "UpdateByPaths", "recvVar": "reg", "rootFunc": "registryWithRepo"}),
		callEdge("engine/update_paths_test.go::TestUpdate", "p.UpdateByPaths",
			map[string]string{"callName": "UpdateByPaths", "recvVar": "p", "rootFunc": "pair"}),
		callEdge("engine/update_paths_test.go::TestUpdate", "ixr.Update", // for _, ixr := range reg.ixrs
			map[string]string{"callName": "Update", "recvVar": "ixr", "rootFunc": "registryWithRepo", "chain": "ixrs.[]"}),
	}
	g := New()
	ls := g.Build(RepoGraph{Repo: "r", Nodes: nodes, Edges: edges})

	if ls.ResolvedByRule[ruleChain] != 2 || ls.ExternalCalls != 1 {
		t.Fatalf("byRule=%v external=%d", ls.ResolvedByRule, ls.ExternalCalls)
	}
	var targets []string
	for _, e := range g.Callees("r//engine/update_paths_test.go::TestUpdate", "calls") {
		if _, ok := g.Get(e.Target); ok {
			targets = append(targets, e.Target)
		}
	}
	want := []string{"r//engine/update.go::Registry.UpdateByPaths", "r//engine/update.go::Indexer.Update"}
	if len(targets) != 2 || targets[0] != want[0] || targets[1] != want[1] {
		t.Fatalf("resolved callees = %v, want %v", targets, want)
	}
}
