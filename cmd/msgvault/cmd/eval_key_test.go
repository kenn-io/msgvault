//go:build sqlite_vec

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
)

// TestDocKeyFuncs pins the doc-key registry: the scoring core only ever sees
// the opaque ids these extractors return, so each --doc-key value must map to
// the field its qrels actually reference. A future judged unit (for example a
// reconstructed-thread id resolved through an external mapping) is added as
// one more entry here and must not require touching this contract.
func TestDocKeyFuncs(t *testing.T) {
	m := query.MessageSummary{
		SourceMessageID:      "<msg-1@example.com>",
		SourceConversationID: "thread-42",
	}

	msgKey, ok := docKeyFuncs["message"]
	require.True(t, ok)
	assert.Equal(t, "<msg-1@example.com>", msgKey(m))

	convKey, ok := docKeyFuncs["conversation"]
	require.True(t, ok)
	assert.Equal(t, "thread-42", convKey(m))

	_, ok = docKeyFuncs["thread"]
	assert.False(t, ok, "thread scoring is a future extension, not yet registered")
}

// TestDocKeyNames keeps usage/error text in step with the registry, sorted so
// the rendering is stable.
func TestDocKeyNames(t *testing.T) {
	assert.Equal(t, "conversation|message", docKeyNames())
}
