package graph

import (
	"path"
	"strings"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// symbolTable indexes a repo's nodes for cross-file call resolution.
//
// Every rule in resolveCallTarget resolves where its EVIDENCE points and
// nowhere else: a bare call in the caller's own package, a pkg.Func() in the
// package that import names, a typed receiver in the package its type came
// from, a chained receiver by following declared field and result types. There
// is no repo-wide name matching. That used to exist as a last resort, and it
// drew a same-named method from a package the caller never imported into its
// blast radius: an unrelated package's test calling the k8s client's promoted
// Update() was linked to the engine package's Indexer.Update, the only
// Update declared in the repo.
type symbolTable struct {
	// byTypeMethod maps "Type.method" -> qualified names, one per package that
	// declares a type of that name with that method.
	byTypeMethod map[string][]string
	// byFunc maps a bare function name -> qualified names (package-level funcs,
	// any package).
	byFunc map[string][]string
	// byMethodName maps a bare method name -> qualified names (any receiver, any
	// package). Consulted only within a single package the receiver provably
	// came from (recvPkg with no type), never across packages.
	byMethodName map[string][]string
	// byClass maps a bare type name -> Class/Type node qns (any package), and
	// nodes keeps every code node by qn, for the chain rule: a struct's Fields
	// and a method's Result are the declared types a chain is followed on.
	byClass map[string][]string
	nodes   map[string]parser.NodeInfo

	// fileImports maps a file path -> the import paths it declares, from the
	// parser's imports edges. It is what lets a rule ask "can this file see that
	// package at all?"
	fileImports map[string]map[string]bool
	// pkgDirs is every directory that declares a code symbol — the repo's own
	// packages, the only ones an import path can resolve to.
	pkgDirs map[string]bool
	// files is every parsed file, pyFiles the Python ones: what a TypeScript or
	// Python import's module can resolve to. scriptImports maps a file to the
	// names its imports bind (see scriptlink.go).
	files         map[string]bool
	pyFiles       map[string]bool
	scriptImports map[string]map[string]scriptImport
	// tsConfigs maps a tsconfig/jsconfig's directory to its baseUrl and paths.
	tsConfigs map[string]*tsConfig

	// module is the repo's inferred Go module path (see inferModule); "" when
	// no intra-repo import was seen, in which case pkg.Func() never resolves.
	module string
}

// buildSymbolTable indexes one repo's nodes (the symbols) and edges (the
// imports, for visibility) for call resolution.
func buildSymbolTable(nodes []parser.NodeInfo, edges []parser.EdgeInfo, repo string) *symbolTable {
	st := &symbolTable{
		byTypeMethod:  map[string][]string{},
		byFunc:        map[string][]string{},
		byMethodName:  map[string][]string{},
		byClass:       map[string][]string{},
		nodes:         map[string]parser.NodeInfo{},
		fileImports:   map[string]map[string]bool{},
		pkgDirs:       map[string]bool{},
		files:         map[string]bool{},
		pyFiles:       map[string]bool{},
		scriptImports: map[string]map[string]scriptImport{},
		tsConfigs:     map[string]*tsConfig{},
	}
	for _, n := range nodes {
		switch n.Kind {
		case "Config":
			st.addTSConfig(n)
			continue
		case "File":
			st.files[n.FilePath] = true
			if strings.HasSuffix(n.FilePath, ".py") {
				st.pyFiles[n.FilePath] = true
			}
			continue
		case "Func", "Test":
		case "Class", "Type":
			st.pkgDirs[path.Dir(n.FilePath)] = true
			qn := repoQN(repo, n.Qualified())
			st.byClass[n.Name] = append(st.byClass[n.Name], qn)
			st.nodes[qn] = n
			continue
		default:
			continue
		}
		st.pkgDirs[path.Dir(n.FilePath)] = true
		qn := repoQN(repo, n.Qualified())
		st.nodes[qn] = n
		if n.ParentName != "" {
			key := n.ParentName + "." + n.Name
			st.byTypeMethod[key] = append(st.byTypeMethod[key], qn)
			st.byMethodName[n.Name] = append(st.byMethodName[n.Name], qn)
		} else {
			st.byFunc[n.Name] = append(st.byFunc[n.Name], qn)
		}
	}
	for _, e := range edges {
		if e.Kind != parser.EdgeImports {
			continue
		}
		if e.Extra[parser.ExtraScriptImport] != "" {
			// Kept apart from fileImports: a "./lib/engine" specifier must not
			// vote in inferModule for a Go module path.
			m := st.scriptImports[e.Source]
			if m == nil {
				m = map[string]scriptImport{}
				st.scriptImports[e.Source] = m
			}
			m[e.Extra[parser.ExtraBinding]] = scriptImport{module: e.Target, imported: e.Extra[parser.ExtraImported]}
			continue
		}
		m := st.fileImports[e.Source]
		if m == nil {
			m = map[string]bool{}
			st.fileImports[e.Source] = m
		}
		imp := e.Target
		if e.Extra["wildcard"] == "1" {
			imp += ".*"
		}
		m[imp] = true
	}
	st.module = st.inferModule()
	return st
}

// inferModule derives the repo's module path from its own imports: every
// intra-repo import is <module>/<dir> for one of the repo's package
// directories, so the prefix shared by the most such imports is the module
// path. Reading go.mod would be more direct, but the linker sees only nodes
// and edges — and snapshots built before this rule existed carry no module —
// so inferring keeps old snapshots resolving too. A third-party path that
// happens to end in one of our directory names ("…/lib/engine" against our
// "engine") yields a different prefix and is outvoted. The winner must be
// strict: a tie means the evidence cannot tell the module from a coincidence,
// so no module is inferred and pkg.Func() stays external — as it does when
// there is no intra-repo import at all.
func (st *symbolTable) inferModule() string {
	votes := map[string]int{}
	for _, imports := range st.fileImports {
		for imp := range imports {
			for dir := range st.pkgDirs {
				if dir == "." {
					continue
				}
				if strings.HasSuffix(imp, "/"+dir) {
					votes[strings.TrimSuffix(imp, "/"+dir)]++
				}
			}
		}
	}
	best, bestN, runnerUp := "", 0, 0
	for m, n := range votes {
		switch {
		case n > bestN:
			best, bestN, runnerUp = m, n, bestN
		case n > runnerUp:
			runnerUp = n
		}
	}
	if bestN == runnerUp {
		return ""
	}
	return best
}

// importDir maps an import path onto one of the repo's own package
// directories, or ok=false for stdlib, third-party and unknown paths.
func (st *symbolTable) importDir(imp string) (string, bool) {
	if st.module == "" {
		return "", false
	}
	if imp == st.module {
		return ".", st.pkgDirs["."]
	}
	if !strings.HasPrefix(imp, st.module+"/") {
		return "", false
	}
	dir := imp[len(st.module)+1:]
	return dir, st.pkgDirs[dir]
}

// fileOfQN returns the file part of a qualified name, with or without a repo
// namespace: "r//a/b.go::T.m" and "a/b.go::T.m" both give "a/b.go".
func fileOfQN(qn string) string {
	if i := strings.Index(qn, "::"); i >= 0 {
		qn = qn[:i]
	}
	if i := strings.Index(qn, "//"); i >= 0 {
		qn = qn[i+2:]
	}
	return qn
}

// uniqueIn returns the one candidate declared in any of dirs; ok=false when
// none is, or when several are (ambiguous — no edge beats a wrong edge).
func uniqueIn(cands []string, dirs map[string]bool) (string, bool) {
	found := ""
	for _, qn := range cands {
		if !dirs[path.Dir(fileOfQN(qn))] {
			continue
		}
		if found != "" {
			return "", false
		}
		found = qn
	}
	return found, found != ""
}

// goBuiltins are Go's predeclared functions — never real call-graph edges.
// Without this exclusion, bare-call resolution (rule 3 below) would treat them
// like any other unqualified name: since no user Go function is ever actually
// named e.g. "append", but some unrelated file in a multi-language repo may
// happen to declare a same-named symbol (a minified JS bundle's own "append"
// function, say), that one coincidence would "uniquely" resolve and every
// append() call site in the entire repo would falsely edge to it, turning an
// unrelated file into an accidental hub in every impact/risk/caller query.
var goBuiltins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
}

