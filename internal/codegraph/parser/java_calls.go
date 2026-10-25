//go:build cgo

package parser

import (
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// javaCalls emits a calls edge per Java method invocation, carrying what the
// source declares about its receiver so the linker can resolve it the way it
// resolves Go: on declared evidence, never on the name alone.
//
//   - foo(), this.foo() → recvType: the enclosing class
//   - x.foo() where x is a parameter, a local, a for-each variable or a field of
//     the enclosing class → recvType: x's declared type
//   - Foo.bar() where Foo is no variable in scope → recvType: Foo (a static call)
//   - anything else (a chain, a call result, super) → recvVar, left external
//
// A recvType here is a bare Java type name, resolved by the linker through the
// file's imports and package (graph/typelink.go) with ExtraJavaRecv set. Scope
// is per method and flat: a local is visible after its declaration anywhere in
// the method, which over-approximates block scope only when two blocks declare
// the same name with different types — the later declaration wins then.
//
// Why: the tags query recorded only the method name, so none of a large
// monorepo's 189,053 Java calls resolved and get_graph_impact over Java
// followed type references alone.
func javaCalls(root *tree_sitter.Node, source []byte, path string) []EdgeInfo {
	w := &javaCallWalker{source: source, path: path}
	w.walk(root)
	return w.edges
}

type javaScope struct {
	class  string
	fields map[string]string
	vars   map[string]string // method scope; nil outside a method

	// fieldValues and localValues resolve a field or local's own address text
	// when it is a string literal or a Spring @Value("${...}") placeholder —
	// the config-shaped inputs an imperative HTTP client call (RestTemplate,
	// WebServiceTemplate) passes through, the same text a Feign url is
	// expanded from. fieldValues is class-wide and shared like fields;
	// localValues is method-scoped like vars.
	fieldValues map[string]string
	localValues map[string]string
	// params is the enclosing method's own formal parameters, in declared
	// order. A call whose address argument is exactly params[0] forwards its
	// own first parameter untouched — the shape a hand-written HTTP client
	// wrapper takes instead of writing the address at the call site.
	params []string
}

type javaCallWalker struct {
	source []byte
	path   string
	stack  []*javaScope
	edges  []EdgeInfo
}

func (w *javaCallWalker) text(n *tree_sitter.Node) string { return n.Utf8Text(w.source) }

func (w *javaCallWalker) top() *javaScope {
	if len(w.stack) == 0 {
		return nil
	}
	return w.stack[len(w.stack)-1]
}

// javaTypeName reduces a declared type to the class it names: List<Foo> →
// List, a.b.Foo → Foo, Foo[] → Foo; a primitive or var gives "".
func (w *javaCallWalker) javaTypeName(t *tree_sitter.Node) string {
	if t == nil {
		return ""
	}
	switch t.Kind() {
	case "type_identifier":
		if name := w.text(t); name != "var" {
			return name
		}
	case "generic_type", "array_type", "annotated_type":
		for i := uint(0); i < t.NamedChildCount(); i++ {
			if c := t.NamedChild(i); c != nil && c.Kind() != "type_arguments" && c.Kind() != "dimensions" && c.Kind() != "marker_annotation" && c.Kind() != "annotation" {
				return w.javaTypeName(c)
			}
		}
	case "scoped_type_identifier":
		txt := w.text(t)
		return txt[strings.LastIndex(txt, ".")+1:]
	}
	return ""
}

// declare binds each declarator name under decl to its declared type.
func (w *javaCallWalker) declare(into map[string]string, decl *tree_sitter.Node) {
	typ := w.javaTypeName(decl.ChildByFieldName("type"))
	if typ == "" || into == nil {
		return
	}
	if name := decl.ChildByFieldName("name"); name != nil { // formal_parameter, for-each
		into[w.text(name)] = typ
	}
	for i := uint(0); i < decl.NamedChildCount(); i++ {
		c := decl.NamedChild(i)
		if c != nil && c.Kind() == "variable_declarator" {
			if name := c.ChildByFieldName("name"); name != nil {
				into[w.text(name)] = typ
			}
		}
	}
}

func (w *javaCallWalker) walk(n *tree_sitter.Node) {
	switch n.Kind() {
	case "class_declaration", "interface_declaration", "enum_declaration", "record_declaration":
		sc := &javaScope{fields: map[string]string{}, fieldValues: map[string]string{}}
		if name := n.ChildByFieldName("name"); name != nil {
			sc.class = w.text(name)
		}
		if body := n.ChildByFieldName("body"); body != nil {
			for i := uint(0); i < body.NamedChildCount(); i++ {
				if c := body.NamedChild(i); c != nil && (c.Kind() == "field_declaration" || c.Kind() == "constant_declaration") {
					w.declare(sc.fields, c)
					w.declareFieldValue(sc.fieldValues, c)
				}
			}
		}
		// A record's components are its fields.
		if params := n.ChildByFieldName("parameters"); params != nil {
			for i := uint(0); i < params.NamedChildCount(); i++ {
				if c := params.NamedChild(i); c != nil {
					w.declare(sc.fields, c)
				}
			}
		}
		w.stack = append(w.stack, sc)
		w.children(n)
		w.stack = w.stack[:len(w.stack)-1]
		return
	case "method_declaration", "constructor_declaration", "compact_constructor_declaration", "lambda_expression":
		outer := w.top()
		sc := &javaScope{vars: map[string]string{}, fields: map[string]string{}, localValues: map[string]string{}}
		if outer != nil {
			sc.class, sc.fields = outer.class, outer.fields
			sc.fieldValues, sc.params = outer.fieldValues, outer.params
			for k, v := range outer.vars { // a lambda sees its method's locals
				sc.vars[k] = v
			}
			for k, v := range outer.localValues {
				sc.localValues[k] = v
			}
		}
		// A method or constructor's own parameter order matters — params[0]
		// is what an address argument forwarding it means — so it is computed
		// fresh here rather than inherited; a lambda keeps its enclosing
		// method's, the closest approximation without modeling closures.
		isMethod := n.Kind() == "method_declaration" || n.Kind() == "constructor_declaration"
		if isMethod {
			sc.params = nil
		}
		if params := n.ChildByFieldName("parameters"); params != nil {
			for i := uint(0); i < params.NamedChildCount(); i++ {
				if c := params.NamedChild(i); c != nil {
					w.declare(sc.vars, c)
					if isMethod {
						if name := c.ChildByFieldName("name"); name != nil {
							sc.params = append(sc.params, w.text(name))
						}
					}
				}
			}
		}
		w.stack = append(w.stack, sc)
		w.children(n)
		w.stack = w.stack[:len(w.stack)-1]
		return
	case "local_variable_declaration", "enhanced_for_statement", "catch_formal_parameter", "resource":
		if sc := w.top(); sc != nil {
			w.declare(sc.vars, n)
			if n.Kind() == "local_variable_declaration" {
				w.declareLocalValue(sc.localValues, n)
			}
		}
	case "method_invocation":
		w.call(n)
	}
	w.children(n)
}

func (w *javaCallWalker) children(n *tree_sitter.Node) {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c != nil {
			w.walk(c)
		}
	}
}

