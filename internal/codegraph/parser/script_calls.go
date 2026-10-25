//go:build cgo

package parser

import (
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// scriptFacts emits, for a TypeScript or Python file, an imports edge per name
// an import binds and a calls edge per call, carrying what the source declares
// about the callee so the linker can resolve it on evidence (graph/scriptlink.go):
//
//   - foo()                 → bare: a function or class this file declares or
//     an import binds
//   - this.foo(), self.foo() → recvType the enclosing class, selfCall
//   - x.foo()               → recvType x's declared type: a parameter or field
//     annotation (x: Foo), a constructor-parameter property, a local assigned
//     new Foo() / Foo(), self.x = Foo()
//   - ns.foo(), mod.foo(), Foo.bar() with no variable of that name → recvVar,
//     resolved when an import binds it
//   - anything else (a chain, a call result) → recvVar as written, external
//
// Scope is per function and flat, like the Java walker. Why: the tags queries
// recorded only the called name, so no TypeScript or Python call resolved and
// get_graph_impact over them saw no callers at all.
func scriptFacts(root *tree_sitter.Node, source []byte, path, lang string) []EdgeInfo {
	w := &scriptWalker{source: source, path: path, python: lang == "python"}
	w.walk(root)
	return w.edges
}

type scriptScope struct {
	class  string
	fields map[string]string
	vars   map[string]string
}

type scriptWalker struct {
	source []byte
	path   string
	python bool
	stack  []*scriptScope
	edges  []EdgeInfo
}

func (w *scriptWalker) text(n *tree_sitter.Node) string {
	if n == nil {
		return ""
	}
	return n.Utf8Text(w.source)
}

func (w *scriptWalker) top() *scriptScope {
	if len(w.stack) == 0 {
		return nil
	}
	return w.stack[len(w.stack)-1]
}

func (w *scriptWalker) importEdge(n *tree_sitter.Node, module, binding, imported string) {
	if module == "" || binding == "" {
		return
	}
	w.edges = append(w.edges, EdgeInfo{
		Kind: EdgeImports, Source: w.path, Target: module, File: w.path,
		Line: int(n.StartPosition().Row) + 1,
		Extra: map[string]string{
			ExtraScriptImport: "1", ExtraBinding: binding, ExtraImported: imported,
		},
	})
}

// typeName reduces a type annotation to the class it names: Foo, Foo<T> and
// a.Foo give Foo (a.Foo as written, for a module-qualified Python type).
func (w *scriptWalker) typeName(t *tree_sitter.Node) string {
	if t == nil {
		return ""
	}
	switch t.Kind() {
	case "type_annotation", "type":
		if c := t.NamedChild(0); c != nil {
			return w.typeName(c)
		}
	case "type_identifier", "identifier":
		return w.text(t)
	case "generic_type":
		return w.typeName(t.ChildByFieldName("name"))
	case "string": // Python forward reference: "Req"
		return strings.Trim(w.text(t), `"'`)
	}
	return ""
}

// constructed is the class a value expression constructs: new Foo(), Foo().
func (w *scriptWalker) constructed(v *tree_sitter.Node) string {
	if v == nil {
		return ""
	}
	switch v.Kind() {
	case "new_expression":
		if c := v.ChildByFieldName("constructor"); c != nil && c.Kind() == "identifier" {
			return w.text(c)
		}
	case "call":
		if f := v.ChildByFieldName("function"); f != nil && f.Kind() == "identifier" {
			if name := w.text(f); name != "" && name[0] >= 'A' && name[0] <= 'Z' {
				return name
			}
		}
	case "await_expression":
		return ""
	}
	return ""
}

func (w *scriptWalker) walk(n *tree_sitter.Node) {
	switch n.Kind() {
	// --- imports ---
	case "import_statement":
		if w.python {
			w.pyImport(n)
		} else {
			w.tsImport(n)
		}
		return
	case "import_from_statement":
		w.pyFromImport(n)
		return

	// --- scopes ---
	case "class_declaration", "abstract_class_declaration", "class_definition", "class":
		sc := &scriptScope{class: w.text(n.ChildByFieldName("name")), fields: map[string]string{}}
		if body := n.ChildByFieldName("body"); body != nil && !w.python {
			for i := uint(0); i < body.NamedChildCount(); i++ {
				c := body.NamedChild(i)
				if c == nil || c.Kind() != "public_field_definition" {
					continue
				}
				name := w.text(c.ChildByFieldName("name"))
				if t := w.typeName(c.ChildByFieldName("type")); t != "" {
					sc.fields[name] = t
				} else if t := w.constructed(c.ChildByFieldName("value")); t != "" {
					sc.fields[name] = t
				}
			}
		}
		w.stack = append(w.stack, sc)
		w.children(n)
		w.stack = w.stack[:len(w.stack)-1]
		return
	case "method_definition", "function_declaration", "function_definition", "arrow_function",
		"function_expression", "generator_function_declaration", "lambda":
		outer := w.top()
		sc := &scriptScope{vars: map[string]string{}, fields: map[string]string{}}
		if outer != nil {
			sc.class, sc.fields = outer.class, outer.fields
			for k, v := range outer.vars {
				sc.vars[k] = v
			}
		}
		if params := n.ChildByFieldName("parameters"); params != nil {
			w.params(params, sc)
		}
		w.stack = append(w.stack, sc)
		w.children(n)
		w.stack = w.stack[:len(w.stack)-1]
		return

	// --- bindings ---
	case "variable_declarator": // TS
		if sc := w.top(); sc != nil && sc.vars != nil {
			name := n.ChildByFieldName("name")
			if name != nil && name.Kind() == "identifier" {
				if t := w.typeName(n.ChildByFieldName("type")); t != "" {
					sc.vars[w.text(name)] = t
				} else if t := w.constructed(n.ChildByFieldName("value")); t != "" {
					sc.vars[w.text(name)] = t
				}
			}
		}
	case "assignment": // Python
		if sc := w.top(); sc != nil {
			left := n.ChildByFieldName("left")
			t := w.typeName(n.ChildByFieldName("type"))
			if t == "" {
				t = w.constructed(n.ChildByFieldName("right"))
			}
			if t == "" && left != nil && left.Kind() == "attribute" {
				// self.repo = repo: the parameter's declared type
				if r := n.ChildByFieldName("right"); r != nil && r.Kind() == "identifier" && sc.vars != nil {
					t = sc.vars[w.text(r)]
				}
			}
			if t != "" && left != nil {
				switch {
				case left.Kind() == "identifier" && sc.vars != nil:
					sc.vars[w.text(left)] = t
				case left.Kind() == "attribute" && w.text(left.ChildByFieldName("object")) == "self" && sc.fields != nil:
					sc.fields[w.text(left.ChildByFieldName("attribute"))] = t
				}
			}
		}

	// --- calls ---
	case "call_expression", "call":
		w.call(n)
	case "new_expression":
		if c := n.ChildByFieldName("constructor"); c != nil && c.Kind() == "identifier" {
			w.emit(n, w.text(c), map[string]string{"bare": "1"})
		}
	}
	w.children(n)
}

func (w *scriptWalker) children(n *tree_sitter.Node) {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c != nil {
			w.walk(c)
		}
	}
}

