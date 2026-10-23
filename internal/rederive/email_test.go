package rederive

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
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

func archiveOutlookEmail(t *testing.T, body string) (*store.Store, *store.Source, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("outlook", "archive@example.net")
	require.NoError(t, err)
	return st, src, archiveEmail(t, st, src, "old", body)
}

func TestEmailRebuildsArchivedText(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, src, id := archiveOutlookEmail(t, "HeizkÃ¶rper")
	sender, err := st.EnsureParticipant("repaired@example.net", "Repaired", "example.net")
	require.NoError(err)
	require.NoError(st.ReplaceMessageRecipients(id, "from", []int64{sender}, []string{"Repaired"}))
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET message_type = 'chat', sender_id = ? WHERE id = ?"), sender, id)
	require.NoError(err)
	Heal(t.Context(), slog.New(slog.DiscardHandler), st, src)
	var body, html string
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text, body_html FROM message_bodies WHERE message_id = ?"), id).Scan(&body, &html))
	assert.Equal("Heizkörper", body)
	assert.Equal("<p>Heizkörper</p>", html)
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
	unblock := blockEmailSnippet(t, st, failed)
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
	unblock()
	_, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	body, err = st.GetMessageBodyText(failed)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
	body, err = st.GetMessageBodyText(bad)
	require.NoError(err)
	assert.Equal("keep me", body)
}

func TestEmailRetryOnlyUnresolvedRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, src, failed := archiveOutlookEmail(t, "stale")
	archiveEmail(t, st, src, "later", "stale")
	unblock := blockEmailSnippet(t, st, failed)
	sum, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.Errors)
	assert.Equal(int64(1), sum.BodiesRewritten)
	sum, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.MessagesScanned, "a sync retries only unresolved MIME rows")
	unblock()
	sum, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.MessagesScanned)
	assert.Zero(sum.Errors)
	sum, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(2), sum.MessagesScanned, "manual repair still inspects the whole archive")
}

func TestEmailFirstSyncBoundsReadQueries(t *testing.T) {
	require := require.New(t)
	st, src, id := archiveOutlookEmail(t, "Heizkörper")
	for i := range 32 {
		if i > 0 {
			id = archiveEmail(t, st, src, strconv.Itoa(i), "Heizkörper")
		}
		_, err := st.UpdateMessageDerivedEmailTextContext(t.Context(), id, sql.NullString{String: "Heizkörper", Valid: true}, sql.NullString{String: "<p>Heizkörper</p>", Valid: true}, sql.NullString{String: "Heizkörper", Valid: true})
		require.NoError(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true})
	t.Cleanup(func() {
		slog.SetDefault(previous)
		store.ConfigureSQLLogging(store.SQLLogOptions{})
	})
	sum, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	require.Equal(int64(32), sum.MessagesScanned)
	reads := strings.Count(logs.String(), `"kind":"query`)
	require.LessOrEqual(reads, 2*32+8, "first-sync repair must share stored-envelope reads with the bounded MIME scan")
}

func blockEmailSnippet(t *testing.T, st *store.Store, id int64) func() {
	t.Helper()
	query := fmt.Sprintf(`CREATE TRIGGER fail_retry_snippet BEFORE UPDATE OF snippet ON messages WHEN NEW.id = %d BEGIN SELECT RAISE(ABORT, 'snippet failure'); END`, id)
	drop := "DROP TRIGGER fail_retry_snippet"
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(fmt.Sprintf(`CREATE FUNCTION fail_retry_snippet() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id = %d THEN RAISE EXCEPTION 'snippet failure'; END IF; RETURN NEW; END $$`, id))
		require.NoError(t, err)
		query = `CREATE TRIGGER fail_retry_snippet BEFORE UPDATE OF snippet ON messages FOR EACH ROW EXECUTE FUNCTION fail_retry_snippet()`
		drop += " ON messages"
	}
	_, err := st.DB().Exec(query)
	require.NoError(t, err)
	return func() {
		_, err := st.DB().Exec(drop)
		require.NoError(t, err)
	}
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
	st, src, id := archiveOutlookEmail(t, "Heizkörper")
	_, err := st.UpdateMessageDerivedEmailTextContext(t.Context(), id, sql.NullString{String: "Heizkörper", Valid: true}, sql.NullString{String: "<p>Heizkörper</p>", Valid: true}, sql.NullString{String: "Heizkörper", Valid: true})
	require.NoError(err)
	var logs bytes.Buffer
	Heal(t.Context(), slog.New(slog.NewTextHandler(&logs, nil)), st, src)
	assert.Contains(logs.String(), "checked archived message text")
	assert.Zero(derivedDataRevision(t, st))
	modified := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET last_modified = ? WHERE id = ?"), modified, id)
	require.NoError(err)
	var after time.Time
	_, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT last_modified FROM messages WHERE id = ?"), id).Scan(&after))
	assert.Equal(modified, after.UTC())
	require.NoError(st.UpsertFTS(id, "Stored subject", "stale search", "", "", ""))
	_, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), derivedDataRevision(t, st))
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "Heizkörper", Valid: true}, sql.NullString{String: "wrong HTML", Valid: true}))
	_, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(2), derivedDataRevision(t, st))
}

func archiveLargeEmail(t *testing.T, st *store.Store) (*store.Source, int64, string) {
	t.Helper()
	src, err := st.GetOrCreateSource("outlook", "overflow@example.net")
	require.NoError(t, err)
	var body strings.Builder
	for i := range 140000 {
		fmt.Fprintf(&body, "%c%c%c%c%c ", 'А'+i%16, 'А'+i/16%16, 'А'+i/256%16, 'А'+i/4096%16, 'А'+i/65536%16)
	}
	id := archiveEmail(t, st, src, "large", "stale")
	raw := "Content-Type: text/plain; charset=UTF-8\r\n\r\n" + strings.TrimSpace(body.String())
	require.NoError(t, st.UpsertMessageRaw(id, []byte(raw)))
	return src, id, raw
}

func TestEmailPostgresOverflowIsTerminal(t *testing.T) {
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL tsvector overflow")
	}
	require := require.New(t)
	assert := assert.New(t)
	src, id, raw := archiveLargeEmail(t, st)
	_, err := Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	_, found, err := st.SearchMessages("Stored", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), found, "repair must preserve the searchable subject of retained mail")
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
