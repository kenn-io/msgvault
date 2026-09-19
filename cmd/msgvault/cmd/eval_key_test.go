//go:build sqlite_vec

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// TestDocKeyFuncs pins the doc-key registry: the scoring core only ever sees
// the opaque ids these extractors return, so each --doc-key value must map to
// the field its qrels actually reference. A future judged unit (for example a
// reconstructed-thread id resolved through an external mapping) is added as
// one more entry here and must not require touching this contract.
func TestDocKeyFuncs(t *testing.T) {
	m := evalHit{
		MessageID:            7,
		SourceMessageID:      "<msg-1@example.com>",
		SourceConversationID: "thread-42",
	}
	registry := newDocKeyRegistry()

	msgKey, ok := registry["message"]
	require.True(t, ok)
	assert.Equal(t, "<msg-1@example.com>", msgKey.extract(m))

	convKey, ok := registry["conversation"]
	require.True(t, ok)
	assert.Equal(t, "thread-42", convKey.extract(m))

	_, ok = registry["thread"]
	assert.False(t, ok, "thread scoring is a future extension, not yet registered")
}

// TestDocKeySpec_Collapses pins which keys need the over-fetch: a conversation
// id is shared by every message in a thread, a source message id is not.
func TestDocKeySpec_Collapses(t *testing.T) {
	registry := newDocKeyRegistry()
	assert.False(t, registry["message"].collapses)
	assert.True(t, registry["conversation"].collapses)
}

// TestNewDocKeyRegistry_IsPerRun backs the extensibility claim in the
// registry's doc comment: it is built by a call, not fixed at program init, so
// an entry closing over state that only exists after flags are parsed (a
// loaded message-id -> thread-id mapping, say) is possible. Two calls must
// therefore produce independent maps.
func TestNewDocKeyRegistry_IsPerRun(t *testing.T) {
	first := newDocKeyRegistry()
	second := newDocKeyRegistry()
	require.Len(t, second, len(first))

	first["thread"] = docKeySpec{extract: func(evalHit) string { return "x" }}
	_, leaked := newDocKeyRegistry()["thread"]
	assert.False(t, leaked, "a run's registry must not mutate the next run's")
}

// TestDocKeyNames keeps usage/error text in step with the registry, sorted so
// the rendering is stable.
func TestDocKeyNames(t *testing.T) {
	assert.Equal(t, "conversation|message", docKeyNames(newDocKeyRegistry()))
}

// TestEvalHitProjections_AgreeAcrossRetrievalPaths backs the claim evalHit
// exists to make: a --doc-key means the same thing whichever engine produced
// the hit. fts hits arrive as store.APIMessage and vector/hybrid hits as
// query.MessageSummary, and if those two projected differently the same
// message would score under two different ids.
func TestEvalHitProjections_AgreeAcrossRetrievalPaths(t *testing.T) {
	fromFTS := hitFromAPIMessage(store.APIMessage{
		ID:                   7,
		SourceMessageID:      "<msg-1@example.com>",
		SourceConversationID: "thread-42",
	})
	fromVector := hitFromSummary(query.MessageSummary{
		ID:                   7,
		SourceMessageID:      "<msg-1@example.com>",
		SourceConversationID: "thread-42",
	})
	assert.Equal(t, fromFTS, fromVector, "the same message must project identically")

	for name, spec := range newDocKeyRegistry() {
		assert.Equal(t, spec.extract(fromFTS), spec.extract(fromVector),
			"--doc-key=%s must not depend on which engine returned the hit", name)
	}
}