// Resolution rule names, reported in LinkStats.ResolvedByRule so a build can
// show WHICH evidence its edges rest on.
const (
	ruleJava       = "java"       // Java receiver of a declared type, found through imports/package
	ruleJavaType   = "javaType"   // Java call on a repo type that does not declare the method: the type
	ruleScript     = "script"     // TypeScript/Python call resolved through the file's own declarations or imports
	ruleScriptType = "scriptType" // TypeScript/Python call on a repo class that does not declare the method: the class
	rulePkg        = "pkg"        // pkg.Func() into one of the repo's own packages
	ruleRecvPkg    = "recvPkg"    // receiver whose type/value came from a known package
	ruleRecvType   = "recvType"   // receiver of a known, package-local type
	ruleChain      = "chain"      // receiver reached through declared field/result types
	ruleBare       = "bare"       // unqualified foo() in the caller's own package
)

// resolveCallTarget rewrites an unresolved calls-edge target to a qualified node
// QN using the edge hints, returning the rule that produced it, or ("", "")
// if the call should stay external.
//
// Every rule rests on declared evidence — a type, an import, a package of
// origin — and resolves only when exactly one symbol matches where that
// evidence points. There is no name-matching fallback: a receiver the parser
// could not type stays external, because "the only method of that name I can
// see" was measured against this repo and put req.Header.Set (net/http) on a
// local SecretStore.Set fifty-six times.
//
//  1. pkg.Func()               → Func in the repo package that import path
//     names; stdlib and third-party stay external
//  2. chained receiver         → walk x.field / x.method() / [] from the
//     root's binding (chain + rootType/Pkg) — or from a package-local
//     function's declared result (rootFunc) — over the declaring struct's
//     Fields and the method's Result, then Type.method where the walk ends;
//     any step without evidence, or into a foreign package → external
//  3. receiver origin known    → its method in the package the receiver came
//     (recvPkg)                  from — by exact Type.method when the type is
//     known, else the one method of that name in
//     that single package; a package outside the
//     repo is external by construction
//  4. receiver type known      → Type.method in the caller's OWN package: an
//     (recvType, unqualified)    unqualified type name is package-local in Go;
//     a known type with no such method is external
//     or promoted from an embedded field
//  5. bare foo()               → func foo in the caller's own package
func (st *symbolTable) resolveCallTarget(e parser.EdgeInfo) (qn, rule string) {
	callName := e.Extra["callName"]
	if callName == "" {
		return "", ""
	}

	// Go predeclared functions (append, len, make, …) are never real call-graph
	// edges — see goBuiltins.
	if goBuiltins[callName] {
		return "", ""
	}

	file := e.File
	if file == "" {
		file = fileOfQN(e.Source)
	}
	own := map[string]bool{path.Dir(file): true}
	rt := e.Extra["recvType"]

	// 0) TypeScript/Python: own declarations and import bindings.
	if e.Extra[parser.ExtraScriptCall] != "" {
		return st.resolveScriptCall(e, callName, file)
	}

	// 0) Java: the receiver's declared type, resolved as a type reference.
	if e.Extra[parser.ExtraJavaRecv] != "" {
		return st.resolveJavaCall(e, callName, file)
	}

	// 1) pkg.Func(): resolve only into the repo's own packages.
	if mod := e.Extra["pkg"]; mod != "" {
		dir, ok := st.importDir(mod)
		if !ok {
			return "", ""
		}
		return pick(rulePkg, st.byFunc[callName], map[string]bool{dir: true})
	}

	// 2) chained receiver: follow the declared types step by step.
	if e.Extra["chain"] != "" || e.Extra["rootFunc"] != "" {
		return st.resolveChain(e, callName, file)
	}

	// 3) receiver origin known: look only in that package, or nowhere.
	if rp := e.Extra["recvPkg"]; rp != "" {
		dir, ok := st.importDir(rp)
		if !ok {
			return "", ""
		}
		there := map[string]bool{dir: true}
		if rt != "" {
			return pick(ruleRecvPkg, st.byTypeMethod[rt+"."+callName], there)
		}
		return pick(ruleRecvPkg, st.byMethodName[callName], there)
	}

	// 4) receiver of a package-local type → exact method match, own package.
	if rt != "" {
		return pick(ruleRecvType, st.byTypeMethod[rt+"."+callName], own)
	}

	// 5) bare foo() → the caller's own package, only if unambiguous there.
	if e.Extra["bare"] == "1" {
		return pick(ruleBare, st.byFunc[callName], own)
	}

	// Receiver of unknown type and no evidence to follow: external.
	return "", ""
}

