package index

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memoryv1 "github.com/triageagent-dev/x-ray/v2/api/v1"
)

func cellWith(embedding string) *memoryv1.MemoryCell {
	return &memoryv1.MemoryCell{Spec: memoryv1.MemoryCellSpec{ID: "c", Kind: "note", Title: "t", Content: "body", Embedding: embedding}}
}

func TestSeedOrEmbed_ACellThatCarriesAUsableEmbeddingIsSeededAndNothingNeedsWriting(t *testing.T) {
	emb := textGateway(t, map[string][]float32{})
	s := NewCellStore(emb, zerolog.Nop())
	got, err := s.SeedOrEmbed(context.Background(), cellWith(EncodeEmbedding(vec(1, 0, 0))), IndexEntry{ID: "c"})
	require.NoError(t, err)
	assert.Empty(t, got, "the cell already holds its vector")
	assert.EqualValues(t, 1, s.CollectionSize())
}

func TestSeedOrEmbed_ACellWithoutAnEmbeddingIsEmbeddedAndTheVectorReturnedToPersist(t *testing.T) {
	doc := IndexEntry{ID: "c", Kind: "note", Title: "t", Content: "body"}
	emb := textGateway(t, map[string][]float32{cellEmbedText(doc): vec(0, 1, 0)})
	s := NewCellStore(emb, zerolog.Nop())
	got, err := s.SeedOrEmbed(context.Background(), cellWith(""), doc)
	require.NoError(t, err)
	require.NotEmpty(t, got)
	v, err := DecodeEmbedding(got)
	require.NoError(t, err)
	assert.Equal(t, vec(0, 1, 0), v)
	assert.EqualValues(t, 1, s.CollectionSize())
}

func TestSeedOrEmbed_AWrongDimensionOrUndecodableEmbeddingIsReplaced(t *testing.T) {
	doc := IndexEntry{ID: "c", Kind: "note", Title: "t", Content: "body"}
	emb := textGateway(t, map[string][]float32{cellEmbedText(doc): vec(0, 1, 0)})
	s := NewCellStore(emb, zerolog.Nop())
	require.True(t, s.SeedFromEmbedding(IndexEntry{ID: "first"}, vec(1, 0, 0))) // fixes the dimension at four

	wrong := EncodeEmbedding([]float32{1, 0})
	got, err := s.SeedOrEmbed(context.Background(), cellWith(wrong), doc)
	require.NoError(t, err)
	assert.NotEmpty(t, got, "the stored vector disagrees with the index dimension, so the cell is re-embedded")
	assert.NotEqual(t, wrong, got)

	got, err = s.SeedOrEmbed(context.Background(), cellWith("!!! not base64 !!!"), doc)
	require.NoError(t, err)
	assert.NotEmpty(t, got, "an embedding that cannot be decoded is replaced too")
}

func TestSeedOrEmbed_AFailedEmbedIsAnError(t *testing.T) {
	s := NewCellStore(textGateway(t, map[string][]float32{}), zerolog.Nop())
	_, err := s.SeedOrEmbed(context.Background(), cellWith(""), IndexEntry{ID: "c", Title: "unknown"})
	assert.Error(t, err)
}
