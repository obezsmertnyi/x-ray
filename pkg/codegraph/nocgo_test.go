//go:build !cgo

package codegraph_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/triageagent-dev/x-ray/v2/pkg/codegraph"
)

// Without cgo there is no parser: Build says so, and Load is the way in.
func TestBuild_WithoutCgoSaysSo(t *testing.T) {
	_, err := codegraph.Build(context.Background(), nil, codegraph.Repo{Path: "."})
	assert.ErrorIs(t, err, codegraph.ErrNoParser)
}
