package rederive

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const emailRaw = "From: Original <original@example.net>\r\nSubject: Archived mail\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<p>Heizk=C3=B6rper</p>"

func archiveEmail(t *testing.T, st *store.Store, src *store.Source, key, kind, body, raw string) int64 {
	t.Helper()
	conv, err := st.EnsureConversation(src.ID, key, "")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: key, MessageType: kind, SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}, Subject: sql.NullString{String: "Stored subject", Valid: true}, Snippet: sql.NullString{String: "HeizkÃ¶rper", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{String: "<p>HeizkÃ¶rper</p>", Valid: true}))
	require.NoError(t, st.UpsertMessageRaw(id, []byte(raw)))
	require.NoError(t, st.UpsertFTS(id, "Stored subject", body, "", "", ""))
	return id
}

func TestEmailRebuildsArchivedText(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("outlook", "archive@example.net")
	require.NoError(err)
	id := archiveEmail(t, st, src, "old", "email", "HeizkÃ¶rper", emailRaw)
	sender, err := st.EnsureParticipant("repaired@example.net", "Repaired", "example.net")
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Repaired"}))
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET sender_id = ? WHERE id = ?"), sender, id)
	require.NoError(err)
	_, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	var body, html, snippet string
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text, body_html FROM message_bodies WHERE message_id = ?"), id).Scan(&body, &html))
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT snippet FROM messages WHERE id = ?"), id).Scan(&snippet))
	assert.Equal("Heizkörper", body)
	assert.Equal("<p>Heizkörper</p>", html)
	assert.Equal("Heizkörper", snippet)
	for _, query := range []string{"Heizkörper", "Stored", "repaired"} {
		matches, _, err := st.SearchMessages(query, 0, 10)
		require.NoError(err)
		require.Len(matches, 1, query)
		assert.Equal(id, matches[0].ID)
	}
	var conv int64
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT conversation_id FROM messages WHERE id = ?"), id).Scan(&conv))
	var preview sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT last_message_preview FROM conversations WHERE id = ?"), conv).Scan(&preview))
	assert.False(preview.Valid)
	newer, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "newer", MessageType: "email", SentAt: sql.NullTime{Time: time.Now(), Valid: true}})
	require.NoError(err)
	deleted, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "deleted", MessageType: "email", SentAt: sql.NullTime{Time: time.Now().Add(time.Hour), Valid: true}})
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET deleted_at = CURRENT_TIMESTAMP, snippet = 'deleted' WHERE id = ?"), deleted)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind("UPDATE conversations SET last_message_preview = 'stale' WHERE id = ?"), conv)
	require.NoError(err)
	require.NoError(st.UpdateMessageDerivedEmailTextContext(t.Context(), newer, sql.NullString{}, sql.NullString{}, sql.NullString{String: "Latest", Valid: true}))
	require.NoError(st.UpdateMessageDerivedEmailTextContext(t.Context(), id, sql.NullString{}, sql.NullString{}, sql.NullString{String: "Older", Valid: true}))
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT last_message_preview FROM conversations WHERE id = ?"), conv).Scan(&preview))
	assert.Equal("Latest", preview.String)
}

func TestEmailRepairsCurrentBodyAndChatAndSkipsUndecodable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "archive@example.net")
	require.NoError(err)
	id := archiveEmail(t, st, src, "chat", "chat", "Heizkörper", emailRaw)
	bad := archiveEmail(t, st, src, "bad", "email", "keep me", "invalid MIME")
	clearSearch := "DELETE FROM messages_fts WHERE rowid = ?"
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		clearSearch = "UPDATE messages SET search_fts = NULL WHERE id = ?"
	}
	_, err = st.DB().Exec(st.Rebind(clearSearch), id)
	require.NoError(err)
	sum, err := Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(2), sum.MessagesScanned)
	assert.Equal(int64(1), sum.Undecodable)
	assert.Zero(sum.BodiesRewritten)
	matches, _, err := st.SearchMessages("Heizkörper", 0, 10)
	require.NoError(err)
	require.Len(matches, 1)
	body, err := st.GetMessageBodyText(bad)
	require.NoError(err)
	assert.Equal("keep me", body)
}

