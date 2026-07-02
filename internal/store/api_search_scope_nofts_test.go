package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
)

// TestSearchMessagesQueryImpl_NoFTS_AccountScoping verifies that Query.AccountIDs
// scopes results on the LIKE fallback path (ftsAvailable=false), reached when
// FTS errors at runtime or the binary is built without the fts5 tag. Forces the
// no-FTS branch directly so it runs regardless of the fts5 build tag.
func TestSearchMessagesQueryImpl_NoFTS_AccountScoping(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := openTestStore(t)

	src1, err := st.GetOrCreateSource("gmail", "scope-nofts-1@example.com")
	require.NoError(err, "GetOrCreateSource src1")
	conv1, err := st.EnsureConversation(src1.ID, "nofts-scope-thread-1", "Thread 1")
	require.NoError(err, "EnsureConversation src1")
	msg1, err := st.UpsertMessage(&Message{
		ConversationID:  conv1,
		SourceID:        src1.ID,
		SourceMessageID: "nofts-scope-1",
		MessageType:     "email",
		Subject:         sql.NullString{String: "quarterly invoice", Valid: true},
		Snippet:         sql.NullString{String: "review invoice", Valid: true},
		SizeEstimate:    100,
	})
	require.NoError(err, "UpsertMessage src1")

	src2, err := st.GetOrCreateSource("gmail", "scope-nofts-2@example.com")
	require.NoError(err, "GetOrCreateSource src2")
	conv2, err := st.EnsureConversation(src2.ID, "nofts-scope-thread-2", "Thread 2")
	require.NoError(err, "EnsureConversation src2")
	_, err = st.UpsertMessage(&Message{
		ConversationID:  conv2,
		SourceID:        src2.ID,
		SourceMessageID: "nofts-scope-2",
		MessageType:     "email",
		Subject:         sql.NullString{String: "quarterly invoice", Valid: true},
		Snippet:         sql.NullString{String: "review invoice", Valid: true},
		SizeEstimate:    100,
	})
	require.NoError(err, "UpsertMessage src2")

	// Baseline: unscoped LIKE search sees both.
	_, total, err := st.searchMessagesQueryImpl(
		&search.Query{TextTerms: []string{"invoice"}}, 0, 50, false)
	require.NoError(err, "baseline no-FTS search")
	require.Equal(int64(2), total, "unscoped no-FTS total")

	msgs, total, err := st.searchMessagesQueryImpl(&search.Query{
		TextTerms:  []string{"invoice"},
		AccountIDs: []int64{src1.ID},
	}, 0, 50, false)
	require.NoError(err, "scoped no-FTS search")
	assert.Equal(int64(1), total, "scoped no-FTS total")
	require.Len(msgs, 1)
	assert.Equal(msg1, msgs[0].ID, "scoped no-FTS message ID")
}
