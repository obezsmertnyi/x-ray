package xray

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// graphRefDataKey is the Data key under which a mental_map cell carries a
// digest-pinned OCI reference to its graph instead of the graph itself, for
// maps too large for an etcd object (about 1.5 MiB, roughly 480 nodes). The
// reference is plain text in Bytes, not base64: there is no blob to encode, only
// a pointer. The same key and shape the Triage core reads.
const graphRefDataKey = "graphRef"

// graphArtifactSuffix names the one file a graph artifact carries: an ordinary
// image whose layers hold a single regular file ending in this suffix (a
// `make maps-image` image qualifies, as does one made with `crane append`).
const graphArtifactSuffix = ".graph.gob.gz"

// maxGraphArtifactBytes caps what is read from the artifact's file. It is a var
// so a test can shrink it; io.LimitReader enforces it while reading, so a lying
// size header cannot exhaust memory first.
var maxGraphArtifactBytes int64 = 256 << 20

// graphRefPullTimeout bounds one pull, so a stuck registry cannot hang a reconcile.
const graphRefPullTimeout = 2 * time.Minute

// GraphFetcher pulls a graph artifact. Nil on a CellSyncer means the registry
// client below; a test injects an in-process one.
type GraphFetcher func(ctx context.Context, ref name.Digest, kc authn.Keychain) ([]byte, error)

// parseGraphRef parses a graphRef and requires it to be pinned by digest: a tag
// can move, and a map that changes under a running server is not reproducible.
func parseGraphRef(ref string) (name.Digest, error) {
	parsed, err := name.ParseReference(strings.TrimSpace(ref))
	if err != nil {
		return name.Digest{}, fmt.Errorf("graphRef %q: %w", ref, err)
	}
	d, ok := parsed.(name.Digest)
	if !ok {
		return name.Digest{}, fmt.Errorf("graphRef %q is not digest-pinned (need name@sha256:...)", ref)
	}
	return d, nil
}

// fetchGraphArtifact pulls ref and returns its single graph file.
func fetchGraphArtifact(ctx context.Context, ref name.Digest, kc authn.Keychain) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, graphRefPullTimeout)
	defer cancel()
	img, err := remote.Image(ref, remote.WithAuthFromKeychain(kc), remote.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("pull graph artifact %s: %w", ref, err)
	}
	return extractGraphArtifact(img)
}

// extractGraphArtifact returns the bytes of the one regular file ending in
// graphArtifactSuffix across img's layers. It errors on none, on more than one,
// and on one over maxGraphArtifactBytes.
func extractGraphArtifact(img v1.Image) ([]byte, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("graph artifact layers: %w", err)
	}
	var found []byte
	matches := 0
	for _, layer := range layers {
		rc, err := layer.Uncompressed()
		if err != nil {
			return nil, fmt.Errorf("graph artifact layer: %w", err)
		}
		b, n, serr := scanGraphLayer(rc)
		if cerr := rc.Close(); serr == nil && cerr != nil {
			serr = fmt.Errorf("graph artifact layer close: %w", cerr)
		}
		if serr != nil {
			return nil, serr
		}
		matches += n
		if n == 1 {
			found = b
		}
		if matches > 1 {
			return nil, fmt.Errorf("graph artifact carries more than one %s file", graphArtifactSuffix)
		}
	}
	if matches == 0 {
		return nil, fmt.Errorf("graph artifact carries no %s file", graphArtifactSuffix)
	}
	return found, nil
}

// scanGraphLayer walks one layer's tar and returns the matching file and how
// many matched (0 or 1; a second match in the same layer is an error before it
// is read).
func scanGraphLayer(rc io.Reader) ([]byte, int, error) {
	tr := tar.NewReader(rc)
	var found []byte
	matches := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return found, matches, nil
		}
		if err != nil {
			return nil, 0, fmt.Errorf("graph artifact tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || !strings.HasSuffix(hdr.Name, graphArtifactSuffix) {
			continue
		}
		matches++
		if matches > 1 {
			return nil, matches, fmt.Errorf("graph artifact carries more than one %s file", graphArtifactSuffix)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxGraphArtifactBytes+1))
		if err != nil {
			return nil, 0, fmt.Errorf("read graph artifact file %s: %w", hdr.Name, err)
		}
		if int64(len(b)) > maxGraphArtifactBytes {
			return nil, 0, fmt.Errorf("graph artifact file %s exceeds %d byte cap", hdr.Name, maxGraphArtifactBytes)
		}
		found = b
	}
}

// pullKeychain builds the registry credentials for a pull: the Secret named by
// --registry-pull-secret (a dockerconfigjson Secret in the cell's namespace),
// then the docker config of the process. With no secret named it is the latter
// alone, which is anonymous in a pod.
func pullKeychain(ctx context.Context, c client.Reader, namespace, secret string) (authn.Keychain, error) {
	if secret == "" {
		return authn.DefaultKeychain, nil
	}
	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secret}, &sec); err != nil {
		return nil, fmt.Errorf("pull secret %s: %w", secret, err)
	}
	raw, ok := sec.Data[corev1.DockerConfigJsonKey]
	if !ok {
		return nil, fmt.Errorf("pull secret %s has no %s", secret, corev1.DockerConfigJsonKey)
	}
	var cfg struct {
		Auths map[string]authn.AuthConfig `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("pull secret %s: %w", secret, err)
	}
	k := secretKeychain{}
	for host, a := range cfg.Auths {
		host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
		if i := strings.IndexByte(host, '/'); i >= 0 {
			host = host[:i]
		}
		k[host] = a
	}
	return authn.NewMultiKeychain(k, authn.DefaultKeychain), nil
}

// secretKeychain maps a registry host to the credentials a Secret holds for it.
type secretKeychain map[string]authn.AuthConfig

func (k secretKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	if a, ok := k[res.RegistryStr()]; ok {
		return authn.FromConfig(a), nil
	}
	return authn.Anonymous, nil
}
