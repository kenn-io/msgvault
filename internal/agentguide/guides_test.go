package agentguide

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetupCarriesCanonicalCallerEdit(t *testing.T) {
	setup := SetupMarkdown()
	_, fenced, ok := strings.Cut(setup, "```text\n")
	require.True(t, ok)
	block, _, ok := strings.Cut(fenced, "\n```")
	require.True(t, ok)
	assert.Equal(t, ManagedBlock(), block)
	updated, err := UpsertManagedBlock("# Caller-owned instructions\n")
	require.NoError(t, err)
	assert.Contains(t, updated, block)
	repeated, err := UpsertManagedBlock(updated)
	require.NoError(t, err)
	assert.Equal(t, updated, repeated)
	assert.LessOrEqual(t, len(setup), 16384)
	assert.LessOrEqual(t, len(QuickstartMarkdown()), 16384)
}
