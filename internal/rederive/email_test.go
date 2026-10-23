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

func archiveEmailSource(t *testing.T, sourceType, body string) (*store.Store, *store.Source, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(sourceType, "archive@example.net")
	require.NoError(t, err)
	return st, src, archiveEmail(t, st, src, "old", body)
}

func TestEmailRebuildsArchivedText(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, src, id := archiveEmailSource(t, "gmail", "HeizkÃ¶rper")
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
}

func TestEmailRetriesFailedRowsAndResumesAfterCancellation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, src, bad := archiveEmailSource(t, "imap", "keep me")
	require.NoError(st.UpsertMessageRaw(bad, []byte("invalid MIME")))
	failed := archiveEmail(t, st, src, "failed", "stale")
	last := archiveEmail(t, st, src, "last", "stale")
	query := fmt.Sprintf(`CREATE TRIGGER fail_retry_snippet BEFORE UPDATE OF snippet ON messages WHEN NEW.id = %d BEGIN SELECT RAISE(ABORT, 'snippet failure'); END`, failed)
	drop := "DROP TRIGGER fail_retry_snippet"
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(fmt.Sprintf(`CREATE FUNCTION fail_retry_snippet() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id = %d THEN RAISE EXCEPTION 'snippet failure'; END IF; RETURN NEW; END $$`, failed))
		require.NoError(err)
		query = `CREATE TRIGGER fail_retry_snippet BEFORE UPDATE OF snippet ON messages FOR EACH ROW EXECUTE FUNCTION fail_retry_snippet()`
		drop += " ON messages"
	}
	_, err := st.DB().Exec(query)
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sum, err := Run(ctx, st, src.SourceType, src.Identifier, src.ID, func(string) { cancel() })
	require.ErrorIs(err, context.Canceled)
	body, err := st.GetMessageBodyText(last)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
	assert.Equal(int64(1), sum.Errors)
	assert.Equal(int64(1), sum.BodiesRewritten)
	assert.Equal(int64(1), sum.Undecodable)
	applied, err := st.IsMigrationApplied(LedgerKey(src.SourceType, src.Identifier, emailVersion))
	require.NoError(err)
	assert.False(applied)
	sum, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(3), sum.MessagesScanned, "an interrupted full walk restarts")
	assert.Equal(int64(1), sum.Errors)
	sum, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.MessagesScanned, "a sync retries only unresolved MIME rows")
	_, err = st.DB().Exec(drop)
	require.NoError(err)
	sum, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(1), sum.MessagesScanned)
	assert.Zero(sum.Errors)
	body, err = st.GetMessageBodyText(failed)
	require.NoError(err)
	assert.Equal("Heizkörper", body)
	body, err = st.GetMessageBodyText(bad)
	require.NoError(err)
	assert.Equal("keep me", body)
	sum, err = Run(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Equal(int64(3), sum.MessagesScanned, "manual repair still inspects the whole archive")
}

func archiveCurrentEmail(t *testing.T, st *store.Store, src *store.Source, key string) {
	t.Helper()
	id := archiveEmail(t, st, src, key, "Heizkörper")
	_, err := st.UpdateMessageDerivedEmailTextContext(t.Context(), id, sql.NullString{String: "Heizkörper", Valid: true}, sql.NullString{String: "<p>Heizkörper</p>", Valid: true}, sql.NullString{String: "Heizkörper", Valid: true})
	require.NoError(t, err)
}

func TestEmailFirstSyncBoundsReadQueries(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("outlook", "archive@example.net")
	require.NoError(err)
	for i := range 32 {
		archiveCurrentEmail(t, st, src, strconv.Itoa(i))
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
	// Each row needs its MIME/envelope scan and FTS comparison. The three
	// fixed reads check completion, check the retry phase, and finish the scan.
	require.LessOrEqual(reads, 2*32+3, "first-sync repair must share stored-envelope reads with the bounded MIME scan")
}

func TestEmailPreservesProviderSnippets(t *testing.T) {
	for _, tc := range []struct{ name, source, body, snippet, want string }{
		{"body-derived", "mbox", "HeizkÃ¶rper", "HeizkÃ¶rper", "Heizkörper"},
		{"provider-set", "gmail", "HeizkÃ¶rper", "Provider preview", "Provider preview"},
		{"empty", "imap", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

func TestEmailCurrentTextKeepsCache(t *testing.T) {
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("outlook", "archive@example.net")
	require.NoError(t, err)
	archiveCurrentEmail(t, st, src, "current")
	var logs bytes.Buffer
	Heal(t.Context(), slog.New(slog.NewTextHandler(&logs, nil)), st, src)
	assert.Contains(t, logs.String(), "checked archived message text")
	assert.Zero(t, derivedDataRevision(t, st))
}
