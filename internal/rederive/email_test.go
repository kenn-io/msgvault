package rederive

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"strconv"
	"testing"

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
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: key, MessageType: kind, Subject: sql.NullString{String: "Stored subject", Valid: true}, Snippet: sql.NullString{String: "HeizkÃ¶rper", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{String: "<p>HeizkÃ¶rper</p>", Valid: true}))
	require.NoError(t, st.UpsertMessageRaw(id, []byte(raw)))
	require.NoError(t, st.UpsertFTS(id, "Stored subject", body, "", "", ""))
	return id
}

func TestEmailRebuildsArchivedText(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "asserts SQLite FTS body and address columns")
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "archive@example.net")
	require.NoError(err)
	id := archiveEmail(t, st, src, "old", "email", "HeizkÃ¶rper", emailRaw)
	// Stored sender data must win over the older MIME envelope.
	sender, err := st.EnsureParticipant("repaired@example.net", "Repaired", "example.net")
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Repaired"}))
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET sender_id = ? WHERE id = ?"), sender, id)
	require.NoError(err)
	var matches int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'body:Heizkörper'").Scan(&matches))
	assert.Zero(matches)
	sum, ran, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	require.True(ran)
	assert.Equal(int64(1), sum.BodiesRewritten)
	var body, html, snippet, from, subject string
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text, body_html FROM message_bodies WHERE message_id = ?"), id).Scan(&body, &html))
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT snippet FROM messages WHERE id = ?"), id).Scan(&snippet))
	assert.Equal("Heizkörper", body)
	assert.Equal("<p>Heizkörper</p>", html)
	assert.Equal("Heizkörper", snippet)
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'body:Heizkörper'").Scan(&matches))
	assert.Equal(1, matches)
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT from_addr, subject FROM messages_fts WHERE rowid = ?"), id).Scan(&from, &subject))
	assert.Contains(from, "repaired@example.net")
	assert.Equal("Stored subject", subject)
	_, ran, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.False(ran)
}

func TestEmailRepairsCurrentBodyAndChatAndSkipsUndecodable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "removes and queries the SQLite FTS row")
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "archive@example.net")
	require.NoError(err)
	id := archiveEmail(t, st, src, "chat", "chat", "Heizkörper", emailRaw)
	bad := archiveEmail(t, st, src, "bad", "email", "keep me", "invalid MIME")
	_, err = st.DB().Exec(st.Rebind("DELETE FROM messages_fts WHERE rowid = ?"), id)
	require.NoError(err)
	sum, err := Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(2), sum.MessagesScanned)
	assert.Equal(int64(1), sum.Undecodable)
	var snippet string
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT snippet FROM messages WHERE id = ?"), id).Scan(&snippet))
	assert.Equal("Heizkörper", snippet)
	var matches int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'body:Heizkörper'").Scan(&matches))
	assert.Equal(1, matches)
	body, err := st.GetMessageBodyText(bad)
	require.NoError(err)
	assert.Equal("keep me", body)
}

func TestEmailResumeAfterInterruption(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "resume@example.net")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(src.ID, `{"provider_cursor":"keep"}`))
	var last int64
	for i := range 501 {
		last = archiveEmail(t, st, src, strconv.Itoa(i), "email", "stale", emailRaw)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sum, err := Run(ctx, st, src.SourceType, src.Identifier, src.ID, func(string) { cancel() })
	require.ErrorIs(err, context.Canceled)
	assert.Equal(int64(500), sum.MessagesScanned)
	applied, err := st.IsMigrationApplied(LedgerKey(src.SourceType, src.Identifier, "v1"))
	require.NoError(err)
	assert.False(applied)
	src, err = st.GetSourceByID(src.ID)
	require.NoError(err)
	var config map[string]any
	require.NoError(json.Unmarshal([]byte(src.SyncConfig.String), &config))
	assert.Equal("keep", config["provider_cursor"])
	sum, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.MessagesScanned)
	body, err := st.GetMessageBodyText(last)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
	// A completed explicit repair can start again from the beginning.
	sum, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(501), sum.MessagesScanned)
}

func TestHealRunsEmailPassOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "heal@example.net")
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
	testutil.SkipIfPostgres(t, "injects a SQLite trigger failure")
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "retry@example.net")
	require.NoError(err)
	first := archiveEmail(t, st, src, "first", "email", "stale", emailRaw)
	failed := archiveEmail(t, st, src, "failed", "email", "stale", emailRaw)
	_, err = st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER fail_email_preview BEFORE UPDATE OF snippet ON messages WHEN NEW.id = %d BEGIN SELECT RAISE(ABORT, 'preview failure'); END`, failed))
	require.NoError(err)
	sum, err := Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.ErrorContains(err, "preview failure")
	assert.Equal(int64(1), sum.Errors)
	body, err := st.GetMessageBodyText(first)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
	body, err = st.GetMessageBodyText(failed)
	require.NoError(err)
	assert.Equal("stale", body, "failed preview must roll back the preceding body write")
	_, err = st.DB().Exec("DROP TRIGGER fail_email_preview")
	require.NoError(err)
	sum, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.MessagesScanned, "retry must start at the failed row within the page")
	body, err = st.GetMessageBodyText(failed)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
}
