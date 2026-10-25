package xray

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mcell "github.com/triageagent-dev/x-ray/v2/api/v1"
)

// layerOf builds one image layer holding the given files.
func layerOf(t *testing.T, files map[string]string) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for n, c := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(c))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil })
	require.NoError(t, err)
	return l
}

// pushImage pushes an image with the given layers to an in-process registry and
// returns its digest-pinned reference.
func pushImage(t *testing.T, layers ...v1.Layer) name.Digest {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	img, err := mutate.AppendLayers(empty.Image, layers...)
	require.NoError(t, err)
	host := strings.TrimPrefix(srv.URL, "http://")
	tag, err := name.NewTag(host + "/maps:test")
	require.NoError(t, err)
	require.NoError(t, remote.Write(tag, img))
	d, err := img.Digest()
	require.NoError(t, err)
	ref, err := name.NewDigest(host + "/maps@" + d.String())
	require.NoError(t, err)
	return ref
}

func TestFetchGraphArtifact_ReturnsTheGraphFile(t *testing.T) {
	ref := pushImage(t, layerOf(t, map[string]string{"maps/xray.graph.gob.gz": "the-graph"}))
	got, err := fetchGraphArtifact(context.Background(), ref, authn.DefaultKeychain)
	require.NoError(t, err)
	assert.Equal(t, "the-graph", string(got))
}

func TestFetchGraphArtifact_RejectsWhatIsNotOneGraph(t *testing.T) {
	none := pushImage(t, layerOf(t, map[string]string{"readme.txt": "x"}))
	_, err := fetchGraphArtifact(context.Background(), none, authn.DefaultKeychain)
	assert.ErrorContains(t, err, "no .graph.gob.gz file")

	two := pushImage(t, layerOf(t, map[string]string{"a.graph.gob.gz": "1", "b.graph.gob.gz": "2"}))
	_, err = fetchGraphArtifact(context.Background(), two, authn.DefaultKeychain)
	assert.ErrorContains(t, err, "more than one")

	old := maxGraphArtifactBytes
	maxGraphArtifactBytes = 4
	t.Cleanup(func() { maxGraphArtifactBytes = old })
	big := pushImage(t, layerOf(t, map[string]string{"a.graph.gob.gz": "0123456789"}))
	_, err = fetchGraphArtifact(context.Background(), big, authn.DefaultKeychain)
	assert.ErrorContains(t, err, "byte cap")
}

func TestParseGraphRef_RequiresADigest(t *testing.T) {
	_, err := parseGraphRef("ghcr.io/acme/maps:latest")
	assert.ErrorContains(t, err, "not digest-pinned")
	_, err = parseGraphRef("ghcr.io/acme/maps@sha256:" + strings.Repeat("a", 64))
	assert.NoError(t, err)
}

type fakeRegistrar struct {
	mu      sync.Mutex
	added   [][]byte
	removed []string
}

func (f *fakeRegistrar) AddSnapshotBytes(_ context.Context, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, b)
	return nil
}
func (f *fakeRegistrar) RemoveSnapshotByLabel(l string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, l)
}

func graphRefCell(ref string) *mcell.MemoryCell {
	return &mcell.MemoryCell{
		ObjectMeta: metav1.ObjectMeta{Name: "map-xray", Namespace: "xray"},
		Spec: mcell.MemoryCellSpec{
			ID: "map-xray", Kind: "mental_map", Scope: "public", Subject: "xray",
			Title: "X-Ray code map", Content: "The X-Ray repository's code graph.",
			Data: map[string]mcell.DataEntry{graphRefDataKey: {Bytes: ref, Mime: "text/plain"}},
		},
	}
}

// A mental_map cell that carries a graphRef is pulled and registered as a map,
// once: a replay of the same cell does not pull or register again, and a failed
// pull is pending (the reconciler retries it) until it succeeds.
func TestSyncMapCell_GraphRef(t *testing.T) {
	ref := "ghcr.io/acme/maps@sha256:" + strings.Repeat("b", 64)
	reg := &fakeRegistrar{}
	s := NewCellSyncer(nil, NewMemoryStore(), zerolog.Nop())
	s.SetMapRegistrar(reg)

	pulls := 0
	var failWith error = errors.New("registry unreachable")
	s.graphFetch = func(_ context.Context, got name.Digest, _ authn.Keychain) ([]byte, error) {
		pulls++
		assert.Equal(t, ref, got.String())
		if failWith != nil {
			return nil, failWith
		}
		return []byte("graph-bytes"), nil
	}
	cell := graphRefCell(ref)

	require.Error(t, s.syncMapCell(context.Background(), cell))
	assert.True(t, s.mapPending(cell), "a failed pull is retried")
	assert.Empty(t, reg.added)

	failWith = nil
	require.NoError(t, s.syncMapCell(context.Background(), cell))
	assert.Equal(t, [][]byte{[]byte("graph-bytes")}, reg.added)
	assert.False(t, s.mapPending(cell))

	require.NoError(t, s.syncMapCell(context.Background(), cell))
	assert.Equal(t, 2, pulls, "an unchanged reference is not pulled again")
	assert.Len(t, reg.added, 1)

	s.removeMapCell(cell)
	assert.Contains(t, reg.removed, "xray")
}

func TestSyncMapCell_GraphRefMustBePinned(t *testing.T) {
	reg := &fakeRegistrar{}
	s := NewCellSyncer(nil, NewMemoryStore(), zerolog.Nop())
	s.SetMapRegistrar(reg)
	s.graphFetch = func(context.Context, name.Digest, authn.Keychain) ([]byte, error) {
		t.Fatal("a tag must never be pulled")
		return nil, nil
	}
	err := s.syncMapCell(context.Background(), graphRefCell("ghcr.io/acme/maps:latest"))
	assert.ErrorContains(t, err, "not digest-pinned")
}