// params binds typed parameters; a TS constructor parameter with an
// accessibility modifier (private client: Client) is also a field.
func (w *scriptWalker) params(params *tree_sitter.Node, sc *scriptScope) {
	for i := uint(0); i < params.NamedChildCount(); i++ {
		p := params.NamedChild(i)
		if p == nil {
			continue
		}
		name, typ := "", ""
		switch p.Kind() {
		case "required_parameter", "optional_parameter":
			name, typ = w.text(p.ChildByFieldName("pattern")), w.typeName(p.ChildByFieldName("type"))
			if typ != "" && p.NamedChild(0) != nil && p.NamedChild(0).Kind() == "accessibility_modifier" {
				sc.fields[name] = typ
			}
		case "typed_parameter":
			if c := p.NamedChild(0); c != nil && c.Kind() == "identifier" {
				name = w.text(c)
			}
			typ = w.typeName(p.ChildByFieldName("type"))
		case "typed_default_parameter":
			name, typ = w.text(p.ChildByFieldName("name")), w.typeName(p.ChildByFieldName("type"))
		}
		if name != "" && typ != "" {
			sc.vars[name] = typ
		}
	}
}

func (w *scriptWalker) call(n *tree_sitter.Node) {
	fn := n.ChildByFieldName("function")
	if fn == nil {
		return
	}
	sc := w.top()
	switch fn.Kind() {
	case "identifier":
		w.emit(n, w.text(fn), map[string]string{"bare": "1"})
		return
	case "member_expression", "attribute":
	default:
		return
	}
	prop := fn.ChildByFieldName("property")
	if w.python {
		prop = fn.ChildByFieldName("attribute")
	}
	obj := fn.ChildByFieldName("object")
	if prop == nil || obj == nil {
		return
	}
	callName := w.text(prop)
	extra := map[string]string{}
	lookupVar := func(v string) string {
		if sc == nil {
			return ""
		}
		if t, ok := sc.vars[v]; ok {
			return t
		}
		return ""
	}
	objText := w.text(obj)
	switch {
	case obj.Kind() == "this" || (w.python && (objText == "self" || objText == "cls")):
		if sc != nil && sc.class != "" {
			extra["recvType"], extra[ExtraSelfCall] = sc.class, "1"
		}
	case obj.Kind() == "identifier":
		if t := lookupVar(objText); t != "" {
			extra["recvType"] = t
		} else {
			extra["recvVar"] = objText
		}
	case obj.Kind() == "member_expression" || obj.Kind() == "attribute":
		inner, field := obj.ChildByFieldName("object"), obj.ChildByFieldName("property")
		if w.python {
			field = obj.ChildByFieldName("attribute")
		}
		innerText := w.text(inner)
		if sc != nil && (innerText == "this" || (w.python && innerText == "self")) {
			if t := sc.fields[w.text(field)]; t != "" {
				extra["recvType"] = t
				break
			}
		}
		extra["recvVar"] = objText // a dotted module path (os.path) or a chain
	default:
		extra["recvVar"] = objText
	}
	if extra["recvType"] == "" && extra["recvVar"] == "" {
		return
	}
	w.emit(n, callName, extra)
}

