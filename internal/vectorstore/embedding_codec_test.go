package vectorstore

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		vec  []float32
	}{
		{"nil", nil},
		{"empty", []float32{}},
		{"single zero", []float32{0}},
		{"single one", []float32{1}},
		{"unit vector", []float32{1, 0, 0}},
		{"negative values", []float32{-1.5, 2.5, -3.14}},
		{"small embedding", []float32{0.1, 0.2, 0.3, 0.4, 0.5}},
		{"special: max float32", []float32{math.MaxFloat32}},
		{"special: smallest positive", []float32{math.SmallestNonzeroFloat32}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded := EncodeEmbedding(tt.vec)
			decoded, err := DecodeEmbedding(encoded)
			require.NoError(t, err)

			if len(tt.vec) == 0 {
				assert.Empty(t, decoded)
				return
			}
			require.Len(t, decoded, len(tt.vec))
			for i, v := range tt.vec {
				assert.Equal(t, v, decoded[i], "element %d mismatch", i)
			}
		})
	}
}

func TestEncodeEmbedding_EmptyReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", EncodeEmbedding(nil))
	assert.Equal(t, "", EncodeEmbedding([]float32{}))
}

func TestDecodeEmbedding_EmptyReturnsNil(t *testing.T) {
	out, err := DecodeEmbedding("")
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestDecodeEmbedding_InvalidBase64(t *testing.T) {
	_, err := DecodeEmbedding("not-valid-base64!!!")
	assert.Error(t, err)
}

func TestDecodeEmbedding_InvalidByteLength(t *testing.T) {
	// "AAEC" decodes to 3 bytes (0x00 0x01 0x02), not a multiple of 4.
	_, err := DecodeEmbedding("AAEC")
	assert.Error(t, err, "3-byte buffer should fail (not multiple of 4)")
}

func TestDecodeEmbedding_LargeVector(t *testing.T) {
	vec := make([]float32, 384) // bge-small-en-v1.5 dimension
	for i := range vec {
		vec[i] = float32(i) * 0.001
	}
	encoded := EncodeEmbedding(vec)
	decoded, err := DecodeEmbedding(encoded)
	require.NoError(t, err)
	require.Len(t, decoded, 384)
	for i, v := range vec {
		assert.Equal(t, v, decoded[i])
	}
}
