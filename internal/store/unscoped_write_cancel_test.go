package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// A store without a sync run writes conversations outside a transaction. Those
// writes must still honor the caller's context.
func TestUnscopedConversationWritesStopOnCancelledContext(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "archive@example.com")
	require.NoError(err)
	existingID, err := st.EnsureConversation(t.Context(), source.ID, "existing-thread", "Original title")
	require.NoError(err)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = st.EnsureConversation(cancelled, source.ID, "new-thread", "New thread")
	require.ErrorIs(err, context.Canceled)
	var created int
	require.NoError(st.DB().QueryRowContext(
		t.Context(),
		st.Rebind(`SELECT COUNT(*) FROM conversations WHERE source_id = ? AND source_conversation_id = ?`),
		source.ID, "new-thread",
	).Scan(&created))
	assert.Equal(0, created)

	err = st.SetConversationTitle(cancelled, source.ID, existingID, "Renamed title")
	require.ErrorIs(err, context.Canceled)
	assert.Equal("Original title", readConvTitle(t, st, existingID))
}
