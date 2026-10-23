package rederive

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const emailRaw = "From: Original <original@example.net>\r\nSubject: Archived mail\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<p>Heizk=C3=B6rper</p>"

func archiveEmail(t *testing.T, st *store.Store, src *store.Source, key, body string) int64 {
	t.Helper()
	conv, err := st.EnsureConversation(src.ID, key, "")
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: key, MessageType: "email", SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}, Subject: sql.NullString{String: "Stored subject", Valid: true}, Snippet: sql.NullString{String: "HeizkÃ¶rper", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{String: "<p>HeizkÃ¶rper</p>", Valid: true}))
	require.NoError(t, st.UpsertMessageRaw(id, []byte(emailRaw)))
	require.NoError(t, st.UpsertFTS(id, "Stored subject", body, "", "", ""))
	return id
}

func TestEmailRebuildsArchivedText(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("outlook", "archive@example.net")
	require.NoError(err)
	id := archiveEmail(t, st, src, "old", "HeizkÃ¶rper")
	sender, err := st.EnsureParticipant("repaired@example.net", "Repaired", "example.net")
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Repaired"}))
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET message_type = 'chat', sender_id = ? WHERE id = ?"), sender, id)
	require.NoError(err)
	Heal(t.Context(), slog.New(slog.DiscardHandler), st, src)
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
	}
	assert.Equal(int64(1), derivedDataRevision(t, st))
}

func TestEmailFailedRowRollsBackAndResumes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "retry@example.net")
	require.NoError(err)
	bad := archiveEmail(t, st, src, "bad", "keep me")
	require.NoError(st.UpsertMessageRaw(bad, []byte("invalid MIME")))
	failed := archiveEmail(t, st, src, "failed", "stale")
	last := archiveEmail(t, st, src, "last", "stale")
	fault := fmt.Sprintf(`CREATE TRIGGER fail_email_preview BEFORE UPDATE OF snippet ON messages WHEN NEW.id = %d BEGIN SELECT RAISE(ABORT, 'preview failure'); END`, failed)
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		_, err = st.DB().Exec(fmt.Sprintf(`CREATE FUNCTION fail_email_preview() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id = %d THEN RAISE EXCEPTION 'preview failure'; END IF; RETURN NEW; END $$`, failed))
		require.NoError(err)
		fault = `CREATE TRIGGER fail_email_preview BEFORE UPDATE OF snippet ON messages FOR EACH ROW EXECUTE FUNCTION fail_email_preview()`
	}
	_, err = st.DB().Exec(fault)
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sum, err := Run(ctx, st, src.SourceType, src.Identifier, src.ID, func(string) { cancel() })
	require.ErrorIs(err, context.Canceled)
	body, err := st.GetMessageBodyText(last)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
	assert.Equal(int64(1), sum.Errors)
	assert.Equal(int64(1), sum.Undecodable)
	applied, err := st.IsMigrationApplied(LedgerKey(src.SourceType, src.Identifier, "email-v1"))
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
	_, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	body, err = st.GetMessageBodyText(failed)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
	body, err = st.GetMessageBodyText(bad)
	require.NoError(err)
	assert.Equal("keep me", body)
}

func TestEmailPreservesProviderSnippets(t *testing.T) {
	for _, tc := range []struct{ source, body, snippet, want string }{
		{"mbox", "HeizkÃ¶rper", "HeizkÃ¶rper", "Heizkörper"},
		{"gmail", "HeizkÃ¶rper", "Provider preview", "Provider preview"},
		{"imap", "", "", ""},
	} {
		t.Run(tc.source, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource(tc.source, "archive@example.net")
			require.NoError(err)
			id := archiveEmail(t, st, src, "old", tc.body)
			_, err = st.DB().Exec(st.Rebind("UPDATE messages SET snippet = ? WHERE id = ?"), tc.snippet, id)
			require.NoError(err)
			_, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
			require.NoError(err)
			var snippet string
			require.NoError(st.DB().QueryRow(st.Rebind("SELECT COALESCE(snippet, '') FROM messages WHERE id = ?"), id).Scan(&snippet))
			assert.Equal(t, tc.want, snippet)
		})
	}
}

func TestEmailCurrentTextKeepsCacheAndRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("outlook", "current@example.net")
	require.NoError(err)
	id := archiveEmail(t, st, src, "current", "Heizkörper")
	_, err = st.UpdateMessageDerivedEmailTextContext(t.Context(), id, sql.NullString{String: "Heizkörper", Valid: true}, sql.NullString{String: "<p>Heizkörper</p>", Valid: true}, sql.NullString{String: "Heizkörper", Valid: true})
	require.NoError(err)
	var logs bytes.Buffer
	Heal(t.Context(), slog.New(slog.NewTextHandler(&logs, nil)), st, src)
	assert.Contains(logs.String(), "checked archived message text")
	assert.Zero(derivedDataRevision(t, st))
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET last_modified = '2000-01-01 00:00:00' WHERE id = ?"), id)
	require.NoError(err)
	var after time.Time
	_, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT last_modified FROM messages WHERE id = ?"), id).Scan(&after))
	assert.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), after)
	require.NoError(st.UpsertFTS(id, "Stored subject", "stale search", "", "", ""))
	_, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), derivedDataRevision(t, st))
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "Heizkörper", Valid: true}, sql.NullString{String: "wrong HTML", Valid: true}))
	_, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(2), derivedDataRevision(t, st))
}

func TestEmailPostgresOverflowIsTerminal(t *testing.T) {
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL tsvector overflow")
	}
	require := require.New(t)
	assert := assert.New(t)
	src, err := st.GetOrCreateSource("outlook", "overflow@example.net")
	require.NoError(err)
	id := archiveEmail(t, st, src, "large", "stale")
	raw := "Content-Type: text/plain; charset=UTF-8\r\n\r\n" + strings.Repeat("ö ", 240000) + "sentinel"
	require.NoError(st.UpsertMessageRaw(id, []byte(raw)))
	_, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	_, err = st.DB().Exec(`CREATE FUNCTION fail_large_email_fts() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.search_fts <> ''::tsvector THEN RAISE EXCEPTION USING ERRCODE = '54000', MESSAGE = 'synthetic tsvector overflow'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_large_email_fts BEFORE UPDATE OF search_fts ON messages FOR EACH ROW EXECUTE FUNCTION fail_large_email_fts()`)
	require.NoError(err)
	_, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), derivedDataRevision(t, st))
	require.NoError(st.UpsertMessageRaw(id, []byte(raw+" changed")))
	sum, err := Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Zero(sum.Errors)
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal(strings.TrimPrefix(raw, "Content-Type: text/plain; charset=UTF-8\r\n\r\n")+" changed", body)
	var terminal bool
	require.NoError(st.DB().QueryRow("SELECT search_fts = ''::tsvector AND indexing_version = $1 FROM messages WHERE id = $2", store.CurrentFTSIndexingVersion, id).Scan(&terminal))
	assert.True(terminal)
	_, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(2), derivedDataRevision(t, st))
}