// resolveChain follows a chained receiver from its root — a bound variable's
// type, or a package-local function's declared result — over declared types:
// a field step reads the struct's Fields, a method() step reads the method's
// Result, "[]" and "[k]" take a collection's element or key. Each
// step must land on exactly one declaration in the package the previous step
// named; a foreign package ends the walk as external, since nothing there is
// a node.
func (st *symbolTable) resolveChain(e parser.EdgeInfo, callName, file string) (string, string) {
	only := func(d string) map[string]bool { return map[string]bool{d: true} }
	cur := parser.TypeRef{Name: e.Extra["rootType"], Pkg: e.Extra["rootPkg"]}
	dir := path.Dir(file)
	if rf := e.Extra["rootFunc"]; rf != "" {
		// Rooted in a package-local function: its declared result is the type.
		qn, ok := uniqueIn(st.byFunc[rf], only(dir))
		if !ok {
			return "", ""
		}
		cur = st.nodes[qn].Result
		if cur.Name == "" && cur.Key == nil {
			return "", ""
		}
		dir = path.Dir(fileOfQN(qn))
	}
	if cur.Pkg != "" {
		d, ok := st.importDir(cur.Pkg)
		if !ok {
			return "", ""
		}
		dir = d
	} else if cur.Name == "" && cur.Key == nil {
		return "", ""
	}
	var segs []string
	if chain := e.Extra["chain"]; chain != "" {
		segs = strings.Split(chain, ".")
	}
	for _, seg := range segs {
		var next parser.TypeRef
		declFile := ""
		switch m, isCall := strings.CutSuffix(seg, "()"); {
		case seg == "[]" || seg == "[k]":
			if cur.Coll == "" {
				return "", ""
			}
			if seg == "[k]" {
				if cur.Key == nil {
					return "", ""
				}
				next = *cur.Key
			} else {
				next = parser.TypeRef{Name: cur.Name, Pkg: cur.Pkg}
			}
		case isCall:
			if cur.Coll != "" {
				return "", ""
			}
			var qn string
			var ok bool
			if cur.Name != "" {
				qn, ok = uniqueIn(st.byTypeMethod[cur.Name+"."+m], only(dir))
			} else {
				qn, ok = uniqueIn(st.byMethodName[m], only(dir))
			}
			if !ok {
				return "", ""
			}
			next, declFile = st.nodes[qn].Result, fileOfQN(qn)
		default:
			if cur.Coll != "" || cur.Name == "" {
				return "", ""
			}
			qn, ok := uniqueIn(st.byClass[cur.Name], only(dir))
			if !ok {
				return "", ""
			}
			next, declFile = st.nodes[qn].Fields[seg], fileOfQN(qn)
		}
		if next.Name == "" && next.Key == nil {
			return "", ""
		}
		if next.Pkg != "" {
			d, ok := st.importDir(next.Pkg)
			if !ok {
				return "", ""
			}
			dir = d
		} else if declFile != "" {
			dir = path.Dir(declFile)
		}
		cur = next
	}
	if cur.Coll != "" || cur.Name == "" {
		return "", ""
	}
	return pick(ruleChain, st.byTypeMethod[cur.Name+"."+callName], only(dir))
}

// pick returns the one candidate declared in any of dirs, tagged with the rule
// that asked; ("", "") when none is, or when several are — ambiguous, and no
// edge beats a wrong edge.
func pick(rule string, cands []string, dirs map[string]bool) (qn, matched string) {
	qn, ok := uniqueIn(cands, dirs)
	if !ok {
		return "", ""
	}
	return qn, rule
}