func (w *javaCallWalker) call(n *tree_sitter.Node) {
	name := n.ChildByFieldName("name")
	if name == nil {
		return
	}
	callName := w.text(name)
	extra := map[string]string{"callName": callName}
	sc := w.top()
	lookup := func(v string) string {
		if sc == nil {
			return ""
		}
		if t, ok := sc.vars[v]; ok {
			return t
		}
		return sc.fields[v]
	}
	recvType := ""
	obj := n.ChildByFieldName("object")
	switch {
	case obj == nil || obj.Kind() == "this":
		if sc != nil {
			recvType = sc.class
			extra[ExtraJavaSelf] = "1"
		}
	case obj.Kind() == "identifier":
		v := w.text(obj)
		if t := lookup(v); t != "" {
			recvType = t
		} else if v != "" && v[0] >= 'A' && v[0] <= 'Z' {
			recvType = v // no variable of that name: a static call on a type
		} else {
			extra["recvVar"] = v
		}
	case obj.Kind() == "field_access":
		if o := obj.ChildByFieldName("object"); o != nil && o.Kind() == "this" && sc != nil {
			if f := obj.ChildByFieldName("field"); f != nil {
				recvType = sc.fields[w.text(f)]
			}
		}
		if recvType == "" {
			extra["recvVar"] = w.text(obj)
		}
	default:
		extra["recvVar"] = w.text(obj)
	}
	if recvType != "" {
		extra["recvType"] = recvType
		extra[ExtraJavaRecv] = "1"
	}
	if args := n.ChildByFieldName("arguments"); args != nil && args.NamedChildCount() > 0 {
		if v, isParam0 := w.resolveArg0(args.NamedChild(0), sc); v != "" {
			extra[ExtraArgPrefix+"url"] = v
		} else if isParam0 {
			extra[ExtraArgURLParam0] = "1"
		}
	}
	w.edges = append(w.edges, EdgeInfo{
		Kind:   EdgeCalls,
		Target: callName,
		File:   w.path,
		Line:   int(n.StartPosition().Row) + 1,
		Extra:  extra,
	})
}

