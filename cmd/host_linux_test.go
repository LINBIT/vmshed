package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostMemory(t *testing.T) {
	memory, err := hostMemory()
	require.NoError(t, err)
	assert.Greater(t, memory, uint64(0))
	assert.Zero(t, memory%(1<<20), "memory should be rounded to whole MiB")
}
