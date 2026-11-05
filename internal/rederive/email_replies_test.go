package rederive

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// archiveThreadMessage stores a row the way an older msgvault left it: raw
// MIME with an In-Reply-To header, but no recorded parent and no link.
func archiveThreadMessage(t *testing.T, st *store.Store, src *store.Source, key, messageType, rfcID, parent string) int64 {
	t.Helper()
	conv, err := st.EnsureConversation(t.Context(), src.ID, "thread", "Thread")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: conv, SourceMessageID: key, MessageType: messageType,
		RFC822MessageID: sql.NullString{String: rfcID, Valid: true},
	})
	require.NoError(t, err)
	raw := "From: sender@example.net\r\nMessage-ID: <" + rfcID + ">\r\n"
	if parent != "" {
		raw += "In-Reply-To: <" + parent + ">\r\n"
	}
	require.NoError(t, st.UpsertMessageRaw(id, []byte(raw+"Subject: Thread\r\n\r\nBody\r\n")))
	return id
}

func replyOf(t *testing.T, st *store.Store, id int64) (sql.NullInt64, sql.NullString) {
	t.Helper()
	var reply sql.NullInt64
	var metadata sql.NullString
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT reply_to_message_id, metadata FROM messages WHERE id = ?`), id).Scan(&reply, &metadata))
	return reply, metadata
}

func emailRepliesState(t *testing.T, st *store.Store, src *store.Source) string {
	t.Helper()
	base := LedgerKey(src.SourceType, src.Identifier, emailRepliesVersion)
	key, err := st.NextMigrationKeyContext(t.Context(), base+"#", "")
	require.NoError(t, err)
	if key == "" {
		return ""
	}
	return key[len(base):]
}

func TestHealBackfillsEmailRepliesFromArchivedMIME(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "archive@example.net")
	require.NoError(err)
	reply := archiveThreadMessage(t, st, src, "reply", store.MessageTypeEmail, "reply@example.net", "parent@example.net")
	parent := archiveThreadMessage(t, st, src, "parent", store.MessageTypeEmail, "parent@example.net", "")
	chat := archiveThreadMessage(t, st, src, "chat", store.MessageTypeGoogleChat, "chat@example.net", "parent@example.net")

	Heal(t.Context(), slog.New(slog.DiscardHandler), st, src)

	got, metadata := replyOf(t, st, reply)
	assert.Equal(sql.NullInt64{Int64: parent, Valid: true}, got)
	assert.JSONEq(`{"email_in_reply_to":"parent@example.net"}`, metadata.String)
	got, metadata = replyOf(t, st, chat)
	assert.False(got.Valid, "chat messages keep their own threading")
	assert.False(metadata.Valid)
	assert.Equal(fmt.Sprintf("#w%016x", chat), emailRepliesState(t, st, src))
}

func TestLinkEmailRepliesResumesHeaderBackfill(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imaps://archive@example.net")
	require.NoError(err)
	archiveThreadMessage(t, st, src, "parent", store.MessageTypeEmail, "parent@example.net", "")
	done := archiveThreadMessage(t, st, src, "done", store.MessageTypeEmail, "done@example.net", "parent@example.net")
	pending := archiveThreadMessage(t, st, src, "pending", store.MessageTypeEmail, "pending@example.net", "parent@example.net")
	base := LedgerKey(src.SourceType, src.Identifier, emailRepliesVersion)
	require.NoError(st.ReplaceMigrationContext(t.Context(), "", fmt.Sprintf("%s#h%016x", base, done)))

	LinkEmailReplies(t.Context(), slog.New(slog.DiscardHandler), st, src)

	_, metadata := replyOf(t, st, done)
	assert.False(metadata.Valid, "an interrupted backfill resumes after its checkpoint")
	reply, metadata := replyOf(t, st, pending)
	assert.True(reply.Valid)
	assert.JSONEq(`{"email_in_reply_to":"parent@example.net"}`, metadata.String)
	assert.Equal(fmt.Sprintf("#w%016x", pending), emailRepliesState(t, st, src))
	assert.Zero(derivedDataRevision(t, st), "reply facts are not part of the analytics cache")
}

func TestLinkEmailRepliesRestartsAfterMalformedState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("mbox", "archive@example.net")
	require.NoError(err)
	parent := archiveThreadMessage(t, st, src, "parent", store.MessageTypeEmail, "parent@example.net", "")
	reply := archiveThreadMessage(t, st, src, "reply", store.MessageTypeEmail, "reply@example.net", "parent@example.net")
	base := LedgerKey(src.SourceType, src.Identifier, emailRepliesVersion)
	require.NoError(st.ReplaceMigrationContext(t.Context(), "", base+"#wbad"))

	var logs bytes.Buffer
	LinkEmailReplies(t.Context(), slog.New(slog.NewTextHandler(&logs, nil)), st, src)
	assert.Contains(logs.String(), "malformed email reply state")
	assert.Empty(emailRepliesState(t, st, src))

	LinkEmailReplies(t.Context(), slog.New(slog.DiscardHandler), st, src)
	got, _ := replyOf(t, st, reply)
	assert.Equal(sql.NullInt64{Int64: parent, Valid: true}, got)
}
