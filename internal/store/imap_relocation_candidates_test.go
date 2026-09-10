package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestIMAPRelocationCandidates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	var sourceID, targetID int64
	for i := range 2 {
		source, err := st.GetOrCreateSource("imap", fmt.Sprintf("candidate-%d@example.test", i))
		require.NoError(err)
		conv, err := st.EnsureConversation(source.ID, "thread", "thread")
		require.NoError(err)
		id, err := st.UpsertMessage(&store.Message{ConversationID: conv, SourceID: source.ID, SourceMessageID: "Drafts|1", RFC822MessageID: sql.NullString{String: "same@example.test", Valid: true}, MessageType: "email"})
		require.NoError(err)
		if i == 0 {
			sourceID, targetID = source.ID, id
		}
		for _, mailbox := range []string{"Drafts", "Sent", "Archive"} {
			_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_message_memberships (source_id, mailbox, uidvalidity, uid, message_id, flags) VALUES (?, ?, ?, ?, ?, ?)`), source.ID, mailbox, 77+i, 1, id, "[]")
			require.NoError(err)
		}
	}
	conv, err := st.EnsureConversation(sourceID, "other-thread", "Other")
	require.NoError(err)
	for _, sourceMessageID := range []string{"Drafts|2", "Drafts|3"} {
		rfc822 := sql.NullString{String: "unrequested@example.test", Valid: sourceMessageID == "Drafts|2"}
		id, err := st.UpsertMessage(&store.Message{ConversationID: conv, SourceID: sourceID, SourceMessageID: sourceMessageID, RFC822MessageID: rfc822, MessageType: "email"})
		require.NoError(err)
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_message_memberships (source_id, mailbox, uidvalidity, uid, message_id, flags) VALUES (?, ?, ?, ?, ?, ?)`), sourceID, sourceMessageID, 77, 1, id, "[]")
		require.NoError(err)
	}
	unidentified, err := st.GetIMAPRelocationCandidatesContext(t.Context(), sourceID, []string{"Drafts|3"})
	require.NoError(err)
	assert.Empty(unidentified)
	got, err := st.GetIMAPRelocationCandidatesContext(t.Context(), sourceID, []string{"Drafts|1"})
	require.NoError(err)
	require.Len(got, 3)
	for _, candidate := range got {
		assert.Equal(store.MessageIdentityGuard{ID: targetID, SourceID: sourceID, SourceMessageID: "Drafts|1"}, candidate.MessageIdentityGuard)
		assert.Equal("same@example.test", candidate.RFC822MessageID)
		assert.Equal(uint32(77), candidate.UIDValidity)
		assert.Equal(uint32(1), candidate.UID)
	}
	assert.Equal("Archive", got[0].Mailbox)
	assert.Equal("Drafts", got[1].Mailbox)
	assert.Equal("Sent", got[2].Mailbox)
	empty, err := st.GetIMAPRelocationCandidatesContext(t.Context(), sourceID, nil)
	require.NoError(err)
	assert.Empty(empty)
	keys := make([]string, 1901)
	for i := range keys {
		keys[i] = fmt.Sprintf("absent|%d", i)
	}
	keys[1900] = "Drafts|1"
	chunked, err := st.GetIMAPRelocationCandidatesContext(t.Context(), sourceID, keys)
	require.NoError(err)
	assert.Equal(got, chunked)
}
