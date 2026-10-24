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
	require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: "Heizkörper", Valid: true}, sql.NullString{String: "<p>Heizkörper</p>", Valid: true}))
	_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET snippet = ? WHERE id = ?`), "Heizkörper", id)
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
	// Each row needs its MIME/envelope scan and FTS comparison. The four
	// fixed reads check completion, check the retry phase, finish the scan, and
	// check whether a prior interrupted repair still needs a cache revision.
	require.LessOrEqual(reads, 2*32+4, "first-sync repair must share stored-envelope reads with the bounded MIME scan")
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

func TestEmailRetryPublishesRevisionAfterRevisionWriteFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, src, id := archiveEmailSource(t, "outlook", "HeizkÃ¶rper")
	fault := `CREATE TRIGGER fail_email_revision BEFORE INSERT ON archive_metadata WHEN NEW.key = 'derived_data_revision' BEGIN SELECT RAISE(ABORT, 'revision failure'); END`
	drop := `DROP TRIGGER fail_email_revision`
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION fail_email_revision() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key = 'derived_data_revision' THEN RAISE EXCEPTION 'revision failure'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		fault = `CREATE TRIGGER fail_email_revision BEFORE INSERT ON archive_metadata FOR EACH ROW EXECUTE FUNCTION fail_email_revision()`
		drop += ` ON archive_metadata`
	}
	_, err := st.DB().Exec(fault)
	require.NoError(err)
	_, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.Error(err)
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("Heizkörper", body, "the failed revision publication follows a committed text repair")
	assert.Zero(derivedDataRevision(t, st))
	_, err = st.DB().Exec(drop)
	require.NoError(err)
	_, _, err = RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	assert.Positive(derivedDataRevision(t, st), "completion must publish the earlier committed repair to analytics caches")
}

func TestEmailCorruptCompressionDoesNotStopLaterRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"header", []byte("invalid zlib")},
		{"checksum", []byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x02}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, src, bad := archiveEmailSource(t, "outlook", "keep me")
			last := archiveEmail(t, st, src, "later", "stale")
			_, err := st.DB().Exec(st.Rebind(`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`), tc.raw, bad)
			require.NoError(err)
			sum, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
			require.NoError(err)
			assert.Equal(int64(1), sum.Undecodable)
			body, err := st.GetMessageBodyText(last)
			require.NoError(err)
			assert.Equal("Heizkörper", body)
			body, err = st.GetMessageBodyText(bad)
			require.NoError(err)
			assert.Equal("keep me", body)
		})
	}
}

func TestEmailUnchangedRetryPublishesPendingRevisionWithRowErrors(t *testing.T) {
	require := require.New(t)
	st, src, repaired := archiveEmailSource(t, "outlook", "stale")
	failed := archiveEmail(t, st, src, "failed", "stale")
	revisionFault := `CREATE TRIGGER fail_pending_revision BEFORE INSERT ON archive_metadata WHEN NEW.key = 'derived_data_revision' BEGIN SELECT RAISE(ABORT, 'revision failure'); END`
	rowFault := fmt.Sprintf(`CREATE TRIGGER fail_pending_row BEFORE UPDATE OF snippet ON messages WHEN NEW.id = %d BEGIN SELECT RAISE(ABORT, 'snippet failure'); END`, failed)
	dropRevision := `DROP TRIGGER fail_pending_revision`
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION fail_pending_revision() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key = 'derived_data_revision' THEN RAISE EXCEPTION 'revision failure'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(fmt.Sprintf(`CREATE FUNCTION fail_pending_row() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id = %d THEN RAISE EXCEPTION 'snippet failure'; END IF; RETURN NEW; END $$`, failed))
		require.NoError(err)
		revisionFault = `CREATE TRIGGER fail_pending_revision BEFORE INSERT ON archive_metadata FOR EACH ROW EXECUTE FUNCTION fail_pending_revision()`
		rowFault = `CREATE TRIGGER fail_pending_row BEFORE UPDATE OF snippet ON messages FOR EACH ROW EXECUTE FUNCTION fail_pending_row()`
		dropRevision += ` ON archive_metadata`
	}
	_, err := st.DB().Exec(revisionFault)
	require.NoError(err)
	_, err = st.DB().Exec(rowFault)
	require.NoError(err)
	sum, _, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.ErrorContains(err, "revision failure")
	require.Equal(int64(1), sum.Errors)
	body, err := st.GetMessageBodyText(repaired)
	require.NoError(err)
	require.Equal("Heizkörper", body)
	require.Zero(derivedDataRevision(t, st))
	_, err = st.DB().Exec(dropRevision)
	require.NoError(err)
	sum, ran, err := RunIfStale(t.Context(), st, src.SourceType, src.Identifier, src.ID, nil)
	require.NoError(err)
	require.True(ran)
	require.Equal(int64(1), sum.Errors, "the row failure remains active")
	require.Zero(sum.BodiesRewritten, "the retry commits no new text")
	assert.Positive(t, derivedDataRevision(t, st), "the earlier committed repair must reach analytics caches")
	applied, err := st.IsMigrationApplied(LedgerKey(src.SourceType, src.Identifier, emailVersion))
	require.NoError(err)
	assert.False(t, applied, "unresolved row failures keep the pass retryable")
}