func (w *scriptWalker) emit(n *tree_sitter.Node, callName string, extra map[string]string) {
	if callName == "" {
		return
	}
	extra["callName"] = callName
	extra[ExtraScriptCall] = "1"
	w.edges = append(w.edges, EdgeInfo{
		Kind: EdgeCalls, Target: callName, File: w.path,
		Line: int(n.StartPosition().Row) + 1, Extra: extra,
	})
}

// tsImport: import D, { A, B as C } from './x'; import * as ns from './y'.
func (w *scriptWalker) tsImport(n *tree_sitter.Node) {
	src := n.ChildByFieldName("source")
	if src == nil {
		return
	}
	module := strings.Trim(w.text(src), `"'`)
	for i := uint(0); i < n.NamedChildCount(); i++ {
		clause := n.NamedChild(i)
		if clause == nil || clause.Kind() != "import_clause" {
			continue
		}
		for j := uint(0); j < clause.NamedChildCount(); j++ {
			c := clause.NamedChild(j)
			if c == nil {
				continue
			}
			switch c.Kind() {
			case "identifier":
				w.importEdge(n, module, w.text(c), "default")
			case "namespace_import":
				if id := c.NamedChild(0); id != nil {
					w.importEdge(n, module, w.text(id), "*")
				}
			case "named_imports":
				for k := uint(0); k < c.NamedChildCount(); k++ {
					spec := c.NamedChild(k)
					if spec == nil || spec.Kind() != "import_specifier" {
						continue
					}
					name := w.text(spec.ChildByFieldName("name"))
					alias := name
					if a := spec.ChildByFieldName("alias"); a != nil {
						alias = w.text(a)
					}
					w.importEdge(n, module, alias, name)
				}
			}
		}
	}
}

// pyImport: import a.b (binds a.b as written) / import a.b as c.
func (w *scriptWalker) pyImport(n *tree_sitter.Node) {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if c == nil {
			continue
		}
		switch c.Kind() {
		case "dotted_name":
			w.importEdge(n, w.text(c), w.text(c), "")
		case "aliased_import":
			w.importEdge(n, w.text(c.ChildByFieldName("name")), w.text(c.ChildByFieldName("alias")), "")
		}
	}
}

// pyFromImport: from .sib import A, b as c / from ..up import *.
func (w *scriptWalker) pyFromImport(n *tree_sitter.Node) {
	mod := n.ChildByFieldName("module_name")
	if mod == nil {
		return
	}
	module := w.text(mod) // ".sib", "..up", "a.b", "."
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if c == nil || c.Equals(*mod) {
			continue
		}
		switch c.Kind() {
		case "dotted_name":
			w.importEdge(n, module, w.text(c), w.text(c))
		case "aliased_import":
			w.importEdge(n, module, w.text(c.ChildByFieldName("alias")), w.text(c.ChildByFieldName("name")))
		case "wildcard_import":
			w.importEdge(n, module, "*", "*")
		}
	}
}
