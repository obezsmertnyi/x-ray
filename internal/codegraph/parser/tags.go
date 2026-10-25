//go:build cgo

package parser

import (
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// extractWithTags is the generic (Tier 1) extractor: it runs a grammar's
// vendored tags.scm query to discover definitions (functions, classes, methods,
// types) and references (calls), emitting nodes + UNRESOLVED calls edges that
// the cross-file linker later resolves. It works for any language that ships a
// tags query, with no language-specific Go code.
//
// Definitions become nodes; @reference.call captures become calls edges from
// the enclosing definition (determined by line containment) to a bare target
// name. The linker resolves those bare names the same way it does for Go.
func extractWithTags(root *tree_sitter.Node, source []byte, lang, path string, spec *langSpec, res *Result) bool {
	query := tagsQuery(lang)
	if query == "" {
		return false
	}
	q, qerr := tree_sitter.NewQuery(spec.language(), query)
	if qerr != nil {
		return false
	}
	defer q.Close()

	qc := tree_sitter.NewQueryCursor()
	defer qc.Close()

	captureNames := q.CaptureNames()

	// Collect definitions with their line spans so references can be attributed
	// to the enclosing definition.
	var defs []tagDef
	var refs []EdgeInfo

	matches := qc.Matches(q, root, source)
	for {
		m := matches.Next()
		if m == nil {
			break
		}
		var name, defKind, refKind string
		var defNode, argsNode *tree_sitter.Node
		for _, cap := range m.Captures {
			capName := captureNames[cap.Index]
			switch {
			case capName == "name":
				name = cap.Node.Utf8Text(source)
			case capName == "args":
				n := cap.Node
				argsNode = &n
			case strings.HasPrefix(capName, "definition."):
				defKind = strings.TrimPrefix(capName, "definition.")
				n := cap.Node
				defNode = &n
			case strings.HasPrefix(capName, "reference."):
				refKind = strings.TrimPrefix(capName, "reference.")
				n := cap.Node
				defNode = &n
			}
		}
		if name == "" {
			continue
		}

		if defKind != "" && defNode != nil {
			kind := tagKindToNodeKind(defKind)
			if kind == "" {
				continue // skip constants/fields/modules we don't model as nodes
			}
			node := NodeInfo{
				Kind:       kind,
				Name:       name,
				ParentName: enclosingTypeName(defNode, source),
				FilePath:   path,
				Language:   lang,
				LineStart:  int(defNode.StartPosition().Row) + 1,
				LineEnd:    int(defNode.EndPosition().Row) + 1,
				// Full declaration source, the same as the Go extractor stores, so
				// get_graph_node can answer from the graph instead of pointing at a
				// file. This is the SAME span the line numbers above report, so body
				// and file:line never disagree. Capped, and display-first: nodeText
				// folds only a bounded slice of it in behind the signature.
				Body: capBody(defNode.Utf8Text(source)),
			}
			res.Nodes = append(res.Nodes, node)
			res.Edges = append(res.Edges, EdgeInfo{
				Kind: EdgeContains, Source: path, Target: node.Qualified(),
				File: path, Line: node.LineStart,
			})
			defs = append(defs, tagDef{qn: node.Qualified(), startLine: node.LineStart, endLine: node.LineEnd})
			continue
		}

		if refKind != "" && defNode != nil {
			// Only call/send references become calls edges; type/class references
			// are recorded as references for impact but kept distinct.
			edgeKind := EdgeReferences
			extra := map[string]string{"callName": name}
			switch refKind {
			case "call", "send":
				edgeKind = EdgeCalls
			case "import":
				// An import is the file's, not the enclosing definition's: it is
				// what every type name in the file resolves against.
				res.Edges = append(res.Edges, EdgeInfo{
					Kind: EdgeImports, Source: path, Target: name,
					File: path, Line: int(defNode.StartPosition().Row) + 1,
					Extra: map[string]string{"wildcard": boolFlag(isWildcardImport(defNode))},
				})
				continue
			case "annotation":
				extra = annotationExtra(name, argsNode, source)
			case "type", "class", "implementation":
				// A declared Java type name: the linker resolves it on the file's
				// own package and imports (graph.resolveTypeRef). Java only: in
				// TypeScript or Python a class in the same directory is not visible
				// without an import, so the package rule would guess.
				if lang == "java" {
					extra["typeName"] = name
				}
			}
			refs = append(refs, EdgeInfo{
				Kind:   edgeKind,
				Target: name,
				File:   path,
				Line:   int(defNode.StartPosition().Row) + 1,
				Extra:  extra,
			})
		}
	}
	switch lang {
	case "java":
		refs = append(refs, javaCalls(root, source, path)...)
	case "typescript", "python":
		for _, e := range scriptFacts(root, source, path, lang) {
			if e.Kind == EdgeImports {
				res.Edges = append(res.Edges, e) // the file's, not a definition's
				continue
			}
			refs = append(refs, e)
		}
	}
	// Attributed once every definition is known: a Java annotation is matched
	// before the declaration it sits on (both start on its line), so
	// attributing as matches arrive gave @FeignClient to the file, not the
	// interface it declares.
	for _, e := range refs {
		e.Source = enclosingDef(defs, e.Line, path)
		res.Edges = append(res.Edges, e)
	}
	return true
}

// tagKindToNodeKind maps a tags definition kind to our node kind taxonomy.
// Returns "" for kinds we don't represent as searchable nodes.
func tagKindToNodeKind(defKind string) string {
	switch defKind {
	case "function":
		return "Func"
	case "method":
		return "Func"
	case "class", "interface":
		return "Class"
	case "type", "struct", "enum":
		return "Type"
	default:
		return "" // constant, field, module, macro, etc.
	}
}

// tagDef is a definition's qualified name and line span, used to attribute
// references to their innermost enclosing definition.
type tagDef struct {
	qn        string
	startLine int
	endLine   int
}

// enclosingDef returns the qualified name of the definition whose line span
// contains the given line, or the file path if the reference is top-level.
func enclosingDef(defs []tagDef, line int, path string) string {
	best := path
	bestSpan := 1 << 30
	for _, d := range defs {
		if line >= d.startLine && line <= d.endLine {
			span := d.endLine - d.startLine
			if span < bestSpan { // innermost (smallest) enclosing def wins
				best = d.qn
				bestSpan = span
			}
		}
	}
	return best
}

func annotationExtra(name string, args *tree_sitter.Node, source []byte) map[string]string {
	extra := map[string]string{ExtraAnnotation: name}
	if args == nil {
		return extra
	}
	for i := uint(0); i < args.NamedChildCount(); i++ {
		c := args.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Kind() == "element_value_pair" {
			k, v := c.ChildByFieldName("key"), c.ChildByFieldName("value")
			if k != nil && v != nil {
				extra[ExtraArgPrefix+k.Utf8Text(source)] = annotationValue(v, source)
			}
			continue
		}
		if c.Kind() != "comment" {
			extra[ExtraArgPrefix+"value"] = annotationValue(c, source)
		}
	}
	return extra
}

func annotationValue(v *tree_sitter.Node, source []byte) string {
	switch v.Kind() {
	case "string_literal":
		return strings.Trim(v.Utf8Text(source), `"`)
	case "element_value_array_initializer":
		var parts []string
		for i := uint(0); i < v.NamedChildCount(); i++ {
			if c := v.NamedChild(i); c != nil && c.Kind() != "comment" {
				parts = append(parts, annotationValue(c, source))
			}
		}
		return strings.Join(parts, ",")
	}
	return v.Utf8Text(source)
}

func isWildcardImport(decl *tree_sitter.Node) bool {
	for i := uint(0); i < decl.ChildCount(); i++ {
		if c := decl.Child(i); c != nil && c.Kind() == "asterisk" {
			return true
		}
	}
	return false
}

func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return ""
}

