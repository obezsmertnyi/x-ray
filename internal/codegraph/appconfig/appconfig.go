// Package appconfig ingests an application's own configuration files — Spring
// Boot's application*.yml / application*.properties and bootstrap*.yml — as
// Config nodes, one per flattened key (a context path, a service's module URL).
//
// They are the same kind of node an .env key becomes (package compose), for the
// same reason: code names an address as ${key} (@FeignClient(url =
// "${some-service.module.url}"), CART_ADDR=${CART_ADDR}) and the value that
// key holds is where the call goes. With both in the graph, one expansion rule
// serves a Spring monorepo and a compose stack alike. Without them, a Spring
// service's clients were only names, and two clients of one URL under two names
// became two services.
//
// Like compose and k8s, the YAML is line-scanned, not parsed: `url : http://…`
// with a space before the colon, ${…} placeholders and profile files that are
// half comments all stay readable, and one bad block costs its own keys only.
// Values go through compose.Redact before they are stored.
package appconfig

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/compose"
	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// KindConfig is the node kind, shared with .env keys.
const KindConfig = compose.KindConfig

var (
	reConfigFile = regexp.MustCompile(`^(?:application|bootstrap)(?:[-.][A-Za-z0-9_.-]+)?\.(?:ya?ml|properties)$`)
	reYAMLKey    = regexp.MustCompile(`^( *)([^\s#:'"][^:#]*?|'[^']*'|"[^"]*")\s*:(?:\s+(.*?))?\s*$`)
	reYAMLItem   = regexp.MustCompile(`^( *)-\s+(.*?)\s*$`)
	rePropLine   = regexp.MustCompile(`^\s*([^#!\s=:][^=:]*?)\s*[=:]\s*(.*?)\s*$`)
)

// IsConfigFile reports whether a path is an application config file.
func IsConfigFile(path string) bool {
	return reConfigFile.MatchString(filepath.Base(path)) || IsTSConfig(path)
}

// IsTSConfig reports whether a path is a TypeScript/JavaScript project config
// (tsconfig.json, tsconfig.*.json, jsconfig.json): where compilerOptions.paths
// maps an import alias (@/*) onto the directories it names.
func IsTSConfig(path string) bool {
	b := filepath.Base(path)
	return b == "jsconfig.json" || (strings.HasPrefix(b, "tsconfig") && strings.HasSuffix(b, ".json"))
}

// Parse turns an application config file into Config nodes.
func Parse(path string, src []byte) parser.Result {
	switch {
	case IsTSConfig(path):
		return parseJSONC(path, src)
	case strings.HasSuffix(path, ".properties"):
		return parseProperties(path, src)
	}
	return parseYAML(path, src)
}

var (
	reJSONComment   = regexp.MustCompile(`(?s)("(?:[^"\\]|\\.)*")|//[^\n]*|/\*.*?\*/`)
	reTrailingComma = regexp.MustCompile(`,(\s*[}\]])`)
)

// parseJSONC flattens a JSON-with-comments file (tsconfig allows comments and
// trailing commas) into Config nodes: compilerOptions.paths.@/* = ./*. Arrays
// of scalars are joined by ",". A file that still does not parse yields
// nothing rather than a guess.
func parseJSONC(path string, src []byte) parser.Result {
	clean := reJSONComment.ReplaceAllStringFunc(string(src), func(m string) string {
		if strings.HasPrefix(m, `"`) {
			return m
		}
		return ""
	})
	clean = reTrailingComma.ReplaceAllString(clean, "$1")
	var doc map[string]any
	if err := json.Unmarshal([]byte(clean), &doc); err != nil {
		return parser.Result{}
	}
	var res parser.Result
	var flatten func(prefix string, v any)
	flatten = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, k := range sortedKeys(x) {
				key := k
				if prefix != "" {
					key = prefix + "." + k
				}
				flatten(key, x[k])
			}
		case []any:
			var items []string
			for _, it := range x {
				if s, ok := it.(string); ok {
					items = append(items, s)
				}
			}
			if len(items) > 0 {
				res.Nodes = append(res.Nodes, node(path, prefix, strings.Join(items, ","), 1))
			}
		case string:
			res.Nodes = append(res.Nodes, node(path, prefix, x, 1))
		case bool, float64:
			res.Nodes = append(res.Nodes, node(path, prefix, fmt.Sprint(x), 1))
		}
	}
	flatten("", doc)
	return res
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func node(path, key, value string, line int) parser.NodeInfo {
	value = compose.Redact(key, value)
	return parser.NodeInfo{
		Kind:      KindConfig,
		Name:      key,
		FilePath:  path,
		Language:  "appconfig",
		LineStart: line,
		LineEnd:   line,
		Content:   "config " + key + " = " + value + " in " + path,
		Body:      value,
	}
}

func parseProperties(path string, src []byte) parser.Result {
	var res parser.Result
	for i, line := range strings.Split(string(src), "\n") {
		m := rePropLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		res.Nodes = append(res.Nodes, node(path, m[1], m[2], i+1))
	}
	return res
}

// parseYAML flattens nested mappings into dotted keys by indentation. A list
// of scalars under a key becomes one node holding the items joined by ","
// (spring.config.import: [a.yml, b.yml]); lists of mappings are skipped, as are
// block scalars (| and >), whose lines are not keys. A "---" starts a new
// document, as a profile section does in one file.
func parseYAML(path string, src []byte) parser.Result {
	var res parser.Result
	type frame struct {
		indent int
		key    string
	}
	var stack []frame
	blockIndent := -1 // inside a block scalar deeper than this indent
	listKey, listLine := "", 0
	var listItems []string
	flush := func() {
		if listKey != "" && len(listItems) > 0 {
			res.Nodes = append(res.Nodes, node(path, listKey, strings.Join(listItems, ","), listLine))
		}
		listKey, listItems = "", nil
	}
	for i, raw := range strings.Split(string(src), "\n") {
		line := strings.TrimRight(raw, " \r\t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if blockIndent >= 0 {
			if indent > blockIndent {
				continue
			}
			blockIndent = -1
		}
		if trimmed == "---" {
			flush()
			stack = nil
			continue
		}
		if m := reYAMLItem.FindStringSubmatch(line); m != nil {
			if listKey != "" && !strings.Contains(m[2], ": ") {
				listItems = append(listItems, unquote(m[2]))
			}
			continue
		}
		m := reYAMLKey.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		flush()
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		key := unquote(strings.TrimSpace(m[2]))
		full := key
		if len(stack) > 0 {
			full = stack[len(stack)-1].key + "." + key
		}
		value := stripComment(m[3])
		switch {
		case value == "":
			stack = append(stack, frame{indent: indent, key: full})
			listKey, listLine = full, i+1
		case value == "|" || value == ">" || strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">"):
			blockIndent = indent
		default:
			res.Nodes = append(res.Nodes, node(path, full, unquote(value), i+1))
		}
	}
	flush()
	return res
}

// stripComment cuts an unquoted value at " #".
func stripComment(v string) string {
	if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, `'`) {
		return v
	}
	if j := strings.Index(v, " #"); j >= 0 {
		return strings.TrimSpace(v[:j])
	}
	return v
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}
