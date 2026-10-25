// Command recall is a local code-intelligence substrate: it parses
// repositories, documentation and Kubernetes manifests into a semantic graph,
// snapshots that graph to disk, and serves retrieval over MCP so a coding agent
// can ask structural questions — who calls this, what breaks if I change it,
// why was this decided — instead of grepping.
//
// Everything is local. Embeddings go to an OpenAI-compatible endpoint you run
// (llama.cpp, Ollama, vLLM, LM Studio); nothing else leaves the machine.
//
// Which subcommands a build carries depends on CGO, because only the
// Tree-sitter parser needs a C toolchain:
//
//	static build (CGO_ENABLED=0)
//	  xray graph serve-snapshots   serve read tools from pre-built snapshots
//	  xray graph servicemap        build a service-dependency graph from a catalog
//	  xray graph update|status|build   drive a running server (editor/agent hooks)
//	  xray graph search <query>    search the snapshots from the shell, no server
//	  xray graph impact <qn>...    blast radius of changing symbols, read from a snapshot
//	  xray graph services <snap>   derive a service catalog from a code map
//	  xray graph install           drop skills + hook into a repo's .claude/
//	  xray graph dump <snapshot>   inspect a snapshot: summary, header, vectors, JSON
//	  xray graph keygen            generate an age identity for encrypted snapshots
//
//	cgo build (CGO_ENABLED=1 — the default for `make build`)
//	  everything above, plus:
//	  xray graph serve             parse + embed live, serve MCP
//	  xray graph snapshot          build offline .graph.gob.gz snapshots
//	  xray graph dump <repo-dir>   parse a tree and print/JSON its graph
//
// The parse modes are stubbed in the static build (parse_nocgo.go,
// dump_parse_nocgo.go) and exit with a message pointing at the CGO build,
// rather than silently missing.
package main

import (
	"fmt"
	"os"

	buildinfo "github.com/triageagent-dev/x-ray/v2/internal/version"
)

// usage lists the subcommands this build actually carries.
func usage() string {
	return `xray graph <subcommand>

  serve             parse + embed live and serve MCP        (CGO build only)
  snapshot          build offline graph snapshots           (CGO build only)
  dump <target>     inspect a graph: a repo dir is parsed (CGO build only), a
                    <label>.graph.gob.gz is read — -header, -vectors [-grep s],
                    -routing, -json, -limit n, -n components, -snapshot-key

  serve-snapshots   serve read tools from pre-built graph snapshots
  servicemap        build a service-dependency graph from a catalog
  update <file>...  incrementally re-index changed files in a running server
  status            print graph stats from a running server
  build             force a full reindex in a running server
  search <query>    hybrid graph search straight over the snapshots (no server;
                    needs the embedder) — -kind code|docs, -limit n, -project map,
                    -attr k=v, -order key[:desc], -gap, -snapshot-dir d,
                    -server URL to ask a running server instead
  impact <qn>...    blast radius of changing symbols, straight from the map's
                    snapshot (no server) — -snapshot-dir d ($XRAY_SNAPSHOT_DIR,
                    else ~/.xray-maps), -depth n, -max-nodes n, -snapshot-key
  services <snap>   derive a service map from a code map's snapshot (images,
                    deployments, gRPC and HTTP clients resolved through config)
                    — -format catalog|triage, -truth file, -o file
  install           install recall skills + file-watch hook into a repo
  keygen            generate an age identity (-out file) for encrypted snapshots

  Snapshots may be age-encrypted: readers take -snapshot-key <identity file>
  ($XRAY_SNAPSHOT_KEY), writers -snapshot-recipient <age1…> ($XRAY_SNAPSHOT_RECIPIENT).

  version           print the version and exit`
}

// runGraph dispatches `xray graph <sub>`.
func runGraph(argv []string) {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, usage())
		os.Exit(2)
	}

	sub, rest := argv[0], argv[1:]
	var err error
	switch sub {
	case "serve":
		err = runServe(rest)
	case "snapshot":
		err = runSnapshot(rest)
	case "dump":
		err = runDump(rest)
	case "servicemap":
		err = runServiceMap(rest)
	case "serve-snapshots":
		err = runServeSnapshots(rest)
	case "update", "status", "build":
		err = runClient(sub, rest)
	case "search":
		err = runSearch(rest)
	case "impact":
		err = runImpact(rest)
	case "services":
		err = runServices(rest)
	case "install":
		err = runInstall(rest)
	case "keygen":
		err = runKeygen(rest)
	case "version", "-v", "--version":
		fmt.Println(buildinfo.Version)
		return
	case "-h", "--help", "help":
		fmt.Println(usage())
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s\n", sub, usage())
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "graph "+sub+":", err)
		os.Exit(1)
	}
}