// typeContainers are the node kinds that declare a type whose members a
// definition inside belongs to, with the field naming the type.
var typeContainers = map[string]string{
	"class_declaration":          "name", // Java, C#, TypeScript, PHP
	"abstract_class_declaration": "name", // TypeScript
	"interface_declaration":      "name", // Java, C#, TypeScript, PHP
	"enum_declaration":           "name", // Java, C#
	"record_declaration":         "name", // Java, C#
	"struct_declaration":         "name", // C#
	"trait_declaration":          "name", // PHP
	"class_definition":           "name", // Python
	"class":                      "name", // Ruby, JavaScript class expression
	"module":                     "name", // Ruby
	"impl_item":                  "type", // Rust
	"trait_item":                 "name", // Rust
	"object_creation_expression": "",     // Java anonymous class: stop, no name
}

// functionBoundaries end the search for an enclosing type: a def nested in a
// Python method or a TypeScript function is not a member of the class around it.
var functionBoundaries = map[string]bool{
	"function_definition":  true, // Python
	"function_declaration": true, // TypeScript, JavaScript
	"function_expression":  true,
	"arrow_function":       true,
	"method_definition":    true,
}

// enclosingTypeName is the innermost type a definition is declared in, or "".
//
// Without it every method of a tags-extracted language was keyed file::name:
// two classes in one file with a method of the same name collided and the
// graph kept only the first (a large monorepo lost 110 Java symbols), and the
// linker's Type.method table was empty for all of them, so no call on a typed
// receiver could resolve. The title a method is embedded under becomes
// Class.method, so these nodes re-embed once.
func enclosingTypeName(def *tree_sitter.Node, source []byte) string {
	if def == nil {
		return ""
	}
	for n := def.Parent(); n != nil; n = n.Parent() {
		if functionBoundaries[n.Kind()] {
			return "" // a function nested in a function belongs to no class
		}
		field, ok := typeContainers[n.Kind()]
		if !ok {
			continue
		}
		if field == "" {
			return ""
		}
		if name := n.ChildByFieldName(field); name != nil {
			return name.Utf8Text(source)
		}
		return ""
	}
	return ""
}
