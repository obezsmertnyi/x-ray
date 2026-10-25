//go:build cgo

package parser

import (
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// maxCodeBodyChars caps a code node's stored Body. Bodies are display-only
// (get_code_node / UI) and live in memory + the snapshot, so an enormous
// generated file should not bloat the graph; a function/type longer than this is
// truncated with a marker. Generous enough that ordinary functions are stored
// whole.
const maxCodeBodyChars = 8192

// capBody trims surrounding whitespace and truncates an over-long code body,
// appending a marker so a reader knows it was cut.
func capBody(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxCodeBodyChars {
		return s
	}
	return s[:maxCodeBodyChars] + "\n… (truncated)"
}

// walkNamed invokes fn on every named descendant of node (pre-order).
func walkNamed(node *tree_sitter.Node, fn func(*tree_sitter.Node)) {
	n := int(node.NamedChildCount())
	for i := 0; i < n; i++ {
		child := node.NamedChild(uint(i))
		fn(child)
		walkNamed(child, fn)
	}
}

func trimQuotes(s string) string {
	return strings.Trim(s, "`\"")
}

func lastSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// goDeclName returns the "name" field of a function/method declaration.
func goDeclName(decl *tree_sitter.Node, source []byte) string {
	if nameNode := decl.ChildByFieldName("name"); nameNode != nil {
		return nameNode.Utf8Text(source)
	}
	return ""
}

// goTypeSpecNames returns every type name declared in a type_declaration.
func goTypeSpecNames(decl *tree_sitter.Node, source []byte) []string {
	var names []string
	n := int(decl.NamedChildCount())
	for i := 0; i < n; i++ {
		spec := decl.NamedChild(uint(i))
		if spec.Kind() != "type_spec" {
			continue
		}
		if nameNode := spec.ChildByFieldName("name"); nameNode != nil {
			names = append(names, nameNode.Utf8Text(source))
		}
	}
	return names
}

// goReceiverType extracts the (possibly pointer) receiver type name of a method.
func goReceiverType(method *tree_sitter.Node, source []byte) string {
	recv := method.ChildByFieldName("receiver")
	if recv == nil {
		return ""
	}
	var typeName string
	walkNamed(recv, func(node *tree_sitter.Node) {
		switch node.Kind() {
		case "type_identifier":
			if typeName == "" {
				typeName = node.Utf8Text(source)
			}
		}
	})
	return typeName
}

// goReceiverBinding returns the receiver variable name and its (bare) type for a
// method declaration, e.g. `func (s *VoiceSession) ...` → ("s", "VoiceSession").
func goReceiverBinding(method *tree_sitter.Node, source []byte) (varName, typeName string) {
	recv := method.ChildByFieldName("receiver")
	if recv == nil {
		return "", ""
	}
	// receiver is a parameter_list with a single parameter_declaration: name + type.
	n := int(recv.NamedChildCount())
	for i := 0; i < n; i++ {
		pd := recv.NamedChild(uint(i))
		if pd.Kind() != "parameter_declaration" {
			continue
		}
		if nameNode := pd.ChildByFieldName("name"); nameNode != nil {
			varName = nameNode.Utf8Text(source)
		}
		if typeNode := pd.ChildByFieldName("type"); typeNode != nil {
			typeName = typeRef(typeNode, source, nil).Name
		}
	}
	return varName, typeName
}

// typeRef reads a declared type into a TypeRef: the named type behind pointer
// and generic wrappers, with a package alias resolved to its import path
// (*VoiceSession → VoiceSession; *http.Request → Request from net/http); a
// slice, array, channel or map of a named type as that element with Coll set
// and a map's key in Key. Anything else — a func type, an unnamed nested
// collection — is zero: nothing a chain could be followed on. sc may be nil
// when only the bare name matters.
func typeRef(n *tree_sitter.Node, source []byte, sc *scope) TypeRef {
	switch n.Kind() {
	case "type_identifier":
		return TypeRef{Name: n.Utf8Text(source)}
	case "qualified_type":
		nm, pk := n.ChildByFieldName("name"), n.ChildByFieldName("package")
		if nm != nil && pk != nil {
			return TypeRef{Name: nm.Utf8Text(source), Pkg: importPathOf(pk.Utf8Text(source), sc)}
		}
	case "pointer_type", "parenthesized_type":
		if inner := n.NamedChild(0); inner != nil {
			return typeRef(inner, source, sc)
		}
	case "generic_type":
		if base := n.ChildByFieldName("type"); base != nil {
			return typeRef(base, source, sc)
		}
	case "slice_type", "array_type", "channel_type":
		el := n.ChildByFieldName("element")
		if el == nil {
			el = n.ChildByFieldName("value") // channel_type
		}
		if el != nil {
			if e := typeRef(el, source, sc); e.Name != "" && e.Coll == "" {
				coll := "slice"
				if n.Kind() == "channel_type" {
					coll = "chan"
				}
				return TypeRef{Name: e.Name, Pkg: e.Pkg, Coll: coll}
			}
		}
	case "map_type":
		// A map keeps whichever side is a named type: map[*Entry][]string
		// still yields Entry keys even though its values are unnamed.
		k, v := n.ChildByFieldName("key"), n.ChildByFieldName("value")
		if k != nil && v != nil {
			ke, ve := typeRef(k, source, sc), typeRef(v, source, sc)
			ref := TypeRef{Coll: "map"}
			if ve.Name != "" && ve.Coll == "" {
				ref.Name, ref.Pkg = ve.Name, ve.Pkg
			}
			if ke.Name != "" && ke.Coll == "" {
				ref.Key = &TypeRef{Name: ke.Name, Pkg: ke.Pkg}
			}
			if ref.Name != "" || ref.Key != nil {
				return ref
			}
		}
	}
	return TypeRef{}
}

// typeBinding turns a declared type into a binding.
func typeBinding(typeNode *tree_sitter.Node, source []byte, sc *scope) binding {
	ref := typeRef(typeNode, source, sc)
	if ref == (TypeRef{}) {
		return binding{}
	}
	return binding{ref: ref}
}

// importPathOf maps a package alias to its import path; an alias the file does
// not import (a dot-import, generated code) is kept as-is so the value still
// reads as foreign rather than package-local.
func importPathOf(alias string, sc *scope) string {
	if alias == "" {
		return ""
	}
	if sc != nil {
		if path, ok := sc.importMap[alias]; ok {
			return path
		}
	}
	return alias
}

// bindShortVar handles `x := expr` and `x, err := expr` for a single RHS
// expression. A typed or derived value binds only the first variable (the
// rest is conventionally an error); a call into another package binds every
// value to that origin, since a foreign function returns foreign values.
func bindShortVar(left, right *tree_sitter.Node, source []byte, sc *scope, out map[string]binding) {
	if right.NamedChildCount() != 1 {
		return // a, b := x, y — nothing worth typing
	}
	b := exprBinding(right.NamedChild(0), source, sc, out)
	if b == (binding{}) {
		return
	}
	n := int(left.NamedChildCount())
	for i := 0; i < n; i++ {
		if i > 0 && (b.ref.Name != "" || b.chain != "" || b.rootFunc != "") {
			break
		}
		if id := left.NamedChild(uint(i)); id.Kind() == "identifier" {
			out[id.Utf8Text(source)] = b
		}
	}
}

// exprBinding infers what a RHS expression produces, for the forms below.
// vars is the scope the expression is evaluated in, so a value derived from a
// bound variable (s.memory, r.load()) keeps its provenance as a chain.
func exprBinding(expr *tree_sitter.Node, source []byte, sc *scope, vars map[string]binding) binding {
	switch expr.Kind() {
	case "identifier": // y := x
		return vars[expr.Utf8Text(source)]
	case "composite_literal": // Type{...}, pkg.Type{...}, []Type{...}
		if t := expr.ChildByFieldName("type"); t != nil {
			return typeBinding(t, source, sc)
		}
	case "unary_expression": // &Type{...}
		if operand := expr.ChildByFieldName("operand"); operand != nil {
			return exprBinding(operand, source, sc, vars)
		}
	case "call_expression":
		fn := expr.ChildByFieldName("function")
		if fn == nil {
			return binding{}
		}
		name, operand := goCallName(fn, source)
		switch {
		case operand == nil:
			// helper(...): a package-local function; its declared result types
			// the value at link time (NewServer() and newTestServer(t) alike).
			return binding{rootFunc: name}
		case operand.Kind() == "identifier" && isImportedPkg(operand.Utf8Text(source), sc):
			// pkg.Anything(...): the type is unknown, its origin is not.
			return binding{ref: TypeRef{Pkg: sc.importMap[operand.Utf8Text(source)]}}
		}
		return chainBinding(expr, source, sc, vars)
	case "selector_expression", "index_expression": // x.field, x.a.b, xs[i]
		return chainBinding(expr, source, sc, vars)
	}
	return binding{}
}

// bindRange binds the variables of `for k, v := range x` from what x is known
// to be: a map's key and value, a slice's element (its index is an int and
// stays unbound), a channel's element — directly when x's type is declared,
// as one more chain step ("[k]" / "[]") when x is itself derived.
func bindRange(clause *tree_sitter.Node, source []byte, sc *scope, out map[string]binding) {
	left, right := clause.ChildByFieldName("left"), clause.ChildByFieldName("right")
	if left == nil || right == nil {
		return
	}
	b := exprBinding(right, source, sc, out)
	if b == (binding{}) {
		return
	}
	var names []string // "" for a blank or non-identifier position
	n := int(left.NamedChildCount())
	for i := 0; i < n; i++ {
		c := left.NamedChild(uint(i))
		if c.Kind() == "identifier" {
			names = append(names, c.Utf8Text(source))
		} else {
			names = append(names, "")
		}
	}
	steps := []string{"[k]", "[]"}
	if len(names) == 1 && b.chain == "" && b.rootFunc == "" && b.ref.Coll == "chan" {
		steps = []string{"[]"} // a channel's single range variable is the element
	}
	for i, name := range names {
		if name == "" || i >= len(steps) {
			continue
		}
		if vb := chainFrom(b, []string{steps[i]}); vb != (binding{}) {
			out[name] = vb
		}
	}
}

// chainBinding is the provenance of a value reached from a bound variable by
// selector and call steps; zero when the root is unbound, is an import, or a
// step cannot be followed.
func chainBinding(expr *tree_sitter.Node, source []byte, sc *scope, vars map[string]binding) binding {
	root := goOperandRoot(expr, source)
	if root == "" || isImportedPkg(root, sc) {
		return binding{}
	}
	rb, ok := vars[root]
	if !ok || rb == (binding{}) {
		return binding{}
	}
	segs, ok := goChain(expr, source)
	if !ok || len(segs) == 0 {
		return binding{}
	}
	return chainFrom(rb, segs)
}

// chainFrom extends a binding by further steps: a chain binding grows; a typed
// root starts a chain — after taking, itself, the element or key of a declared
// collection, which needs no linker ([]*Item → Item is right here). A step the
// declared type cannot take (a field on a slice, a key on a slice) is zero.
func chainFrom(rb binding, segs []string) binding {
	if rb.rootFunc != "" {
		steps := strings.Join(segs, ".")
		if rb.chain != "" {
			steps = rb.chain + "." + steps
		}
		return binding{rootFunc: rb.rootFunc, chain: steps}
	}
	if rb.chain != "" {
		return binding{rootType: rb.rootType, rootPkg: rb.rootPkg, chain: rb.chain + "." + strings.Join(segs, ".")}
	}
	ref := rb.ref
	if ref.Coll != "" {
		if len(segs) == 0 {
			return binding{}
		}
		switch segs[0] {
		case "[]":
			ref = TypeRef{Name: ref.Name, Pkg: ref.Pkg}
		case "[k]":
			if ref.Key == nil {
				return binding{}
			}
			ref = *ref.Key
		default:
			return binding{}
		}
		if segs = segs[1:]; len(segs) == 0 {
			return binding{ref: ref}
		}
	}
	if ref.Name == "" && ref.Pkg == "" {
		return binding{}
	}
	return binding{rootType: ref.Name, rootPkg: ref.Pkg, chain: strings.Join(segs, ".")}
}

// bindVarSpec handles `var x Type` / `var x *Type` / `var x pkg.Type`.
func bindVarSpec(spec *tree_sitter.Node, source []byte, sc *scope, out map[string]binding) {
	nameNode := spec.ChildByFieldName("name")
	typeNode := spec.ChildByFieldName("type")
	if nameNode == nil || typeNode == nil {
		return
	}
	if b := typeBinding(typeNode, source, sc); b != (binding{}) {
		out[nameNode.Utf8Text(source)] = b
	}
}

// bindParams binds every named parameter of a parameter_list to its declared
// type: `func f(d Deps, w http.ResponseWriter, a, b int)` binds all four. A
// parameter is the commonest receiver of all, and an unbound one is exactly
// what forces the linker onto its last-resort rule.
func bindParams(params *tree_sitter.Node, source []byte, sc *scope, out map[string]binding) {
	n := int(params.NamedChildCount())
	for i := 0; i < n; i++ {
		pd := params.NamedChild(uint(i))
		if pd.Kind() != "parameter_declaration" && pd.Kind() != "variadic_parameter_declaration" {
			continue
		}
		typeNode := pd.ChildByFieldName("type")
		if typeNode == nil {
			continue
		}
		b := typeBinding(typeNode, source, sc)
		if b == (binding{}) {
			continue
		}
		m := int(pd.NamedChildCount())
		for j := 0; j < m; j++ {
			if c := pd.NamedChild(uint(j)); c.Kind() == "identifier" {
				out[c.Utf8Text(source)] = b
			}
		}
	}
}

// goStructFields records a struct's declared fields, name → type, with
// embedded fields under their bare type name. Named types and collections of
// them are kept (see typeRef); a func field cannot be a chain step. Direct
// fields only — an anonymous nested struct's fields belong to it, not to the
// outer type.
func goStructFields(structNode *tree_sitter.Node, source []byte, sc *scope) map[string]TypeRef {
	var out map[string]TypeRef
	n := int(structNode.NamedChildCount())
	for i := 0; i < n; i++ {
		list := structNode.NamedChild(uint(i))
		if list.Kind() != "field_declaration_list" {
			continue
		}
		m := int(list.NamedChildCount())
		for j := 0; j < m; j++ {
			fd := list.NamedChild(uint(j))
			if fd.Kind() != "field_declaration" {
				continue
			}
			typeNode := fd.ChildByFieldName("type")
			if typeNode == nil {
				continue
			}
			ref := typeRef(typeNode, source, sc)
			if ref == (TypeRef{}) {
				continue
			}
			var names []string
			k := int(fd.NamedChildCount())
			for l := 0; l < k; l++ {
				if c := fd.NamedChild(uint(l)); c.Kind() == "field_identifier" {
					names = append(names, c.Utf8Text(source))
				}
			}
			if len(names) == 0 {
				if ref.Name == "" || ref.Coll != "" {
					continue // an embedded field is always a named type
				}
				names = []string{ref.Name} // embedded field: promoted under its type name
			}
			if out == nil {
				out = map[string]TypeRef{}
			}
			for _, nm := range names {
				out[nm] = ref
			}
		}
	}
	return out
}

// goResultRef returns a function's single result type — `*Graph`, `(g
// *Graph)`, `engine.LinkStats`, `[]*Node` → that type (see typeRef) — or, for
// the `(T, error)` convention, the T. Nothing, several other results (`(int,
// string)`, `(a, b int)`) or a func type → zero.
func goResultRef(decl *tree_sitter.Node, source []byte, sc *scope) TypeRef {
	typeNode := decl.ChildByFieldName("result")
	if typeNode == nil {
		return TypeRef{}
	}
	if typeNode.Kind() == "parameter_list" {
		var decls []*tree_sitter.Node
		n := int(typeNode.NamedChildCount())
		for i := 0; i < n; i++ {
			if c := typeNode.NamedChild(uint(i)); c.Kind() == "parameter_declaration" {
				decls = append(decls, c)
			}
		}
		if len(decls) == 2 {
			// (T, error): the value is a T.
			if t := decls[1].ChildByFieldName("type"); t == nil || t.Utf8Text(source) != "error" {
				return TypeRef{}
			}
			decls = decls[:1]
		}
		if len(decls) != 1 {
			return TypeRef{}
		}
		ids := 0
		m := int(decls[0].NamedChildCount())
		for i := 0; i < m; i++ {
			if decls[0].NamedChild(uint(i)).Kind() == "identifier" {
				ids++
			}
		}
		if ids > 1 {
			return TypeRef{}
		}
		if typeNode = decls[0].ChildByFieldName("type"); typeNode == nil {
			return TypeRef{}
		}
	}
	return typeRef(typeNode, source, sc)
}

// goChain returns the steps of an operand chain from its root outward —
// `s.memory` → ["memory"], `r.Index()` → ["Index()"], `x.a.b()` → ["a",
// "b()"], `xs[i]` → ["[]"] — and ok=false when a step cannot be followed on
// declared types alone: a slice expression, a type assertion, a call whose
// function is not a selector.
func goChain(n *tree_sitter.Node, source []byte) ([]string, bool) {
	var rev []string
	for n != nil {
		switch n.Kind() {
		case "identifier":
			segs := make([]string, len(rev))
			for i, s := range rev {
				segs[len(rev)-1-i] = s
			}
			return segs, true
		case "selector_expression":
			f := n.ChildByFieldName("field")
			if f == nil {
				return nil, false
			}
			rev = append(rev, f.Utf8Text(source))
			n = n.ChildByFieldName("operand")
		case "call_expression":
			fn := n.ChildByFieldName("function")
			if fn == nil || fn.Kind() != "selector_expression" {
				return nil, false
			}
			f := fn.ChildByFieldName("field")
			if f == nil {
				return nil, false
			}
			rev = append(rev, f.Utf8Text(source)+"()")
			n = fn.ChildByFieldName("operand")
		case "index_expression":
			rev = append(rev, "[]")
			n = n.ChildByFieldName("operand")
		case "parenthesized_expression":
			n = n.NamedChild(0)
		default:
			return nil, false
		}
	}
	return nil, false
}

// goEmbeddedTypes returns embedded field type names of a struct/interface
// (Go's form of composition / interface embedding).
func goEmbeddedTypes(typeNode *tree_sitter.Node, source []byte) []string {
	var bases []string
	walkNamed(typeNode, func(node *tree_sitter.Node) {
		// An embedded field is a field_declaration with no field name, whose type
		// is a (possibly qualified) type identifier.
		if node.Kind() != "field_declaration" {
			return
		}
		if node.ChildByFieldName("name") != nil {
			return // named field, not embedded
		}
		if t := node.ChildByFieldName("type"); t != nil {
			switch t.Kind() {
			case "type_identifier", "qualified_type":
				bases = append(bases, t.Utf8Text(source))
			}
		}
	})
	return bases
}

// goCallName splits a call's function expression into the called name and the
// operand it is selected from: `foo()` → ("foo", nil); `x.m()`, `pkg.F()`,
// `a.b.m()`, `f().m()`, `xs[i].m()` → (name, <operand node>). The operand comes
// back as a node, not text, so the caller can decide how much of its shape it
// can type: only a plain identifier can be looked up as an import alias or a
// bound variable. A chained operand used to be dropped here, which turned
// `d.Resolver.UpdateByPaths()` into a bare `UpdateByPaths()` for the linker —
// the wrong shape, resolved by the wrong rule.
func goCallName(fn *tree_sitter.Node, source []byte) (name string, operand *tree_sitter.Node) {
	switch fn.Kind() {
	case "identifier":
		return fn.Utf8Text(source), nil
	case "selector_expression":
		field := fn.ChildByFieldName("field")
		if field == nil {
			return "", nil
		}
		return field.Utf8Text(source), fn.ChildByFieldName("operand")
	}
	return "", nil
}

// goOperandRoot returns the leftmost identifier of an operand chain — `a.b.c`
// → a, `f(x).m` → f, `xs[i]` → xs, `(v)` → v — or "" when the chain does not
// start with a plain identifier (a literal, a func literal, …). It is how a
// chain rooted in an imported package (`os.Stdout.Write`) is recognised as
// external by construction, exactly like `pkg.Func()`.
func goOperandRoot(n *tree_sitter.Node, source []byte) string {
	for n != nil {
		switch n.Kind() {
		case "identifier":
			return n.Utf8Text(source)
		case "selector_expression", "index_expression", "slice_expression", "type_assertion_expression", "unary_expression":
			n = n.ChildByFieldName("operand")
		case "call_expression":
			n = n.ChildByFieldName("function")
		case "parenthesized_expression":
			n = n.NamedChild(0)
		default:
			return ""
		}
	}
	return ""
}

// isGoTest reports whether a function is a Go test/benchmark/fuzz entry point.
func isGoTest(name, params string) bool {
	if strings.HasPrefix(name, "Test") && strings.Contains(params, "*testing.T") {
		return true
	}
	if strings.HasPrefix(name, "Benchmark") && strings.Contains(params, "*testing.B") {
		return true
	}
	if strings.HasPrefix(name, "Fuzz") && strings.Contains(params, "*testing.F") {
		return true
	}
	return false
}
