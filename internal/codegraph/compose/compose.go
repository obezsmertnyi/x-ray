// Package compose ingests Docker Compose files and the .env files they read into
// the code graph, so how deployments address each other is graph data rather
// than something a reader has to go and look up.
//
// A compose service becomes a Deployment node; each environment key it passes
// and each service it depends_on becomes a references edge. An .env key becomes
// a Config node holding its value. Nothing here decides what calls what: the
// service map follows these edges and expands ${VAR} through Config nodes, and a
// value that names another deployment is the address one service uses to reach
// it — the same evidence whether the stack is Go, Kotlin or nginx.
//
// Like the Kubernetes scan, this is a line scan, not a YAML parse: compose files
// interpolate ${VAR:-default} into places a strict parser rejects, and a scan
// keeps every other service when one block is malformed.
//
// Values are credentials often enough to be treated as such: a key that names a
// password, secret, token or key keeps its name and loses its value, and a URL's
// user:password@ is dropped before anything is stored.
package compose

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

// Node kinds and edge hints.
const (
	KindDeployment = "Deployment"
	KindConfig     = "Config"

	// ExtraRef says what a Deployment's references edge is: RefEnv (Target is
	// "env:KEY", ExtraValue the inline value when the file sets one),
	// RefDependsOn (Target "deploy:NAME") or RefBuild (Target the Dockerfile
	// path as written, possibly ${VAR}).
	ExtraRef     = "composeRef"
	ExtraValue   = "value"
	RefEnv       = "env"
	RefDependsOn = "depends_on"
	RefBuild     = "build"
)

var (
	// Spring spells keys in kebab case (client-key, db-password), .env in
	// snake case; both are covered.
	reSecretKey = regexp.MustCompile(`(?i)pass(word)?|pwd|secret|token|credential|private|api[-_]?key|[-_.]key$`)
	reUserInfo  = regexp.MustCompile(`(://)[^/@\s]+@`)
	// A connection string keeps its address and loses its secrets: an Azure
	// Service Bus or storage string's SharedAccessKey=/AccountKey=, an ADO.NET
	// Password=. Redacting every connection-string key instead blanked the demo's
	// postgres://…@${POSTGRES_HOST}/ — the evidence two services use the DB.
	reSecretPart = regexp.MustCompile(`(?i)\b(SharedAccessKey|SharedAccessSignature|AccountKey|Password|Pwd)=[^;]*`)
	reService    = regexp.MustCompile(`^  ([A-Za-z0-9._-]+):\s*$`)
	reSection    = regexp.MustCompile(`^    ([a-z_]+):\s*(.*)$`)
	reListItem   = regexp.MustCompile(`^\s+-\s*(.+?)\s*$`)
	reMapItem    = regexp.MustCompile(`^      ([A-Za-z0-9._-]+):\s*(.*?)\s*$`)
	reDotEnv     = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*?)\s*$`)
)

// IsComposeFile reports whether a YAML file is a compose file: it has a
// top-level services: key.
func IsComposeFile(src []byte) bool {
	for _, line := range strings.Split(string(src), "\n") {
		if strings.TrimRight(line, " \r") == "services:" {
			return true
		}
	}
	return false
}

// IsDotEnv reports whether a path is an .env file (.env, .env.override, …).
func IsDotEnv(path string) bool {
	b := filepath.Base(path)
	return b == ".env" || strings.HasPrefix(b, ".env.")
}

// Redact strips credentials from a value: a secret-named key's value entirely,
// any URL's user:password@ otherwise.
func Redact(key, value string) string {
	if reSecretKey.MatchString(key) {
		return "<redacted>"
	}
	value = reSecretPart.ReplaceAllString(value, "$1=<redacted>")
	return reUserInfo.ReplaceAllString(value, "$1")
}

// ParseDotEnv turns an .env file into one Config node per key.
func ParseDotEnv(path string, src []byte) parser.Result {
	var res parser.Result
	for i, line := range strings.Split(string(src), "\n") {
		m := reDotEnv.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		value := m[2]
		// An unquoted value ends at an inline comment; a quoted one at its quote.
		if strings.HasPrefix(value, `"`) || strings.HasPrefix(value, `'`) {
			if j := strings.Index(value[1:], value[:1]); j >= 0 {
				value = value[1 : j+1]
			}
		} else if j := strings.Index(value, " #"); j >= 0 {
			value = strings.TrimSpace(value[:j])
		}
		value = Redact(m[1], value)
		res.Nodes = append(res.Nodes, parser.NodeInfo{
			Kind:      KindConfig,
			Name:      m[1],
			FilePath:  path,
			Language:  "dotenv",
			LineStart: i + 1,
			LineEnd:   i + 1,
			Content:   "config " + m[1] + " = " + value + " in " + path,
			Body:      value,
		})
	}
	return res
}

