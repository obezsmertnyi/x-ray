// Package parser turns source files into structural graph nodes and edges using
// Tree-sitter. It follows the cursor-walk + node-type-table strategy proven by
// an earlier code-review graph, ported to Go.
package parser

// TypeRef names a declared type: its bare name and the import path of the
// package that declares it ("" for the declaring file's own package). Struct
// field types and single result types are recorded this way so the linker can
// follow x.field.m() and x.method().m() chains on declared evidence instead of
// guessing by name.
type TypeRef struct {
	Name string
	Pkg  string
	// Collections: a []T, [N]T, chan T or map[K]T declaration records the
	// element T as Name/Pkg with Coll set ("slice", "chan", "map") and a map's
	// key as Key — what an index expression or `for k, v := range x` yields.
	// Only one level deep: [][]T or map[K][]T records nothing.
	Coll string
	Key  *TypeRef
}

// NodeInfo is an extracted structural entity: a file, function, class, type,
// test, or documentation Doc/Section.
type NodeInfo struct {
	Kind       string // File, Func, Class, Type, Test, Doc, Section
	Name       string
	FilePath   string
	Language   string
	LineStart  int
	LineEnd    int
	ParentName string // enclosing class/type, or doc title for Section nodes
	Params     string
	ReturnType string
	IsTest     bool
	// Content is the lean EMBEDDING/preview text (what semantic search matches on
	// and what a search hit previews). Set for Doc/Section and Manifest nodes;
	// empty for code nodes (their EMBEDDING text is synthesized from the
	// signature — see nodeText — so the body never dilutes the vector).
	Content string
	// Attrs are caller-supplied facets carried by catalog entries (kind-specific
	// key/value pairs such as team, tier, owner). Empty for parsed code nodes —
	// nothing in the extractors sets it. Searchable via index.AttrFilter.
	Attrs map[string]string
	// Body is the node's FULL, clean content (no embedding prefix), returned by
	// get_code_node and shown in the UI node panel. Held in memory — this is the
	// graph's own data, not a file read. Set for Section/Manifest nodes AND for
	// code nodes (Func/Test/Class/Type carry their full declaration source, capped
	// at maxCodeBodyChars); empty for Doc nodes (reassembled from sections) and
	// File nodes. Display-only: it is NOT what gets embedded.
	Body string
	// Fields maps a struct's field names to their declared types — embedded
	// fields under their bare type name — for Class nodes that are structs.
	// Result is a function's single named result type; zero when it returns
	// nothing, several values, or an unnamed type. Both feed the linker's chain
	// rule and are absent from snapshots written before they existed (gob
	// leaves them zero, and those edges simply stay external).
	Fields map[string]TypeRef
	Result TypeRef
}

// Qualified returns the node's stable identity: file::Parent.Name or file::Name.
// File and Doc nodes are keyed by their path alone.
func (n NodeInfo) Qualified() string {
	if n.Kind == "File" || n.Kind == "Doc" {
		return n.FilePath
	}
	if n.ParentName != "" {
		return n.FilePath + "::" + n.ParentName + "." + n.Name
	}
	return n.FilePath + "::" + n.Name
}

// EdgeInfo is a directed relationship between two qualified names.
//
// For calls edges, Target is left UNRESOLVED at parse time (a bare symbol or
// receiver-qualified name); the cross-file linker in package graph rewrites it
// to a qualified node using the hints in Extra. The hint keys are:
//
//	"callName"   the method/function name being called
//	"recvType"   the declared or inferred receiver type for x.method() calls
//	"recvVar"    the receiver text when its type is unknown — x, a.b, f(), xs[i]
//	"recvPkg"    the import path the receiver's type or value came from, when
//	             it is not the caller's own package (w http.ResponseWriter;
//	             fs := flag.NewFlagSet(); reg *engine.Registry)
//	"pkg"        the resolved import path when the call is pkg.Func() or is a
//	             chain rooted in an import, pkg.Var.m() ("" if none)
//	"bare"       "1" when the call is an unqualified foo() (package-local candidate)
//	"chain"      for a chained receiver whose root variable is bound: the steps
//	             from the root outward, "memory" for s.memory.m(), "Index()"
//	             for r.Index().m(), "a.b()" for x.a.b().m(), "[]" for an
//	             element (xs[i], a range value) and "[k]" for a map key (a
//	             range key); with the root's own binding in "rootType" /
//	             "rootPkg" — or, for a value a package-local function
//	             returned (reg := newRegistry(t)), that function in
//	             "rootFunc", typed by its declared result at link time. Absent
//	             when a step cannot be followed on declared types (a type
//	             assertion, a func call on a non-selector)
//
// Exactly one of recvType / recvVar / pkg / bare is set per calls edge: the
// hint names the call's SHAPE, and the linker picks its rule from it. recvPkg
// is an optional ORIGIN qualifier on recvType/recvVar; chain with
// rootType/rootPkg or rootFunc is optional EVIDENCE on a recvVar the linker
// can follow step by step.
type EdgeInfo struct {
	Kind   string // calls, imports, inherits, implements, contains, references
	Source string // qualified name or file path
	Target string // qualified name, bare symbol, or module path
	File   string
	Line   int
	Extra  map[string]string
}