// resolveArg0 is a call's first argument, resolved the same way a Feign url
// is: a literal, or a field carrying a literal or a Spring @Value("${...}")
// placeholder — else, when it is exactly the enclosing method's own first
// parameter, isParam0 reports that a caller supplies it (services_graph.go
// follows the call graph back to find who). Any other shape (a chain, a
// concatenation, a constructor) is left unresolved: no edge beats a guessed
// one.
func (w *javaCallWalker) resolveArg0(arg *tree_sitter.Node, sc *javaScope) (value string, isParam0 bool) {
	switch arg.Kind() {
	case "string_literal":
		return javaUnquote(w.text(arg)), false
	case "identifier":
		name := w.text(arg)
		if sc != nil && len(sc.params) > 0 && sc.params[0] == name {
			return "", true
		}
		if sc != nil {
			if v, ok := sc.localValues[name]; ok {
				return v, false
			}
			if v, ok := sc.fieldValues[name]; ok {
				return v, false
			}
		}
	case "field_access":
		if o := arg.ChildByFieldName("object"); o != nil && o.Kind() == "this" && sc != nil {
			if f := arg.ChildByFieldName("field"); f != nil {
				if v, ok := sc.fieldValues[w.text(f)]; ok {
					return v, false
				}
			}
		}
	}
	return "", false
}

// declareFieldValue records a field's own address text: a Spring
// @Value("${...}") placeholder on the field_declaration (whichever
// declarator it names), else a string literal initializer, per declarator.
// into is nil for no scope the walker builds today, but guarded like declare
// in case a future scope (an anonymous class body, say) reaches it without
// one.
func (w *javaCallWalker) declareFieldValue(into map[string]string, decl *tree_sitter.Node) {
	if into == nil {
		return
	}
	annoVal := w.valueAnnotationArg(decl)
	for i := uint(0); i < decl.NamedChildCount(); i++ {
		c := decl.NamedChild(i)
		if c == nil || c.Kind() != "variable_declarator" {
			continue
		}
		name := c.ChildByFieldName("name")
		if name == nil {
			continue
		}
		if annoVal != "" {
			into[w.text(name)] = annoVal
			continue
		}
		if v := c.ChildByFieldName("value"); v != nil && v.Kind() == "string_literal" {
			into[w.text(name)] = javaUnquote(w.text(v))
		}
	}
}

// declareLocalValue records a local's own address text when its initializer
// is a plain string literal — deliberately not a concatenation or a chained
// getter (SpotfireLoader.getSpotfireConfig().getUrl() + PATH): a value read
// at runtime from outside config has no config-shaped answer, and leaving it
// unresolved is correct.
func (w *javaCallWalker) declareLocalValue(into map[string]string, decl *tree_sitter.Node) {
	// into is the top scope's localValues, nil whenever that scope is a
	// class/interface/enum/record scope rather than a method one: a
	// local_variable_declaration reaches call() under the enclosing class
	// scope, with no scope of its own, when it sits directly in a static or
	// instance initializer block — walk() pushes a scope for a method,
	// constructor or lambda, not for an initializer block. declare() already
	// drops a var it has nowhere to put (into == nil) the same way; this
	// matches it, rather than crashing on the write.
	if into == nil {
		return
	}
	for i := uint(0); i < decl.NamedChildCount(); i++ {
		c := decl.NamedChild(i)
		if c == nil || c.Kind() != "variable_declarator" {
			continue
		}
		name := c.ChildByFieldName("name")
		v := c.ChildByFieldName("value")
		if name == nil || v == nil || v.Kind() != "string_literal" {
			continue
		}
		into[w.text(name)] = javaUnquote(w.text(v))
	}
}

// valueAnnotationArg is a field_declaration's @Value("...") argument text,
// unquoted, or "" when it carries no such annotation.
func (w *javaCallWalker) valueAnnotationArg(decl *tree_sitter.Node) string {
	for i := uint(0); i < decl.NamedChildCount(); i++ {
		mods := decl.NamedChild(i)
		if mods == nil || mods.Kind() != "modifiers" {
			continue
		}
		for j := uint(0); j < mods.NamedChildCount(); j++ {
			a := mods.NamedChild(j)
			if a == nil || a.Kind() != "annotation" {
				continue
			}
			name := a.ChildByFieldName("name")
			if name == nil || w.text(name) != "Value" {
				continue
			}
			args := a.ChildByFieldName("arguments")
			if args == nil {
				continue
			}
			for k := uint(0); k < args.NamedChildCount(); k++ {
				v := args.NamedChild(k)
				if v != nil && v.Kind() == "string_literal" {
					return javaUnquote(w.text(v))
				}
			}
		}
	}
	return ""
}

// javaUnquote strips a string_literal's surrounding quotes. Escapes are left
// as written, matching annotationValue in tags.go.
func javaUnquote(s string) string {
	return strings.Trim(s, `"`)
}