// Parse turns a compose file into Deployment nodes and their references edges.
func Parse(path string, src []byte) parser.Result {
	var res parser.Result
	lines := strings.Split(string(src), "\n")
	inServices := false

	type block struct {
		name       string
		start, end int
	}
	var blocks []block
	for i, line := range lines {
		trimmed := strings.TrimRight(line, " \r")
		if len(trimmed) > 0 && trimmed[0] != ' ' && trimmed[0] != '#' {
			inServices = trimmed == "services:"
			if len(blocks) > 0 && blocks[len(blocks)-1].end == 0 {
				blocks[len(blocks)-1].end = i
			}
			continue
		}
		if !inServices {
			continue
		}
		if m := reService.FindStringSubmatch(trimmed); m != nil {
			if len(blocks) > 0 && blocks[len(blocks)-1].end == 0 {
				blocks[len(blocks)-1].end = i
			}
			blocks = append(blocks, block{name: m[1], start: i})
		}
	}
	if len(blocks) > 0 && blocks[len(blocks)-1].end == 0 {
		blocks[len(blocks)-1].end = len(lines)
	}

	for _, b := range blocks {
		node := parser.NodeInfo{
			Kind:      KindDeployment,
			Name:      b.name,
			FilePath:  path,
			Language:  "compose",
			LineStart: b.start + 1,
			LineEnd:   b.end,
		}
		qn := node.Qualified()
		var env, deps []string
		var bodyLines []string
		section := ""
		for i := b.start; i < b.end; i++ {
			line := strings.TrimRight(lines[i], " \r")
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			edge := func(kind, target string, extra map[string]string) {
				extra[ExtraRef] = kind
				res.Edges = append(res.Edges, parser.EdgeInfo{
					Kind: parser.EdgeReferences, Source: qn, Target: target,
					File: path, Line: i + 1, Extra: extra,
				})
			}
			if m := reSection.FindStringSubmatch(line); m != nil {
				section = m[1]
				bodyLines = append(bodyLines, line)
				continue
			}
			if !strings.HasPrefix(line, "      ") {
				if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "    ") {
					section = ""
				}
			}
			switch section {
			case "environment":
				key, value, inline := "", "", false
				if m := reListItem.FindStringSubmatch(line); m != nil {
					k, v, ok := strings.Cut(strings.Trim(m[1], `"'`), "=")
					key, value, inline = k, v, ok
				} else if m := reMapItem.FindStringSubmatch(line); m != nil {
					key, value, inline = m[1], strings.Trim(m[2], `"'`), m[2] != ""
				}
				if key == "" {
					continue
				}
				extra := map[string]string{}
				if inline {
					value = Redact(key, value)
					extra[ExtraValue] = value
					bodyLines = append(bodyLines, "      - "+key+"="+value)
				} else {
					bodyLines = append(bodyLines, "      - "+key)
				}
				env = append(env, key)
				edge(RefEnv, "env:"+key, extra)
			case "depends_on":
				name := ""
				if m := reListItem.FindStringSubmatch(line); m != nil {
					name = m[1]
				} else if m := reMapItem.FindStringSubmatch(line); m != nil {
					name = m[1]
				}
				if name != "" {
					deps = append(deps, name)
					bodyLines = append(bodyLines, "      "+name)
					edge(RefDependsOn, "deploy:"+name, map[string]string{})
				}
			case "build":
				if m := reMapItem.FindStringSubmatch(line); m != nil && m[1] == "dockerfile" {
					bodyLines = append(bodyLines, line)
					edge(RefBuild, strings.Trim(m[2], `"'`), map[string]string{})
				}
			}
		}
		node.Body = strings.Join(bodyLines, "\n")
		var c strings.Builder
		c.WriteString("compose service " + b.name + " in " + path)
		if len(deps) > 0 {
			c.WriteString(". depends on: " + strings.Join(deps, ", "))
		}
		if len(env) > 0 {
			c.WriteString(". environment: " + strings.Join(env, ", "))
		}
		node.Content = c.String()
		res.Nodes = append(res.Nodes, node)
	}
	return res
}
