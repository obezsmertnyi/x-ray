package graph

import (
	"path"
	"strings"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/appconfig"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// scriptImport is one name a TypeScript or Python import binds in a file.
type scriptImport struct {
	module   string // as written: "./lib/x", "..up", "pkg.mod"
	imported string // symbol name, "default", "*", or "" for the module itself
}

// resolveScriptCall resolves a TypeScript or Python call on the evidence the
// extractor recorded (parser/script_calls.go), and only where it points:
//
//   - recvType T: T is a class this file declares, or one an import binds;
//     the method is T's own declaration in T's file. A method T does not
//     declare lands on T itself (inherited, or assigned at runtime) unless the
//     receiver is this/self — the same rule as Java's javaType.
//   - bare foo(): a top-level function or class this file declares, else the
//     one an import binds (a class call is its construction).
//   - recvVar v: v bound by an import to a module or namespace → the top-level
//     function or class of that name in the module's file; v bound to a class
//     → that class's method, as for recvType.
//
// A module resolves to a repo file only through the path it names: a relative
// TypeScript specifier from the importing file's directory (./x → x.ts, x.tsx,
// x/index.ts …), a Python module by its dotted path (a.b → a/b.py or
// a/b/__init__.py, relative imports from the file's package, absolute ones
// matched on a unique path suffix so a src/ root need not be known). A bare
// TypeScript package or a tsconfig path alias names no repo file and stays
// external.
func (st *symbolTable) resolveScriptCall(e parser.EdgeInfo, callName, file string) (string, string) {
	imports := st.scriptImports[file]
	if t := e.Extra["recvType"]; t != "" {
		cls := st.scriptClass(t, file)
		if cls == "" {
			return "", ""
		}
		return st.memberOf(cls, callName, e.Extra[parser.ExtraSelfCall] != "")
	}
	if e.Extra["bare"] == "1" {
		if qn := st.topLevel(callName, file); qn != "" {
			return qn, ruleScript
		}
		if imp, ok := imports[callName]; ok {
			if mf := st.moduleFile(imp.module, file); mf != "" {
				name := imp.imported
				if name == "" || name == "*" || name == "default" {
					name = callName
				}
				if qn := st.topLevel(name, mf); qn != "" {
					return qn, ruleScript
				}
			}
		}
		// from m import *: a name the file neither declares nor imports by name.
		if imp, ok := imports["*"]; ok {
			if mf := st.moduleFile(imp.module, file); mf != "" {
				if qn := st.topLevel(callName, mf); qn != "" {
					return qn, ruleScript
				}
			}
		}
		return "", ""
	}
	v := e.Extra["recvVar"]
	imp, ok := imports[v]
	if !ok {
		return "", ""
	}
	switch imp.imported {
	case "", "*":
		if mf := st.moduleFile(imp.module, file); mf != "" {
			if qn := st.topLevel(callName, mf); qn != "" {
				return qn, ruleScript
			}
		}
		return "", ""
	}
	// from a import B / import { B } from './a': B is a class (static call) or
	// a submodule (Python: from pkg import mod).
	if mf := st.moduleFile(imp.module, file); mf != "" {
		if cls := st.classIn(imp.imported, mf); cls != "" {
			return st.memberOf(cls, callName, false)
		}
	}
	if mf := st.moduleFile(joinModule(imp.module, imp.imported), file); mf != "" {
		if qn := st.topLevel(callName, mf); qn != "" {
			return qn, ruleScript
		}
	}
	return "", ""
}

func joinModule(module, name string) string {
	if strings.HasSuffix(module, ".") {
		return module + name
	}
	return module + "." + name
}

// memberOf is cls's own method name, else cls itself unless self.
func (st *symbolTable) memberOf(cls, name string, self bool) (string, string) {
	n, ok := st.nodes[cls]
	if !ok {
		return "", ""
	}
	clsFile := fileOfQN(cls)
	for _, qn := range st.byTypeMethod[n.Name+"."+name] {
		if fileOfQN(qn) == clsFile {
			return qn, ruleScript
		}
	}
	if self {
		return "", ""
	}
	return cls, ruleScriptType
}

// scriptClass is the class named t that file can see: its own, or imported.
func (st *symbolTable) scriptClass(t, file string) string {
	if qn := st.classIn(t, file); qn != "" {
		return qn
	}
	imp, ok := st.scriptImports[file][t]
	if !ok {
		return ""
	}
	mf := st.moduleFile(imp.module, file)
	if mf == "" {
		return ""
	}
	name := imp.imported
	if name == "" || name == "*" || name == "default" {
		name = t
	}
	return st.classIn(name, mf)
}

func (st *symbolTable) classIn(name, file string) string {
	for _, qn := range st.byClass[name] {
		if fileOfQN(qn) == file {
			return qn
		}
	}
	return ""
}

// topLevel is a function or class named name declared at the top of file.
func (st *symbolTable) topLevel(name, file string) string {
	for _, qn := range st.byFunc[name] {
		if fileOfQN(qn) == file {
			return qn
		}
	}
	return st.classIn(name, file)
}

var tsModuleSuffixes = []string{"", ".ts", ".tsx", ".d.ts", ".js", ".jsx", "/index.ts", "/index.tsx", "/index.js"}

// moduleFile maps an import's module onto the repo file it names, or "".
func (st *symbolTable) moduleFile(module, from string) string {
	if strings.HasSuffix(from, ".py") {
		return st.pyModuleFile(module, from)
	}
	if !strings.HasPrefix(module, ".") {
		return st.aliasedFile(module, from)
	}
	return st.tsFile(path.Join(path.Dir(from), module))
}

// tsConfig is what a tsconfig.json/jsconfig.json says about resolving a
// non-relative import: its baseUrl and compilerOptions.paths patterns.
type tsConfig struct {
	baseURL string
	paths   map[string][]string // "@/*" -> ["./src/*"]
}

func (st *symbolTable) addTSConfig(n parser.NodeInfo) {
	if !appconfig.IsTSConfig(n.FilePath) {
		return
	}
	dir := path.Dir(n.FilePath)
	c := st.tsConfigs[dir]
	if c == nil {
		c = &tsConfig{paths: map[string][]string{}}
		st.tsConfigs[dir] = c
	}
	switch {
	case n.Name == "compilerOptions.baseUrl":
		c.baseURL = n.Body
	case strings.HasPrefix(n.Name, "compilerOptions.paths."):
		c.paths[strings.TrimPrefix(n.Name, "compilerOptions.paths.")] = strings.Split(n.Body, ",")
	}
}

// aliasedFile resolves a non-relative TypeScript import through the nearest
// tsconfig/jsconfig above the importing file: a compilerOptions.paths pattern
// ("@/*" → "./src/*", relative to baseUrl when set), else baseUrl itself. With
// neither, the specifier is a package and names no repo file. Why: a Next.js
// app imports its own code as @/components/…, and without the alias 147 of
// a UI repository's imports resolved to nothing.
func (st *symbolTable) aliasedFile(module, from string) string {
	for dir := path.Dir(from); ; dir = path.Dir(dir) {
		if c := st.tsConfigs[dir]; c != nil && (len(c.paths) > 0 || c.baseURL != "") {
			base := path.Join(dir, c.baseURL)
			for pattern, targets := range c.paths {
				prefix, suffix, wild := strings.Cut(pattern, "*")
				var rest string
				switch {
				case !wild && module == pattern:
				case wild && strings.HasPrefix(module, prefix) && strings.HasSuffix(module, suffix) && len(module) >= len(prefix)+len(suffix):
					rest = module[len(prefix) : len(module)-len(suffix)]
				default:
					continue
				}
				for _, target := range targets {
					if f := st.tsFile(path.Join(base, strings.Replace(target, "*", rest, 1))); f != "" {
						return f
					}
				}
			}
			if c.baseURL != "" {
				return st.tsFile(path.Join(base, module))
			}
			return ""
		}
		if dir == "." || dir == "/" {
			return ""
		}
	}
}

// tsFile is the source file an import path without extension names.
func (st *symbolTable) tsFile(base string) string {
	// An ESM import written with .js names the .ts source it compiles from.
	trimmed := strings.TrimSuffix(base, ".js")
	for _, b := range []string{base, trimmed} {
		for _, suf := range tsModuleSuffixes {
			if st.files[b+suf] {
				return b + suf
			}
		}
	}
	return ""
}

func (st *symbolTable) pyModuleFile(module, from string) string {
	rel := strings.TrimLeft(module, ".")
	dots := len(module) - len(rel)
	parts := strings.ReplaceAll(rel, ".", "/")
	candidates := func(p string) []string {
		if p == "" || p == "." {
			return []string{path.Join(p, "__init__.py")}
		}
		return []string{p + ".py", path.Join(p, "__init__.py")}
	}
	if dots > 0 {
		dir := path.Dir(from)
		for i := 1; i < dots; i++ {
			dir = path.Dir(dir)
		}
		for _, c := range candidates(path.Join(dir, parts)) {
			if st.files[c] {
				return c
			}
		}
		return ""
	}
	if parts == "" {
		return ""
	}
	// Absolute: the file whose path ends with the module's, when exactly one
	// does. The same module inside two services' trees (both with a
	// shared/util.py) prefers the importing file's own tree.
	var hits []string
	for _, c := range candidates(parts) {
		for f := range st.pyFiles {
			if f == c || strings.HasSuffix(f, "/"+c) {
				hits = append(hits, f)
			}
		}
	}
	switch len(hits) {
	case 0:
		return ""
	case 1:
		return hits[0]
	}
	var qns []string
	for _, h := range hits {
		qns = append(qns, h+"::")
	}
	if qn, ok := nearest(qns, from); ok {
		return fileOfQN(qn)
	}
	return ""
}
