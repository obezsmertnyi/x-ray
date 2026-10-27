package index

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
)

// EncodeEmbedding serializes a float32 vector to a base64 string (little-endian).
// Returns an empty string for a nil or empty slice.
func EncodeEmbedding(vec []float32) string {
	if len(vec) == 0 {
		return ""
	}
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// DecodeEmbedding deserializes a base64 string produced by EncodeEmbedding back
// to a float32 vector. Returns nil for an empty string.
func DecodeEmbedding(s string) ([]float32, error) {
	if s == "" {
		return nil, nil
	}
	buf, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode embedding: %w", err)
	}
	if len(buf)%4 != 0 {
		return nil, fmt.Errorf("decode embedding: invalid byte length %d", len(buf))
	}
	vec := make([]float32, len(buf)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return vec, nil
}
