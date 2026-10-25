//go:build cgo

package parser

import (
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// collectScope pre-scans the top level of the file for imports and the names of
// top-level declarations, so call targets can be resolved during the main walk.
func collectScope(root *tree_sitter.Node, spec *langSpec, source []byte) *scope {
	sc := &scope{importMap: map[string]string{}, definedNames: map[string]bool{}}
	n := int(root.NamedChildCount())
	for i := 0; i < n; i++ {
		child := root.NamedChild(uint(i))
		nt := child.Kind()
		switch {
		case spec.importTypes[nt]:
			collectGoImports(child, source, sc)
		case spec.funcTypes[nt]:
			if name := goDeclName(child, source); name != "" {
				sc.definedNames[name] = true
			}
		case spec.classTypes[nt]:
			for _, name := range goTypeSpecNames(child, source) {
				sc.definedNames[name] = true
			}
		}
	}
	return sc
}

// collectGoImports records alias->path (or last path segment -> path) mappings.
func collectGoImports(decl *tree_sitter.Node, source []byte, sc *scope) {
	walkNamed(decl, func(node *tree_sitter.Node) {
		if node.Kind() != "import_spec" {
			return
		}
		pathNode := node.ChildByFieldName("path")
		if pathNode == nil {
			return
		}
		raw := pathNode.Utf8Text(source)
		path := trimQuotes(raw)
		alias := lastSegment(path)
		if nameNode := node.ChildByFieldName("name"); nameNode != nil {
			alias = nameNode.Utf8Text(source)
		}
		sc.importMap[alias] = path
	})
}

func (w *walker) extractImports(decl *tree_sitter.Node) {
	walkNamed(decl, func(node *tree_sitter.Node) {
		if node.Kind() != "import_spec" {
			return
		}
		pathNode := node.ChildByFieldName("path")
		if pathNode == nil {
			return
		}
		path := trimQuotes(pathNode.Utf8Text(w.source))
		w.res.Edges = append(w.res.Edges, EdgeInfo{
			Kind:   EdgeImports,
			Source: w.path,
			Target: path,
			File:   w.path,
			Line:   w.line(node),
		})
	})
}

// extractClass handles Go `type_declaration` (which may hold several type_specs).
func (w *walker) extractClass(decl *tree_sitter.Node, enclosingClass string) bool {
	emitted := false
	n := int(decl.NamedChildCount())
	for i := 0; i < n; i++ {
		spec := decl.NamedChild(uint(i))
		if spec.Kind() != "type_spec" {
			continue
		}
		nameNode := spec.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		name := nameNode.Utf8Text(w.source)
		typeNode := spec.ChildByFieldName("type")
		kind := "Type"
		if typeNode != nil {
			switch typeNode.Kind() {
			case "struct_type", "interface_type":
				kind = "Class" // struct/interface = the closest Go has to a class
			}
		}
		node := NodeInfo{
			Kind:       kind,
			Name:       name,
			FilePath:   w.path,
			Language:   w.spec.name,
			LineStart:  w.line(spec),
			LineEnd:    int(spec.EndPosition().Row) + 1,
			ParentName: enclosingClass,
			// Full type-spec source for display (one node per spec, so a grouped
			// `type ( ... )` block yields one Body per type). Display-only; the
			// embedding uses the synthesized signature.
			Body: capBody("type " + spec.Utf8Text(w.source)),
		}
		if typeNode != nil && typeNode.Kind() == "struct_type" {
			node.Fields = goStructFields(typeNode, w.source, w.scope)
		}
		w.res.Nodes = append(w.res.Nodes, node)
		w.res.Edges = append(w.res.Edges, EdgeInfo{
			Kind: EdgeContains, Source: w.path, Target: node.Qualified(),
			File: w.path, Line: w.line(spec),
		})
		// Embedded fields in a struct/interface => inherits edges.
		if typeNode != nil {
			for _, base := range goEmbeddedTypes(typeNode, w.source) {
				w.res.Edges = append(w.res.Edges, EdgeInfo{
					Kind: EdgeInherits, Source: node.Qualified(), Target: base,
					File: w.path, Line: w.line(spec),
				})
			}
		}
		emitted = true
	}
	return emitted
}

// extractFunc handles function_declaration and method_declaration.
func (w *walker) extractFunc(decl *tree_sitter.Node, enclosingClass, enclosingFunc string) bool {
	nameNode := decl.ChildByFieldName("name")
	if nameNode == nil {
		return false
	}
	name := nameNode.Utf8Text(w.source)

	parent := enclosingClass
	// Methods attach to their receiver type.
	if decl.Kind() == "method_declaration" {
		if recv := goReceiverType(decl, w.source); recv != "" {
			parent = recv
		}
	}

	params := ""
	if pn := decl.ChildByFieldName("parameters"); pn != nil {
		params = pn.Utf8Text(w.source)
	}
	ret := ""
	if rn := decl.ChildByFieldName("result"); rn != nil {
		ret = rn.Utf8Text(w.source)
	}

	isTest := isGoTest(name, params)
	kind := "Func"
	if isTest {
		kind = "Test"
	}
	node := NodeInfo{
		Kind:       kind,
		Name:       name,
		FilePath:   w.path,
		Language:   w.spec.name,
		LineStart:  w.line(decl),
		LineEnd:    int(decl.EndPosition().Row) + 1,
		ParentName: parent,
		Params:     params,
		ReturnType: ret,
		Result:     goResultRef(decl, w.source, w.scope),
		IsTest:     isTest,
		// Full declaration source for display (get_code_node / UI). Embedding still
		// uses the synthesized signature via nodeText — Body is display-only.
		Body: capBody(decl.Utf8Text(w.source)),
	}
	w.res.Nodes = append(w.res.Nodes, node)
	w.res.Edges = append(w.res.Edges, EdgeInfo{
		Kind: EdgeContains, Source: w.path, Target: node.Qualified(),
		File: w.path, Line: w.line(decl),
	})

	// Recurse into the body, attributing calls to this function. Local variable
	// types are collected inline during this single walk (see noteGoVarType) so
	// x.method() calls can be resolved by the linker — no second traversal.
	if body := decl.ChildByFieldName("body"); body != nil {
		saved := w.varTypes
		w.varTypes = map[string]binding{}
		if recvVar, recvType := goReceiverBinding(decl, w.source); recvVar != "" {
			w.varTypes[recvVar] = binding{ref: TypeRef{Name: recvType}}
		}
		if params := decl.ChildByFieldName("parameters"); params != nil {
			bindParams(params, w.source, w.scope, w.varTypes)
		}
		w.walk(body, parent, name)
		w.varTypes = saved
	}
	return true
}

// walkFuncLiteral walks a closure with the enclosing scope's bindings plus the
// closure's own parameters — the handler shape, `func(w http.ResponseWriter, r
// *http.Request) {…}`, is where a great many receivers live — and restores the
// enclosing scope afterwards so the closure's names do not leak out.
func (w *walker) walkFuncLiteral(lit *tree_sitter.Node, enclosingClass, enclosingFunc string) {
	saved := w.varTypes
	w.varTypes = make(map[string]binding, len(saved))
	for k, v := range saved {
		w.varTypes[k] = v
	}
	if params := lit.ChildByFieldName("parameters"); params != nil {
		bindParams(params, w.source, w.scope, w.varTypes)
	}
	w.walk(lit, enclosingClass, enclosingFunc)
	w.varTypes = saved
}

// noteGoVarType records a variable→type binding when the walk passes over a
// short_var_declaration or var_spec, feeding receiver-call resolution without a
// separate full-body traversal.
func (w *walker) noteGoVarType(node *tree_sitter.Node, nt string) {
	switch nt {
	case "short_var_declaration":
		left := node.ChildByFieldName("left")
		right := node.ChildByFieldName("right")
		if left != nil && right != nil {
			bindShortVar(left, right, w.source, w.scope, w.varTypes)
		}
	case "var_spec":
		bindVarSpec(node, w.source, w.scope, w.varTypes)
	case "range_clause":
		bindRange(node, w.source, w.scope, w.varTypes)
	}
}

// extractCall emits an UNRESOLVED calls edge. Target resolution (bare name →
// qualified node, receiver var → Type.method → qualified node) is deferred to
// the cross-file linker in package graph; here we only record the hints it needs.
//
// The hint says what SHAPE the call has, and the shape is decided by the operand
// alone — never by how much of its type this walk could infer:
//
//	foo()              bare      package-local function candidate
//	pkg.Func()         pkg       external (import path recorded)
//	pkg.Var.m()        pkg       external: the chain is rooted in an import
//	x.m(), x bound     recvType  x's declared or inferred type
//	x.m(), x unbound   recvVar   receiver of unknown type
//	a.b.m(), f().m()   recvVar   receiver of unknown type — SAME as x.m(),
//	                             plus chain/rootType/rootPkg when a is bound
//
// recvPkg is an ORIGIN qualifier on top of recvType/recvVar: the import path
// the receiver's type or value came from (w http.ResponseWriter; fs :=
// flag.NewFlagSet()). A receiver from another package can only be resolved in
// that package, and a receiver from a package outside the repo is external by
// construction — the linker must never guess either one against the caller's
// own packages.
//
// A chained receiver is unknown in exactly the way an unbound x is, so it gets
// the same shape hint; it must never degrade into a bare call, which the linker
// resolves by a different rule (package-local functions), and which would make
// `d.Resolver.UpdateByPaths()` look like a call to a free function named
// UpdateByPaths. What a chain adds is EVIDENCE: when its root variable is
// bound, the steps are recorded so the linker can follow them on the declared
// field and result types (see chain in EdgeInfo).
func (w *walker) extractCall(call *tree_sitter.Node, enclosingClass, enclosingFunc string) {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return
	}
	callName, operandNode := goCallName(fn, w.source)
	if callName == "" {
		return
	}

	source := w.path
	if enclosingFunc != "" {
		source = qualify(w.path, enclosingFunc, enclosingClass)
	}

	extra := map[string]string{"callName": callName}
	provisional := callName

	operand := ""
	if operandNode != nil {
		// One token, whatever the chain's layout in the source: the text is a
		// hint and, when unresolved, the edge's displayed target.
		operand = strings.Join(strings.Fields(operandNode.Utf8Text(w.source)), "")
	}
	simple := operandNode != nil && operandNode.Kind() == "identifier"
	root := goOperandRoot(operandNode, w.source)

	switch {
	case operandNode == nil:
		// Bare call foo(): a package-local function candidate.
		extra["bare"] = "1"
	case simple && isImportedPkg(operand, w.scope):
		// pkg.Func(): external; resolve pkg to its import path for readability.
		mod := w.scope.importMap[operand]
		extra["pkg"] = mod
		provisional = mod + "." + callName
	case simple && w.varTypes[operand] != binding{}:
		// x.method() with x bound → receiver-qualified resolution.
		b := w.varTypes[operand]
		if b.chain != "" || b.rootFunc != "" {
			// x holds a value derived from another binding (ixr, err :=
			// r.load()) or returned by a local helper (reg := newRegistry(t)):
			// the call is that chain, one step longer.
			extra["recvVar"] = operand
			provisional = operand + "." + callName
			setChain(extra, b)
			break
		}
		provisional = w.typedReceiver(extra, b.ref, operand, callName)
	case !simple && root != "" && isImportedPkg(root, w.scope):
		// pkg.Var.m() / pkg.F().m(): rooted in an import, so external by
		// construction, like pkg.Func().
		mod := w.scope.importMap[root]
		extra["pkg"] = mod
		provisional = mod + operand[len(root):] + "." + callName
	default:
		// Receiver of unknown type — unbound x, or a chain (a.b, f(), xs[i]).
		// Keep the receiver text for display. A chain whose root IS bound
		// carries the evidence the linker needs to follow it on declared types:
		// the root's binding and the steps (s.memory.Upsert → Server, "memory").
		// A chain it cannot follow (xs[i].m()) on a root of foreign origin still
		// carries that origin: a foreign type's methods return foreign values.
		extra["recvVar"] = operand
		provisional = operand + "." + callName
		if simple || root == "" {
			break
		}
		b := w.varTypes[root]
		if b == (binding{}) {
			break
		}
		segs, ok := goChain(operandNode, w.source)
		if !ok {
			if b.ref.Pkg != "" {
				extra["recvPkg"] = b.ref.Pkg
			}
			break
		}
		switch cb := chainFrom(b, segs); {
		case cb.chain != "" || cb.rootFunc != "":
			setChain(extra, cb)
		case cb.ref.Name != "":
			// The chain was only an element/key of a declared collection
			// (xs[i].m() on xs []*Item): a plain typed receiver after all.
			delete(extra, "recvVar")
			provisional = w.typedReceiver(extra, cb.ref, operand, callName)
		}
	}

	w.res.Edges = append(w.res.Edges, EdgeInfo{
		Kind: EdgeCalls, Source: source, Target: provisional,
		File: w.path, Line: w.line(call), Extra: extra,
	})
}

