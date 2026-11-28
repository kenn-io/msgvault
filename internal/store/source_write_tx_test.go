package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func sourceWriteTxFixture(t *testing.T) (*store.Store, int64, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "batch@example.com")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "thread-1", "Thread")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conv, SourceMessageID: "m1", MessageType: store.MessageTypeEmail,
	})
	require.NoError(t, err)
	return st, source.ID, id
}

// A failed store call inside a batch transaction leaves the batch usable, as
// it would on its own: PostgreSQL otherwise aborts every later statement.
func TestSourceWriteTxSurvivesFailedJoinedCall(t *testing.T) {
	t.Parallel()
	st, sourceID, messageID := sourceWriteTxFixture(t)
	body := sql.NullString{String: "kept", Valid: true}

	err := st.WithSourceWriteTxContext(t.Context(), sourceID, func(ctx context.Context) error {
		missing := st.UpsertMessageBodyContext(ctx, messageID+1000, body, sql.NullString{})
		require.Error(t, missing, "a body for a missing message violates its foreign key")
		return st.UpsertMessageBodyContext(ctx, messageID, body, sql.NullString{})
	})
	require.NoError(t, err)

	got, err := st.GetMessageBodyText(t.Context(), messageID)
	require.NoError(t, err)
	assert.Equal(t, "kept", got)
}

// A batch item rolled back through its savepoint leaves no writes, while the
// rest of the batch commits.
func TestSavepointRollsBackOneBatchItem(t *testing.T) {
	t.Parallel()
	st, sourceID, first := sourceWriteTxFixture(t)
	conv, err := st.EnsureConversation(t.Context(), sourceID, "thread-1", "Thread")
	require.NoError(t, err)
	second, err := st.UpsertMessage(&store.Message{
		SourceID: sourceID, ConversationID: conv, SourceMessageID: "m2", MessageType: store.MessageTypeEmail,
	})
	require.NoError(t, err)

	err = st.WithSourceWriteTxContext(t.Context(), sourceID, func(ctx context.Context) error {
		for _, item := range []struct {
			id   int64
			keep bool
		}{{first, false}, {second, true}} {
			done, err := st.SavepointContext(ctx)
			require.NoError(t, err)
			require.NoError(t, st.UpsertMessageBodyContext(ctx, item.id,
				sql.NullString{String: "body", Valid: true}, sql.NullString{}))
			require.NoError(t, done(item.keep))
		}
		return nil
	})
	require.NoError(t, err)

	got, err := st.GetMessageBodyText(t.Context(), second)
	require.NoError(t, err)
	assert.Equal(t, "body", got, "the kept item commits with the batch")
	got, err = st.GetMessageBodyText(t.Context(), first)
	if err == nil {
		assert.Empty(t, got, "the rolled-back item leaves no body")
	} else {
		require.ErrorIs(t, err, sql.ErrNoRows)
	}
}
