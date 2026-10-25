package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// runClient implements the thin CLI subcommands that drive a running serve
// process over HTTP, so editor/agent hooks can keep the live graph current:
//
//	xray graph update [-server URL] <file>...   incremental re-index of changed files
//	xray graph status [-server URL]             print graph stats as JSON
//	xray graph build  [-server URL]             force a full reindex
//
// The default server URL matches the default serve --addr; serve-snapshots
// listens on :8085, so give -server for it. search (search.go) reads snapshots
// itself and comes here only with -server: GET /search, which both servers
// register over mcpserver.SearchDirect/SearchDirectIndex — the same path
// search_graph takes — so the ranked pointers are identical either way.
func runClient(cmd string, argv []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	server := fs.String("server", "http://localhost:8084", "base URL of the running `xray graph serve` (or serve-snapshots) process")
	_ = fs.Parse(argv)

	switch cmd {
	case "status":
		return clientGet(*server + "/status")
	case "build":
		return clientPost(*server+"/reindex", nil)
	case "update":
		files := fs.Args()
		if len(files) == 0 {
			// Nothing to do (hook fired on a non-file event) — succeed quietly so
			// the hook never blocks the editor/agent.
			return nil
		}
		// Resolve relative paths against the CLIENT's cwd before sending: the
		// server's cwd differs (it runs from the serve cwd), so a bare relative
		// path would otherwise resolve against the wrong root and match nothing.
		abs := make([]string, 0, len(files))
		for _, f := range files {
			if a, err := filepath.Abs(f); err == nil {
				abs = append(abs, a)
			} else {
				abs = append(abs, f)
			}
		}
		body, _ := json.Marshal(map[string]any{"paths": abs})
		return clientPost(*server+"/update", body)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// searchOpts are the /search query parameters beyond q.
type searchOpts struct {
	kind, project, attr, order string
	limit                      int
	gap                        bool
}

// searchURL builds GET /search for q, leaving out every parameter at its
// default so the server's own defaults decide, as they do for the MCP tools.
func searchURL(server, q string, o searchOpts) string {
	v := url.Values{"q": {q}}
	if o.kind != "" && o.kind != "all" {
		v.Set("kind", o.kind)
	}
	if o.limit > 0 {
		v.Set("limit", strconv.Itoa(o.limit))
	}
	if o.project != "" {
		v.Set("project", o.project)
	}
	if o.attr != "" {
		v.Set("attr", o.attr)
	}
	if o.order != "" {
		v.Set("order", o.order)
	}
	if o.gap {
		v.Set("cutoff", "gap")
	}
	return strings.TrimRight(server, "/") + "/search?" + v.Encode()
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func clientGet(url string) error {
	return clientDo(http.MethodGet, url, nil)
}

func clientPost(url string, body []byte) error {
	return clientDo(http.MethodPost, url, body)
}

// clientDo sends $XRAY_AUTH_TOKEN as a bearer token when it is set, so the
// same hook works against a server started with -auth-token-file.
func clientDo(method, url string, body []byte) error {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := os.Getenv(envAuthToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return connErr(err)
	}
	defer resp.Body.Close()
	return relayBody(resp)
}

func relayBody(resp *http.Response) error {
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(b))
	}
	_, _ = os.Stdout.Write(b)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		fmt.Println()
	}
	return nil
}

// connErr makes "server not running" a soft, clearly-worded failure — hooks
// should tolerate the server being down without erroring the whole tool call.
func connErr(err error) error {
	return fmt.Errorf("cannot reach the xray graph server (is `serve` running?): %w", err)
}
