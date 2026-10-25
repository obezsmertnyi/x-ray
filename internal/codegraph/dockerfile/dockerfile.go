// Package dockerfile ingests Dockerfiles as Image nodes, with a references
// edge for every local path the image copies in.
//
// What an image copies is what it runs. A Gradle monorepo's service image
// copies its own module and the shared modules it builds against (COPY
// of the service's module, COPY of the shared modules), which is the evidence that a
// shared module is a library of that service and not a service of its own —
// and that a client declared in the library is code the service runs. A
// compose service's build names its Dockerfile, so the same node ties a
// deployment to the source it was built from.
package dockerfile

import (
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/triageagent-dev/x-ray/v2/internal/codegraph/parser"
)

const (
	KindImage = "Image"

	// ExtraRef marks an Image's references edge: RefCopy, Target "path:<src>"
	// with the source as written (relative to the build context, which the
	// Dockerfile does not name).
	ExtraRef = "dockerRef"
	RefCopy  = "copy"
)

var reCopy = regexp.MustCompile(`(?i)^\s*(?:COPY|ADD)\s+(.*)$`)

// IsDockerfile reports whether a path is a Dockerfile: Dockerfile,
// Dockerfile.<variant> or <name>.Dockerfile.
func IsDockerfile(p string) bool {
	b := filepath.Base(p)
	return b == "Dockerfile" || strings.HasPrefix(b, "Dockerfile.") || strings.HasSuffix(b, ".Dockerfile")
}

// Parse turns a Dockerfile into one Image node named after the directory
// holding it, and its COPY/ADD sources into references edges. Copies from
// another stage (--from=) and remote URLs are not local sources and are skipped.
func Parse(p string, src []byte) parser.Result {
	name := path.Base(path.Dir(filepath.ToSlash(p)))
	if name == "." || name == "/" {
		name = filepath.Base(p)
	}
	lines := strings.Split(string(src), "\n")
	n := parser.NodeInfo{
		Kind: KindImage, Name: name, FilePath: p, Language: "dockerfile",
		LineStart: 1, LineEnd: len(lines),
	}
	qn := n.Qualified()
	var res parser.Result
	var copies []string
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		for strings.HasSuffix(line, `\`) && i+1 < len(lines) {
			i++
			line = strings.TrimSuffix(line, `\`) + " " + strings.TrimRight(lines[i], "\r")
		}
		m := reCopy.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		fields := strings.Fields(m[1])
		var args []string
		skip := false
		for _, f := range fields {
			if strings.HasPrefix(f, "--") {
				if strings.HasPrefix(f, "--from") {
					skip = true
				}
				continue
			}
			args = append(args, f)
		}
		if skip || len(args) < 2 || strings.HasPrefix(args[0], "[") {
			continue
		}
		for _, s := range args[:len(args)-1] {
			if strings.Contains(s, "://") {
				continue
			}
			copies = append(copies, s)
			res.Edges = append(res.Edges, parser.EdgeInfo{
				Kind: parser.EdgeReferences, Source: qn, Target: "path:" + s,
				File: p, Line: i + 1, Extra: map[string]string{ExtraRef: RefCopy},
			})
		}
	}
	n.Content = "image " + name + " built by " + p
	if len(copies) > 0 {
		n.Content += ". copies: " + strings.Join(copies, ", ")
	}
	n.Body = n.Content
	res.Nodes = append(res.Nodes, n)
	return res
}