func TestEmailRetryIncludesMergedOlderHistory(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	older, err := st.GetOrCreateSource("imap", "older@example.net")
	require.NoError(err)
	oldID := archiveEmail(t, st, older, "older", "email", "stale", emailRaw)
	src, err := st.GetOrCreateSource("imap", "resume@example.net")
	require.NoError(err)
	first := archiveEmail(t, st, src, "first", "email", "stale", emailRaw)
	last := archiveEmail(t, st, src, "last", "email", "stale", emailRaw)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sum, err := Run(ctx, st, src.SourceType, src.Identifier, src.ID, func(string) { cancel() })
	require.ErrorIs(err, context.Canceled)
	assert.Equal(int64(1), sum.MessagesScanned)
	applied, err := st.IsMigrationApplied(LedgerKey(src.SourceType, src.Identifier, "v1"))
	require.NoError(err)
	assert.False(applied)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: older.ID, IntoSourceID: src.ID})
	require.NoError(err)
	sum, ran, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.True(ran)
	assert.Equal(int64(3), sum.MessagesScanned)
	for _, id := range []int64{oldID, first, last} {
		body, err := st.GetMessageBodyText(id)
		require.NoError(err)
		assert.Equal("Heizkörper", body)
	}
}

func TestHealRunsEmailPassOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("outlook", "heal@example.net")
	require.NoError(err)
	id := archiveEmail(t, st, src, "old", "email", "stale", emailRaw)
	logger := slog.New(slog.DiscardHandler)
	Heal(t.Context(), logger, st, src)
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "later", Valid: true}, sql.NullString{}))
	Heal(t.Context(), logger, st, src)
	body, err = st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("later", body)
}

func TestEmailFailedRowRollsBackAndResumes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "retry@example.net")
	require.NoError(err)
	first := archiveEmail(t, st, src, "first", "email", "stale", emailRaw)
	failed := archiveEmail(t, st, src, "failed", "email", "stale", emailRaw)
	last := archiveEmail(t, st, src, "last", "email", "stale", emailRaw)
	fault := fmt.Sprintf(`CREATE TRIGGER fail_email_preview BEFORE UPDATE OF snippet ON messages WHEN NEW.id = %d BEGIN SELECT RAISE(ABORT, 'preview failure'); END`, failed)
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		_, err = st.DB().Exec(fmt.Sprintf(`CREATE FUNCTION fail_email_preview() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id = %d THEN RAISE EXCEPTION 'preview failure'; END IF; RETURN NEW; END $$`, failed))
		require.NoError(err)
		fault = `CREATE TRIGGER fail_email_preview BEFORE UPDATE OF snippet ON messages FOR EACH ROW EXECUTE FUNCTION fail_email_preview()`
	}
	_, err = st.DB().Exec(fault)
	require.NoError(err)
	sum, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(3), sum.MessagesScanned)
	body, err := st.GetMessageBodyText(last)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
	assert.Equal(int64(1), sum.Errors)
	applied, err := st.IsMigrationApplied(LedgerKey(src.SourceType, src.Identifier, "v1"))
	require.NoError(err)
	assert.False(applied)
	body, err = st.GetMessageBodyText(failed)
	require.NoError(err)
	assert.Equal("stale", body, "failed preview must roll back the preceding body write")
	drop := "DROP TRIGGER fail_email_preview"
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		drop += " ON messages"
	}
	_, err = st.DB().Exec(drop)
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(first, sql.NullString{String: "stale", Valid: true}, sql.NullString{}))
	_, err = st.DB().Exec(st.Rebind("UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?"), []byte("corrupt"), last)
	require.NoError(err)
	_, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.Error(err)
	body, err = st.GetMessageBodyText(first)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
	applied, err = st.IsMigrationApplied(LedgerKey(src.SourceType, src.Identifier, "v1"))
	require.NoError(err)
	assert.False(applied)
	require.NoError(st.UpsertMessageRaw(last, []byte(emailRaw)))
	sum, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(3), sum.MessagesScanned)
	body, err = st.GetMessageBodyText(failed)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
}
