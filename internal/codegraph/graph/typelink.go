package graph

import (
	"path"
	"strings"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// resolveTypeRef resolves a declared type name (a references edge carrying
// Extra typeName: a Java field, parameter or local type, a superclass, a new
// T()) where the file can see it, which in Java is where it says:
//
//  1. an import naming it exactly (import a.b.T) → T declared in a directory
//     ending a/b
//  2. its own package → T declared in the file's own directory
//  3. a wildcard import (import a.b.*) → T declared in a directory ending a/b
//
// A package can be declared in several modules of one repo — several
// services each hold the same fully qualified class — and only one of them is on the importing module's classpath. Where an
// import matches several, the declaration sharing the longest directory prefix
// with the importing file is taken when exactly one does: its own module. Any
// other tie stays unresolved, as calls do: no edge beats a wrong edge.
//
// Why it is a linker rule and not a service-map rule: "which class does this
// field hold" is what get_graph_impact needs for Java too, and the service map
// reads the resolved edge like any other.
func (st *symbolTable) resolveTypeRef(e parser.EdgeInfo) (string, bool) {
	name := e.Extra["typeName"]
	cands := st.byClass[name]
	if len(cands) == 0 {
		return "", false
	}
	file := e.File
	if file == "" {
		file = fileOfQN(e.Source)
	}
	var wildcards []string
	for imp := range st.fileImports[file] {
		if pkg, ok := strings.CutSuffix(imp, ".*"); ok {
			wildcards = append(wildcards, pkg)
			continue
		}
		if pkg, last, ok := cutLastDot(imp); ok && last == name {
			return nearest(inPackage(cands, pkg), file)
		}
	}
	if qn, ok := uniqueIn(cands, map[string]bool{path.Dir(file): true}); ok {
		return qn, true
	}
	var hits []string
	for _, pkg := range wildcards {
		hits = append(hits, inPackage(cands, pkg)...)
	}
	return nearest(hits, file)
}

// resolveJavaCall resolves x.m() where the extractor knows x's declared type
// T (a field, parameter, local, this, or T itself for a static call): T is
// resolved like any type reference, then m is taken from T's own declaration.
// A type outside the repo (String, a library) stays external.
//
// When T is the repo's but does not declare m, the call is still a call on T:
// m is inherited (a Spring Data repository's findById), generated (a Lombok
// getter) or declared on a supertype. The edge goes to T itself (rule
// javaType) — what the source proves — rather than nowhere: in a large monorepo
// 116k calls on repo types named no declared method, and an impact query on a
// DTO or a repository interface missed every caller. A receiver-less foo() is
// the exception: a name its class does not declare may be a static import
// (when, assertEquals), so it stays external.
func (st *symbolTable) resolveJavaCall(e parser.EdgeInfo, callName, file string) (string, string) {
	typeName := e.Extra["recvType"]
	cls, ok := st.resolveTypeRef(parser.EdgeInfo{File: file, Source: e.Source,
		Extra: map[string]string{"typeName": typeName}})
	if !ok {
		return "", ""
	}
	clsFile := fileOfQN(cls)
	found := ""
	for _, qn := range st.byTypeMethod[typeName+"."+callName] {
		if fileOfQN(qn) != clsFile {
			continue
		}
		if found != "" && found != qn {
			return "", "" // several declarations in one file: not one target
		}
		found = qn
	}
	if found == "" {
		if e.Extra[parser.ExtraJavaSelf] != "" {
			return "", ""
		}
		return cls, ruleJavaType
	}
	return found, ruleJava
}

func cutLastDot(s string) (string, string, bool) {
	i := strings.LastIndex(s, ".")
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// inPackage keeps the candidates declared in a directory spelling the dotted
// package a.b.c as its trailing a/b/c.
func inPackage(cands []string, pkg string) []string {
	suffix := strings.ReplaceAll(pkg, ".", "/")
	var out []string
	for _, qn := range cands {
		dir := path.Dir(fileOfQN(qn))
		if dir == suffix || strings.HasSuffix(dir, "/"+suffix) {
			out = append(out, qn)
		}
	}
	return out
}

// nearest picks the one candidate sharing the longest directory prefix with
// file; ok=false when there is none or the longest is shared.
func nearest(cands []string, file string) (string, bool) {
	best, bestLen, tie := "", -1, false
	for _, qn := range cands {
		n := commonDirPrefix(path.Dir(fileOfQN(qn)), path.Dir(file))
		switch {
		case n > bestLen:
			best, bestLen, tie = qn, n, false
		case n == bestLen:
			tie = true
		}
	}
	if best == "" || tie {
		return "", false
	}
	return best, true
}

func commonDirPrefix(a, b string) int {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	n := 0
	for n < len(as) && n < len(bs) && as[n] == bs[n] {
		n++
	}
	return n
}
