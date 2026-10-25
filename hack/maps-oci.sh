#!/bin/sh
# maps-oci.sh — assemble a maps image as an OCI image layout, LOCALLY.
#
# Usage: maps-oci.sh <snapshot-dir> <out-tar> <ref> [label ...]
#
# The image is `FROM scratch` with one layer holding maps/<label>.graph.gob.gz
# for each label (every *.graph.gob.gz in <snapshot-dir> when none is named).
# The chart mounts such an image as a Kubernetes image volume (or seeds a PVC
# from it), so the file layout is the contract: snapshots live under /maps.
#
# Why hand-assembled and not `crane append`: with no base image crane writes a
# config with no os/architecture, and the kubelet rejects a platform-less
# image for an image volume. The layout below declares linux/amd64 explicitly.
# (The layer is plain data, so the platform is a formality — but a required one.)
#
# Why local only: this script never talks to a registry. Pushing is a separate,
# deliberate step — the exact command is printed at the end, not run.
#
# Needs: tar (bsdtar or GNU), jq, and shasum or sha256sum. No docker, no crane.
set -eu

snapdir=${1:?snapshot dir}
out=${2:?output tar}
ref=${3:?image ref}
shift 3

sha() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
	else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# Owner inside the image: the chart runs as 65532. tar flags differ between
# bsdtar (macOS) and GNU tar, so pick the spelling the local tar understands.
tar_owned() { # tar_owned <out.tar> <dir-to-chdir> <entry>
	# bsdtar on macOS adds a ._<name> AppleDouble file for every file with extended
	# attributes; the server then logs it as a snapshot it cannot read.
	export COPYFILE_DISABLE=1
	if tar --version 2>/dev/null | grep -q bsdtar; then
		tar --uid 65532 --gid 65532 --uname '' --gname '' -C "$2" -cf "$1" "$3"
	else
		tar --owner=65532 --group=65532 --numeric-owner -C "$2" -cf "$1" "$3"
	fi
}

work=$(mktemp -d "${TMPDIR:-/tmp}/maps-oci.XXXXXX")
trap 'rm -rf "$work"' EXIT

# 1. Stage the snapshots as maps/<label>.graph.gob.gz.
mkdir -p "$work/stage/maps"
if [ $# -eq 0 ]; then
	set -- $(ls "$snapdir"/*.graph.gob.gz 2>/dev/null | sed 's#.*/##; s#\.graph\.gob\.gz$##')
fi
[ $# -gt 0 ] || { echo "maps-oci: no *.graph.gob.gz in $snapdir" >&2; exit 1; }
for label in "$@"; do
	src="$snapdir/$label.graph.gob.gz"
	[ -f "$src" ] || { echo "maps-oci: no snapshot for map \"$label\" at $src" >&2; exit 1; }
	cp "$src" "$work/stage/maps/$label.graph.gob.gz"
	chmod 0644 "$work/stage/maps/$label.graph.gob.gz"
done
chmod 0755 "$work/stage/maps"

# 2. One uncompressed tar layer. Its digest is the diff_id (uncompressed) AND the
#    blob digest, because the layer is stored uncompressed.
tar_owned "$work/layer.tar" "$work/stage" maps
layer_digest=$(sha "$work/layer.tar")
layer_size=$(wc -c <"$work/layer.tar" | tr -d ' ')

# 3. Config, manifest, index, oci-layout — each blob addressed by its digest.
layout="$work/layout"
mkdir -p "$layout/blobs/sha256"
mv "$work/layer.tar" "$layout/blobs/sha256/$layer_digest"

jq -nc --arg d "sha256:$layer_digest" --arg ref "$ref" --arg created "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '{
	architecture: "amd64",
	os: "linux",
	created: $created,
	config: {},
	rootfs: {type: "layers", diff_ids: [$d]},
	history: [{created: $created, created_by: "xray-memory make maps-image", comment: ("maps image " + $ref)}]
}' >"$work/config.json"
config_digest=$(sha "$work/config.json")
config_size=$(wc -c <"$work/config.json" | tr -d ' ')
mv "$work/config.json" "$layout/blobs/sha256/$config_digest"

jq -nc --arg cd "sha256:$config_digest" --argjson cs "$config_size" \
	--arg ld "sha256:$layer_digest" --argjson ls "$layer_size" '{
	schemaVersion: 2,
	mediaType: "application/vnd.oci.image.manifest.v1+json",
	config: {mediaType: "application/vnd.oci.image.config.v1+json", digest: $cd, size: $cs},
	layers: [{mediaType: "application/vnd.oci.image.layer.v1.tar", digest: $ld, size: $ls}]
}' >"$work/manifest.json"
manifest_digest=$(sha "$work/manifest.json")
manifest_size=$(wc -c <"$work/manifest.json" | tr -d ' ')
mv "$work/manifest.json" "$layout/blobs/sha256/$manifest_digest"

jq -nc --arg md "sha256:$manifest_digest" --argjson ms "$manifest_size" --arg ref "$ref" '{
	schemaVersion: 2,
	mediaType: "application/vnd.oci.image.index.v1+json",
	manifests: [{
		mediaType: "application/vnd.oci.image.manifest.v1+json", digest: $md, size: $ms,
		platform: {architecture: "amd64", os: "linux"},
		annotations: {"org.opencontainers.image.ref.name": $ref}
	}]
}' >"$layout/index.json"
printf '{"imageLayoutVersion":"1.0.0"}' >"$layout/oci-layout"

# 4. Pack the layout. Unpack it anywhere and `crane push <dir> <registry-ref>`.
mkdir -p "$(dirname "$out")"
tar -C "$layout" -cf "$out" oci-layout index.json blobs

echo "maps-oci: wrote $out"
echo "  ref:      $ref"
echo "  maps:     $*"
echo "  manifest: sha256:$manifest_digest"
echo "  layer:    sha256:$layer_digest ($layer_size bytes)"
echo "NOT RUN — to publish, unpack and push it yourself. A push tool may rewrite the"
echo "manifest, so pin a mental_map cell's graphRef to the digest the REGISTRY reports"
echo "afterwards (skopeo inspect --format '{{.Digest}}' docker://<ref>), not the one above:"
echo "  mkdir layout && tar -C layout -xf $out && crane push layout <registry>/<repo>:<tag>"