// Result is the output of parsing one file.
type Result struct {
	Nodes []NodeInfo
	Edges []EdgeInfo
}

// Edge relation kinds. This vocabulary is the graph's contract: parsers emit
// these strings and every query filters on them.
const (
	EdgeCalls      = "calls"
	EdgeImports    = "imports"
	EdgeInherits   = "inherits"
	EdgeImplements = "implements"
	EdgeContains   = "contains"
	EdgeReferences = "references"
)

// ExtraAnnotation names the annotation a references edge records; its
// arguments ride along as ExtraArgPrefix+key ("arg.url"), an unnamed single
// argument under "arg.value". A string is stored unquoted, an array of strings
// joined by ",", anything else as written (CoreFeignConfiguration.class).
// Untagged, unlike the extractor that sets them: CGO-free readers of a
// snapshot (xray-memory services) read these edges too.
const (
	ExtraAnnotation = "annotation"
	ExtraArgPrefix  = "arg."
)

// ExtraJavaRecv marks a calls edge whose recvType is a Java type name, to be
// resolved through the file's imports and package (graph/typelink.go) rather
// than as a Go package-local type.
const ExtraJavaRecv = "javaRecv"

// ExtraJavaSelf marks a Java call with no receiver or this: foo(), this.foo().
// Unlike x.foo(), a name the class does not declare can be a static import
// (when, assertEquals), so it does not fall back to the class.
const ExtraJavaSelf = "javaSelf"

// ExtraArgURLParam0 marks a calls edge whose first argument is exactly the
// enclosing method's own first parameter, unresolved on its own. A
// hand-written HTTP client wrapper (RestTemplate, WebServiceTemplate) is
// usually an interface plus one impl that just forwards what it was given,
// the shape a declared client (@FeignClient and peers) does not need: there
// the address is written at the annotation itself. The service map (which
// carries no CGO parser and cannot re-walk the source) follows this marker
// back through the method's own callers instead, one call frame at a time,
// until it reaches a literal or an ExtraArgPrefix+"url" a caller resolved
// directly (a field's Spring @Value("${...}") placeholder, expanded through
// Config nodes exactly like a Feign url).
const ExtraArgURLParam0 = "argUrlParam0"

// FactsVersion numbers what the extractors put in a graph. Bump it when an
// extractor starts emitting a fact an existing snapshot cannot hold — a new node
// kind, a new edge or hint — so a reader of an older snapshot can say so
// instead of answering with less: `services` on a snapshot built before Image
// and Config nodes silently found fewer services, and a server seeded from
// one kept serving the old facts until the next cold build.
//
//	0  before the number existed
//	1  Image, Config (application config) and Deployment nodes; Java
//	   annotations, declared types, imports and receiver-typed calls; methods
//	   of tags-extracted languages carry their class (ParentName)
//	2  TypeScript and Python import bindings and receiver-hinted calls;
//	   tsconfig/jsconfig as Config nodes (compilerOptions.paths)
const FactsVersion = 2

// Hints on TypeScript and Python facts (parser/script_calls.go, resolved in
// graph/scriptlink.go). An imports edge with ExtraScriptImport binds
// ExtraBinding in the importing file to ExtraImported from the module in
// Target: a symbol name, "default", "*" (a namespace or wildcard) or "" (the
// module itself). A calls edge with ExtraScriptCall carries bare, recvType or
// recvVar, and ExtraSelfCall when the receiver is this/self.
const (
	ExtraScriptImport = "scriptImport"
	ExtraBinding      = "binding"
	ExtraImported     = "imported"
	ExtraScriptCall   = "scriptCall"
	ExtraSelfCall     = "selfCall"
)
