// Package k8s ingests Kubernetes manifests (Helm chart templates, CRDs, raw
// YAML) into the code graph. Each YAML document ("---" separated)
// becomes a Manifest node titled "Kind/name"; container images and
// secret/configMap references become reference edges so the graph can answer
// "what uses this secret".
//
// Manifests are NOT Tree-sitter parsed: Helm templating ({{ .Values… }}) is not
// valid YAML and would break a strict parser. Instead a resilient line scan
// pulls top-level kind / metadata.name / metadata.namespace and known reference
// fields, tolerating template expressions.
package k8s

import (
	"regexp"
	"strings"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// KindManifest is the node kind for a Kubernetes resource.
const KindManifest = "Manifest"

// indentOf returns the number of leading spaces on a line.
func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

var (
	reKind      = regexp.MustCompile(`^kind:\s*(\S+)`)
	reName      = regexp.MustCompile(`^\s{2}name:\s*(.+?)\s*$`) // metadata.name at 2-space indent
	reNamespace = regexp.MustCompile(`^\s{2}namespace:\s*(.+?)\s*$`)
	reImage     = regexp.MustCompile(`^\s*-?\s*image:\s*(.+?)\s*$`)
	// Secret / ConfigMap references in env, volumes, envFrom, etc.
	reSecretName  = regexp.MustCompile(`^\s*secretName:\s*(.+?)\s*$`)
	reRefName     = regexp.MustCompile(`(secretKeyRef|configMapKeyRef|secretRef|configMapRef):`)
	reNestedName  = regexp.MustCompile(`^\s*name:\s*(.+?)\s*$`)
	reTemplateVal = regexp.MustCompile(`\{\{.*?\}\}`)
)

// Parse turns one YAML file's bytes into Manifest nodes + reference edges.
func Parse(path string, source []byte) parser.Result {
	res := parser.Result{}
	lines := strings.Split(string(source), "\n")

	// Split into documents on "---" separators, tracking line offsets.
	type docRange struct{ start, end int }
	var ranges []docRange
	start := 0
	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			if i > start {
				ranges = append(ranges, docRange{start, i})
			}
			start = i + 1
		}
	}
	if start < len(lines) {
		ranges = append(ranges, docRange{start, len(lines)})
	}

	for _, r := range ranges {
		doc := lines[r.start:r.end]
		if node, edges := parseDoc(path, doc, r.start); node != nil {
			res.Nodes = append(res.Nodes, *node)
			res.Edges = append(res.Edges, edges...)
		}
	}
	return res
}

// parseDoc extracts a single resource node + its reference edges. lineOffset is
// the 0-based index of the doc's first line within the file.
func parseDoc(path string, doc []string, lineOffset int) (*parser.NodeInfo, []parser.EdgeInfo) {
	var kind, name, namespace string
	var refs []ref
	pendingRef := "" // a *KeyRef:/...Ref: seen; the next name: is its target

	for _, line := range doc {
		if kind == "" {
			if m := reKind.FindStringSubmatch(line); m != nil {
				kind = clean(m[1])
				continue
			}
		}
		// metadata.name / namespace are at exactly 2-space indent (top-level meta).
		if name == "" {
			if m := reName.FindStringSubmatch(line); m != nil && indentOf(line) == 2 {
				name = clean(m[1])
			}
		}
		if namespace == "" {
			if m := reNamespace.FindStringSubmatch(line); m != nil && indentOf(line) == 2 {
				namespace = clean(m[1])
			}
		}
		// References: container images.
		if m := reImage.FindStringSubmatch(line); m != nil {
			if v := clean(m[1]); v != "" {
				refs = append(refs, ref{kind: "image", name: v})
			}
		}
		// secretName: directly names a Secret.
		if m := reSecretName.FindStringSubmatch(line); m != nil {
			if v := clean(m[1]); v != "" {
				refs = append(refs, ref{kind: "secret", name: v})
			}
		}
		// *KeyRef:/secretRef:/configMapRef: → the next nested name: is the target.
		if loc := reRefName.FindStringSubmatch(line); loc != nil {
			switch {
			case strings.Contains(loc[1], "secret"):
				pendingRef = "secret"
			default:
				pendingRef = "configmap"
			}
			continue
		}
		if pendingRef != "" {
			if m := reNestedName.FindStringSubmatch(line); m != nil {
				if v := clean(m[1]); v != "" {
					refs = append(refs, ref{kind: pendingRef, name: v})
				}
				pendingRef = ""
			}
		}
	}

	if kind == "" {
		return nil, nil // not a recognisable resource (e.g. values.yaml, Chart.yaml)
	}
	if name == "" {
		name = "unnamed"
	}

	title := kind + "/" + name
	node := &parser.NodeInfo{
		Kind:      KindManifest,
		Name:      title,
		FilePath:  path,
		Language:  "yaml",
		LineStart: lineOffset + 1,
		LineEnd:   lineOffset + len(doc),
		Content:   manifestContent(kind, name, namespace, refs), // embedding/preview
		Body:      strings.Join(doc, "\n"),                      // full manifest YAML
	}
	qn := node.Qualified()

	var edges []parser.EdgeInfo
	for _, rf := range refs {
		edges = append(edges, parser.EdgeInfo{
			Kind:   parser.EdgeReferences,
			Source: qn,
			Target: rf.kind + ":" + rf.name,
			File:   path,
			Line:   node.LineStart,
			Extra:  map[string]string{"refKind": rf.kind},
		})
	}
	return node, edges
}

type ref struct {
	kind string // image, secret, configmap
	name string
}

// manifestContent is the embeddable text for a manifest node.
func manifestContent(kind, name, namespace string, refs []ref) string {
	var b strings.Builder
	b.WriteString("Kubernetes ")
	b.WriteString(kind)
	b.WriteString(" named ")
	b.WriteString(name)
	if namespace != "" {
		b.WriteString(" in namespace ")
		b.WriteString(namespace)
	}
	if len(refs) > 0 {
		b.WriteString(". references: ")
		var parts []string
		for _, r := range refs {
			parts = append(parts, r.kind+" "+r.name)
		}
		b.WriteString(strings.Join(parts, ", "))
	}
	return b.String()
}

// clean strips quotes and collapses Helm template expressions to a placeholder
// so a templated name like "{{ .Release.Name }}-secret" stays meaningful.
func clean(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	s = reTemplateVal.ReplaceAllString(s, "${}")
	return strings.TrimSpace(s)
}
