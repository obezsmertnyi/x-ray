#!/bin/sh
# spec-map.sh — build the specification as a graph map and pack it as an OCI image, LOCALLY.
#
# Usage: spec-map.sh <graph-binary> <out-dir> <image-ref> [spec.md]
#
# Splits the spec into one node per sub-section (split-doc-map.py), snapshots it
# as the map "xray-spec" with the parser build of xray, and packs the snapshot
# with maps-oci.sh. Nothing is pushed: the command to publish is printed.
# Embedding flags come from XRAY_EMBEDDING_{ENDPOINT,MODEL,DIMS}; they must match
# the server that will load the map (see the fingerprint note in docs/graph.md).
set -eu

bin=${1:?parser-build xray binary (make graph-build)}
out=${2:?output dir}
ref=${3:?image ref}
spec=${4:-docs/spec.md}
here=$(cd "$(dirname "$0")" && pwd)

rm -rf "$out"
mkdir -p "$out/src" "$out/maps"
python3 -I "$here/split-doc-map.py" "$spec" > "$out/src/spec.md"

"$bin" graph snapshot \
	-repo "xray-spec=$out/src" \
	-snapshot-dir "$out/maps" \
	-embedding-endpoint "${XRAY_EMBEDDING_ENDPOINT:-http://localhost:8090}" \
	-embedding-model "${XRAY_EMBEDDING_MODEL:-nomic}" \
	-embedding-dims "${XRAY_EMBEDDING_DIMS:-256}" \
	-summary "xray-spec=what X-Ray is and how it behaves: the MemoryCell model and kinds, scope and provenance, storage and restart, recall ranking, MCP tools and REST routes, access, voice, code graph and maps, flags and chart values"

sh "$here/maps-oci.sh" "$out/maps" "$out/xray-spec.oci.tar" "$ref" xray-spec
