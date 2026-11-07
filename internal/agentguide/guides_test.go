package agentguide

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetupCarriesCanonicalCallerEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	setup := SetupMarkdown()
	_, fenced, ok := strings.Cut(setup, "```text\n")
	require.True(ok)
	block, _, ok := strings.Cut(fenced, "\n```")
	require.True(ok)
	assert.Equal(ManagedBlock(), block)
	updated, err := UpsertManagedBlock("# Caller-owned instructions\n")
	require.NoError(err)
	assert.Contains(updated, block)
	repeated, err := UpsertManagedBlock(updated)
	require.NoError(err)
	assert.Equal(updated, repeated)
	assert.LessOrEqual(len(setup), 16384)
	assert.LessOrEqual(len(QuickstartMarkdown()), 16384)
}
