#!/usr/bin/env bash
# PostToolUse hook: keep the running X-Ray code graph current.
#
# Two cases, both driven by the Claude Code hook payload (JSON on stdin):
#   1. Edit / Write / MultiEdit — re-index the file(s) the tool touched.
#   2. Bash `git commit`        — re-index the files changed in the new commit,
#      so the graph reflects committed state (the "on commit" cycle).
#
# Talks MCP, not the /update admin route. That route exists ONLY in `xray graph
# serve` (Mode A); `xray graph serve-snapshots` (Mode B, the usual local setup)
# returns 404 for it, so the old CLI-based hook was a silent no-op there —
# `|| true` hid it. The update_code_graph MCP tool is registered in BOTH modes,
# so this works everywhere and needs no binary on PATH.
#
# Never blocks the agent: on any failure it exits 0 quietly. Configure the
# server via XRAY_SERVER, and XRAY_AUTH_TOKEN when it runs with -auth-token-file.
set -uo pipefail

SERVER="${XRAY_SERVER:-http://localhost:8084}"
AUTH=()
[ -n "${XRAY_AUTH_TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer $XRAY_AUTH_TOKEN")

command -v jq   >/dev/null 2>&1 || exit 0
command -v curl >/dev/null 2>&1 || exit 0

payload="$(cat)"
tool="$(printf '%s' "$payload" | jq -r '.tool_name // empty' 2>/dev/null)"

files=""

case "$tool" in
  Bash)
    cmd="$(printf '%s' "$payload" | jq -r '.tool_input.command // empty' 2>/dev/null)"
    # Only react to git commits; ignore every other Bash command.
    case "$cmd" in
      *"git commit"*|*"git "*"commit"*)
        # Files changed in the just-created commit (relative to repo root).
        root="$(git rev-parse --show-toplevel 2>/dev/null)"
        [ -z "$root" ] && exit 0
        rels="$(git -C "$root" diff-tree --no-commit-id --name-only -r HEAD 2>/dev/null)"
        [ -z "$rels" ] && exit 0
        # Make absolute so the server resolves them regardless of its cwd.
        files="$(printf '%s\n' "$rels" | while IFS= read -r f; do [ -n "$f" ] && printf '%s\n' "$root/$f"; done)"
        ;;
      *) exit 0 ;;
    esac
    ;;
  *)
    # Edit / Write / MultiEdit: paths from the tool input.
    files="$(printf '%s' "$payload" | jq -r '
      [ .tool_input.file_path? ,
        ( .tool_input.edits[]?.file_path? ),
        ( .tool_input.file_paths[]? )
      ] | map(select(. != null)) | unique | .[]' 2>/dev/null)"
    ;;
esac

[ -z "$files" ] && exit 0

paths_json="$(printf '%s\n' "$files" | jq -R . | jq -sc .)" || exit 0

MCP="$SERVER/mcp"
ACCEPT='application/json, text/event-stream'

# MCP is session-oriented: initialize first and reuse the session id it returns
# in the Mcp-Session-Id response header. -f is deliberately NOT used; a non-2xx
# here should fall through to a quiet exit, not print curl's error to the agent.
sid="$(curl -s -i -m 10 -X POST "$MCP" ${AUTH[@]+"${AUTH[@]}"} \
        -H 'Content-Type: application/json' -H "Accept: $ACCEPT" \
        -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"post-edit-hook","version":"1"}}}' \
      2>/dev/null | tr -d '\r' | awk -F': ' '/^[Mm]cp-[Ss]ession-[Ii]d/{print $2; exit}')"
[ -z "$sid" ] && exit 0

curl -s -m 30 -X POST "$MCP" ${AUTH[@]+"${AUTH[@]}"} \
  -H 'Content-Type: application/json' -H "Accept: $ACCEPT" \
  -H "Mcp-Session-Id: $sid" \
  -d "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"update_code_graph\",\"arguments\":{\"paths\":$paths_json}}}" \
  >/dev/null 2>&1 || true

exit 0
