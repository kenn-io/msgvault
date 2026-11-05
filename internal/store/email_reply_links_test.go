package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func replyLinkState(t *testing.T, st *store.Store, id int64) (sql.NullString, sql.NullInt64) {
	t.Helper()
	var metadata sql.NullString
	var reply sql.NullInt64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT metadata, reply_to_message_id FROM messages WHERE id = ?`), id).Scan(&metadata, &reply))
	return metadata, reply
}

func createThreadEmail(t *testing.T, f *storetest.Fixture, sourceMessageID, thread, rfcID string) int64 {
	t.Helper()
	conversation, err := f.Store.EnsureConversation(t.Context(), f.Source.ID, thread, "Thread")
	require.NoError(t, err)
	id, err := f.Store.UpsertMessage(&store.Message{
		SourceID: f.Source.ID, ConversationID: conversation, SourceMessageID: sourceMessageID,
		MessageType:     store.MessageTypeEmail,
		RFC822MessageID: sql.NullString{String: rfcID, Valid: rfcID != ""},
	})
	require.NoError(t, err)
	return id
}

func TestPersistMessageRecordsEmailReplyParent(t *testing.T) {
	for _, tc := range []struct {
		name, messageType, stored string
		replace                   *sql.NullString
		want                      string
	}{
		{name: "new email", messageType: store.MessageTypeEmail,
			want: `{"email_in_reply_to":"parent@example.test"}`},
		{name: "after metadata replacement", messageType: store.MessageTypeEmail,
			replace: &sql.NullString{String: `{"imap_content_origin":"sent"}`, Valid: true},
			want:    `{"imap_content_origin":"sent","email_in_reply_to":"parent@example.test"}`},
		{name: "keeps stored parent", messageType: store.MessageTypeEmail,
			stored: `{"email_in_reply_to":"original@example.test"}`,
			want:   `{"email_in_reply_to":"original@example.test"}`},
		{name: "keeps unreadable metadata", messageType: store.MessageTypeEmail,
			stored: `[1,2]`, want: `[1,2]`},
		{name: "chat message", messageType: store.MessageTypeGoogleChat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := storetest.New(t)
			message := &store.Message{
				SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "child",
				MessageType: tc.messageType,
			}
			if tc.stored != "" {
				id := f.CreateMessage("child")
				require.NoError(f.Store.SetMessageMetadata(id, sql.NullString{String: tc.stored, Valid: true}))
			}
			id, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
				Message: message, Metadata: tc.replace, EmailInReplyTo: "<parent@example.test>",
			})
			require.NoError(err)
			metadata, _ := replyLinkState(t, f.Store, id)
			if tc.want == "" {
				assert.False(metadata.Valid, metadata.String)
				return
			}
			require.True(metadata.Valid)
			assert.JSONEq(tc.want, metadata.String)
		})
	}
}

func TestLinkEmailRepliesSinceLinksParentArchivedAfterReply(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	early := createThreadEmail(t, f, "early-reply", "thread-a", "early@example.test")
	stray := createThreadEmail(t, f, "stray-reply", "thread-b", "stray@example.test")
	ambiguous := createThreadEmail(t, f, "ambiguous-reply", "thread-c", "ambiguous@example.test")
	for id, parent := range map[int64]string{
		early: "parent@example.test", stray: "split@example.test", ambiguous: "twin@example.test",
	} {
		require.NoError(st.RecordEmailHeadersContext(t.Context(), f.Source.ID, id, "", parent))
	}
	watermark, err := st.LatestSourceMessageIDContext(t.Context(), f.Source.ID)
	require.NoError(err)

	parent := createThreadEmail(t, f, "parent", "thread-a", "parent@example.test")
	// A parent filed in another conversation is outside the incremental scope.
	split := createThreadEmail(t, f, "split", "thread-elsewhere", "split@example.test")
	createThreadEmail(t, f, "twin-1", "thread-c", "twin@example.test")
	createThreadEmail(t, f, "twin-2", "thread-c", "twin@example.test")
	reply := createThreadEmail(t, f, "new-reply", "thread-a", "new@example.test")
	require.NoError(st.RecordEmailHeadersContext(t.Context(), f.Source.ID, reply, "", "parent@example.test"))
	latest, err := st.LatestSourceMessageIDContext(t.Context(), f.Source.ID)
	require.NoError(err)
	assert.Equal(reply, latest)

	require.NoError(st.LinkEmailRepliesSinceContext(t.Context(), f.Source.ID, watermark, latest))
	for _, tc := range []struct {
		name  string
		child int64
		want  sql.NullInt64
	}{
		{"late parent", early, sql.NullInt64{Int64: parent, Valid: true}},
		{"new reply", reply, sql.NullInt64{Int64: parent, Valid: true}},
		{"other conversation", stray, sql.NullInt64{}},
		{"ambiguous", ambiguous, sql.NullInt64{}},
	} {
		_, got := replyLinkState(t, st, tc.child)
		assert.Equal(tc.want, got, tc.name)
	}

	require.NoError(st.ResolveEmailReplyParentsContext(t.Context(), f.Source.ID, 0, nil))
	_, got := replyLinkState(t, st, stray)
	assert.Equal(sql.NullInt64{Int64: split, Valid: true}, got, "a full pass finds the split parent")
	_, got = replyLinkState(t, st, ambiguous)
	assert.False(got.Valid, "a full pass keeps ambiguous parents unlinked")
}

func TestBackfillEmailReplyHeadersReadsArchivedMIME(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	listed := f.PersistEmail(storetest.EmailSpec{From: "sender@example.com", Subject: "Re: list",
		Raw: "From: sender@example.com\r\nSubject: Re: list\r\nIn-Reply-To: <first@example.test>\r\n <second@example.test>\r\n\r\nBody\r\n"})
	plain := f.PersistEmail(storetest.EmailSpec{From: "sender@example.com", Subject: "No parent"})
	kept := f.PersistEmail(storetest.EmailSpec{From: "sender@example.com", Subject: "Re: kept",
		Raw: "From: sender@example.com\r\nSubject: Re: kept\r\nIn-Reply-To: <other@example.test>\r\n\r\nBody\r\n"})
	require.NoError(st.SetMessageMetadata(kept, sql.NullString{String: `{"email_in_reply_to":"kept@example.test"}`, Valid: true}))
	broken := f.PersistEmail(storetest.EmailSpec{From: "sender@example.com", Subject: "Re: broken",
		Raw: "From: sender@example.com\r\nIn-Reply-To: <broken@example.test>\r\n\r\nBody\r\n"})
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`),
		[]byte("not zlib"), broken)
	require.NoError(err)

	page, err := st.BackfillEmailReplyHeadersContext(t.Context(), f.Source.ID, 0)
	require.NoError(err)
	assert.Equal(store.EmailReplyBackfillPage{LastID: broken, Scanned: 3, Recorded: 1, Undecodable: 1}, page)
	page, err = st.BackfillEmailReplyHeadersContext(t.Context(), f.Source.ID, page.LastID)
	require.NoError(err)
	assert.Equal(store.EmailReplyBackfillPage{}, page)

	for id, want := range map[int64]string{
		listed: `{"email_in_reply_to":"first@example.test"}`,
		kept:   `{"email_in_reply_to":"kept@example.test"}`,
	} {
		metadata, _ := replyLinkState(t, st, id)
		assert.JSONEq(want, metadata.String)
	}
	metadata, _ := replyLinkState(t, st, plain)
	assert.False(metadata.Valid)
}

func TestResolveEmailRepliesSkipsDamagedParentFacts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	damaged := createThreadEmail(t, f, "damaged", "thread", "damaged@example.test")
	require.NoError(st.SetMessageMetadata(damaged, sql.NullString{String: `{"email_in_reply_to":42}`, Valid: true}))
	parent := createThreadEmail(t, f, "parent", "thread", "parent@example.test")
	child := createThreadEmail(t, f, "child", "thread", "child@example.test")
	require.NoError(st.RecordEmailHeadersContext(t.Context(), f.Source.ID, child, "", "parent@example.test"))

	require.NoError(st.ResolveEmailReplyParentsContext(t.Context(), f.Source.ID, 0, nil))
	_, got := replyLinkState(t, st, child)
	assert.Equal(sql.NullInt64{Int64: parent, Valid: true}, got)
	metadata, got := replyLinkState(t, st, damaged)
	assert.False(got.Valid)
	assert.JSONEq(`{"email_in_reply_to":42}`, metadata.String)
}