// typedReceiver records a receiver of declared type ref (recvType/recvPkg) and
// returns the provisional target. A collection (ref.Coll set) has no methods
// of its own and an origin-only ref (x := pkg.New()) has no type: both stay a
// receiver of unknown type, the latter with its origin.
func (w *walker) typedReceiver(extra map[string]string, ref TypeRef, operand, callName string) string {
	if ref.Pkg != "" {
		extra["recvPkg"] = ref.Pkg
	}
	if ref.Name == "" || ref.Coll != "" {
		extra["recvVar"] = operand
		return operand + "." + callName
	}
	extra["recvType"] = ref.Name
	if ref.Pkg != "" {
		return ref.Pkg + "." + ref.Name + "." + callName
	}
	return ref.Name + "." + callName
}

// setChain records a chain binding as the evidence hints on a calls edge.
func setChain(extra map[string]string, b binding) {
	if b.chain != "" {
		extra["chain"] = b.chain
	}
	if b.rootFunc != "" {
		extra["rootFunc"] = b.rootFunc
	}
	if b.rootType != "" {
		extra["rootType"] = b.rootType
	}
	if b.rootPkg != "" {
		extra["rootPkg"] = b.rootPkg
	}
}

// isImportedPkg reports whether operand names an imported package in this file.
func isImportedPkg(operand string, sc *scope) bool {
	_, ok := sc.importMap[operand]
	return ok
}
