package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXSourceMergeRefusesBeforeMutation(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	into, err := st.GetOrCreateSource("x", "1002")
	_testRequire.NoError(err)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	post, err := st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: from.ID, OwnerID: "1001", AuthorID: "1001", PostID: "101", ConversationID: "101", EditHistory: []string{"101"}, Text: "synthetic owner-bound post", CreatedAt: at, FetchedAt: at, Availability: "public"})
	_testRequire.NoError(err)
	for _, dry := range []bool{true, false} {
		_, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: dry})
		_testRequire.ErrorIs(err, store.ErrSourceMergeInvalid)
		current, err := st.GetSourceByID(from.ID)
		_testRequire.NoError(err)
		_testAssert.Zero(current.MergedIntoSourceID)
		target, err := st.ResolveXPostContext(t.Context(), from.ID, "101")
		_testRequire.NoError(err)
		_testAssert.Equal(post.MessageID, target.MessageID)
		var sourceID int64
		_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT source_id FROM messages WHERE id=?"), post.MessageID).Scan(&sourceID))
		_testAssert.Equal(from.ID, sourceID)
	}
}

func TestXMergeGuardPreservesImportedProviderMerges(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("mbox", "first@example.test")
	_testRequire.NoError(err)
	into, err := st.GetOrCreateSource("mbox", "second@example.test")
	_testRequire.NoError(err)
	conversation, err := st.EnsureConversationWithType(from.ID, "synthetic-thread", "email_thread", "Synthetic import")
	_testRequire.NoError(err)
	message, err := st.UpsertMessage(&store.Message{SourceID: from.ID, ConversationID: conversation, SourceMessageID: "synthetic-message", MessageType: "email"})
	_testRequire.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	_testRequire.NoError(err)
	current, err := st.GetSourceByID(from.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(into.ID, current.MergedIntoSourceID)
	var sourceID int64
	_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT source_id FROM messages WHERE id=?"), message).Scan(&sourceID))
	_testAssert.Equal(into.ID, sourceID)
}
