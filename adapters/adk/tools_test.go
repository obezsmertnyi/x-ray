package xrayadk_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	xrayadk "github.com/triageagent-dev/x-ray/adapters/adk"
)

// The tools build their JSON schemas from the argument structs, so a bad tag or
// type shows up here and not when the agent starts.
func TestTools_BuildAndAreNamed(t *testing.T) {
	_, c := newStub(t)
	save, err := xrayadk.SaveTool(c)
	require.NoError(t, err)
	assert.Equal(t, "save_memory", save.Name())

	refute, err := xrayadk.RefuteTool(c)
	require.NoError(t, err)
	assert.Equal(t, "refute_memory", refute.Name())
}
