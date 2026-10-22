package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// linkageOutcome returns a message's outcome, or "pending" while it is NULL.
func linkageOutcome(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	var outcome *string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT email_linkage_outcome FROM messages WHERE id = ?`), id).Scan(&outcome))
	if outcome == nil {
		return "pending"
	}
	return *outcome
}

func linkageKeys(t *testing.T, st *store.Store, id int64) map[string][]string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`SELECT kind, value FROM email_link_keys WHERE message_id = ? ORDER BY kind, value`), id)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	result := map[string][]string{}
	for rows.Next() {
		var kind, value string
		require.NoError(t, rows.Scan(&kind, &value))
		result[kind] = append(result[kind], value)
	}
	require.NoError(t, rows.Err())
	return result
}

func TestEmailLinkagePersistAndRawReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "gmail", "linkage@example.test")
	raw := "Message-ID: <Self@example.test>\r\nIn-Reply-To: <reply@example.test>\r\nReferences: <root@example.test> <reply@example.test>\r\n\r\nBody"
	id := f.persist(attrMail{raw: raw})
	assert.Equal("complete", linkageOutcome(t, f.st, id))
	assert.Equal(map[string][]string{"reply": {"reply@example.test"}, "reference": {"reply@example.test", "root@example.test"}}, linkageKeys(t, f.st, id))
	for range 2 {
		require.NoError(f.st.UpsertMessageRawWithFormat(id, []byte("In-Reply-To: <replacement@example.test>\r\n\r\n"), "mime"))
		assert.Equal(map[string][]string{"reply": {"replacement@example.test"}}, linkageKeys(t, f.st, id))
	}
	require.NoError(f.st.UpsertMessageRawWithFormat(id, []byte(`{"text":"chat"}`), "chat_json"))
	assert.Empty(linkageKeys(t, f.st, id))
	assert.Equal("unsupported_raw", linkageOutcome(t, f.st, id))
}

func TestEmailLinkagePersistWithoutRawStaysPending(t *testing.T) {
	f := newAttrFixture(t, "gmail", "no-raw@example.test")
	id := f.persist(attrMail{})
	assert.Equal(t, "pending", linkageOutcome(t, f.st, id))
}

// Other message types have no linkage, so persisting one reads and writes nothing for it.
func TestEmailLinkageSkipsNonEmailWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "chat@example.test")
	id, err := f.st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "chat-1", MessageType: "whatsapp"},
		RawMIME: []byte("In-Reply-To: <parent@example.test>\r\n\r\n"),
	})
	require.NoError(err)
	assert.Empty(linkageKeys(t, f.st, id))
	assert.Equal("pending", linkageOutcome(t, f.st, id), "the row is never derived or counted as pending work")
	require.NoError(f.st.UpsertMessageRawWithFormat(id, []byte(`{"text":"hi"}`), "chat_json"))
	assert.Equal("pending", linkageOutcome(t, f.st, id))
}

func TestEmailLinkagePersistenceRollsBack(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "gmail", "rollback@example.test")
	_, err := f.st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "rollback", MessageType: "email"},
		RawMIME: []byte("In-Reply-To: <rollback@example.test>\r\n\r\n"), LabelIDs: []int64{9999999},
	})
	require.Error(err)
	var count int
	require.NoError(f.st.DB().QueryRow(`SELECT COUNT(*) FROM email_link_keys`).Scan(&count))
	assert.Zero(count)
}

func TestEmailLinkageRepairCoverageAndScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "repair@example.test")
	other := newAttrFixtureOn(t, f.st, "imap", "other@example.test")
	raw := "In-Reply-To: <parent@example.test>\r\n\r\n"
	ids := make([]int64, 0, 5)
	for range 5 {
		ids = append(ids, f.persist(attrMail{raw: raw}))
	}
	outside := other.persist(attrMail{raw: raw})
	_, err := f.st.DB().Exec(`DELETE FROM email_link_keys`)
	require.NoError(err)
	_, err = f.st.DB().Exec(`UPDATE messages SET email_linkage_outcome = NULL`)
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), ids[0])
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE message_raw SET raw_data = ? WHERE message_id = ?`), []byte("corrupt zlib"), ids[1])
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE message_raw SET raw_format = 'chat_json' WHERE message_id = ?`), ids[2])
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET message_type = 'chat' WHERE id = ?`), ids[3])
	require.NoError(err)
	sum, err := f.st.RepairEmailLinkageContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.EqualValues(4, sum.Scanned)
	assert.EqualValues(3, sum.Incomplete)
	for i, want := range []string{"missing_raw", "invalid_raw", "unsupported_raw", "pending", "complete"} {
		assert.Equal(want, linkageOutcome(t, f.st, ids[i]), i)
	}
	assert.Equal(map[string][]string{"reply": {"parent@example.test"}}, linkageKeys(t, f.st, ids[4]))
	assert.Equal("pending", linkageOutcome(t, f.st, outside), "another source stays pending")
	sum, err = f.st.RepairEmailLinkageContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.Zero(sum.Scanned, "a second run finds nothing pending")
	sum, err = f.st.RepairEmailLinkageContext(t.Context(), other.source.ID, nil)
	require.NoError(err)
	assert.EqualValues(1, sum.Scanned)
}

// A key the column cannot hold must not fail the write: the email is marked
// unreadable instead, on persist and on repair, on SQLite and PostgreSQL.
func TestEmailLinkageControlBytesMarkInvalidHeaders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "nul@example.test")
	raw := "In-Reply-To: <a\x00b@x.test> <ok@x.test>\r\n\r\n"
	id := f.persist(attrMail{raw: raw})
	assert.Equal("invalid_headers", linkageOutcome(t, f.st, id))
	assert.Equal(map[string][]string{"reply": {"ok@x.test"}}, linkageKeys(t, f.st, id))

	_, err := f.st.DB().Exec(f.st.Rebind(`DELETE FROM email_link_keys WHERE message_id = ?`), id)
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET email_linkage_outcome = NULL WHERE id = ?`), id)
	require.NoError(err)
	sum, err := f.st.RepairEmailLinkageContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.EqualValues(1, sum.Incomplete)
	assert.Equal("invalid_headers", linkageOutcome(t, f.st, id))
}

func TestEmailLinkageRepairResumesAtCommittedPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "pages@example.test")
	for i := range 501 {
		_, err := f.st.DB().Exec(f.st.Rebind(`INSERT INTO messages (source_id, conversation_id, source_message_id, message_type) VALUES (?, ?, ?, 'email')`), f.source.ID, f.conv, fmt.Sprintf("pending-%d", i))
		require.NoError(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	sum, err := f.st.RepairEmailLinkageContext(ctx, f.source.ID, func(store.EmailLinkageRepairSummary) { cancel() })
	require.ErrorIs(err, context.Canceled)
	assert.EqualValues(500, sum.Scanned)
	sum, err = f.st.RepairEmailLinkageContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.EqualValues(1, sum.Scanned)
}

// Every sync asks for a source's pending emails, so once a source is derived
// the lookup must not scan it.
func TestEmailLinkagePendingLookupUsesPartialIndex(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("EXPLAIN QUERY PLAN is SQLite-specific; the PostgreSQL index build is covered by the concurrent index tests")
	}
	f := newAttrFixtureOn(t, st, "mbox", "archive-1")
	for range 200 {
		f.persist(attrMail{raw: "In-Reply-To: <p@example.test>\r\n\r\nbody"})
	}
	_, err := st.DB().Exec(`ANALYZE`)
	require.NoError(err)
	rows, err := st.DB().Query(`EXPLAIN QUERY PLAN `+store.EmailLinkagePendingQueryForTest, f.source.ID, 0, 500)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(rows.Err())
	assert.Contains(t, strings.Join(plan, "\n"), "idx_messages_email_linkage_pending")
}

func TestEmailLinkageDedupBackfillRefreshesSurvivor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "dedup@example.test")
	survivor := f.persist(attrMail{})
	duplicate := f.persist(attrMail{raw: "In-Reply-To: <shared@example.test>\r\n\r\n"})
	require.Equal("pending", linkageOutcome(t, f.st, survivor))
	result, err := f.st.MergeDuplicates(survivor, []int64{duplicate}, "email-linkage")
	require.NoError(err)
	assert.Equal(1, result.RawMIMEBackfilled)
	assert.Equal("complete", linkageOutcome(t, f.st, survivor))
	assert.Equal(map[string][]string{"reply": {"shared@example.test"}}, linkageKeys(t, f.st, survivor))
}

func TestEmailLinkageSubsetPreservesKeys(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("subset copies SQLite files")
	}
	srcPath := filepath.Join(t.TempDir(), "source.db")
	st, err := store.Open(srcPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchemaContext(t.Context()))
	f := newAttrFixtureOn(t, st, "imap", "subset@example.test")
	id := f.persist(attrMail{raw: "In-Reply-To: <copied@example.test>\r\nReferences: <ancestor@example.test>\r\n\r\n"})
	dstDir := t.TempDir()
	_, err = store.CopySubset(srcPath, dstDir, 1, false)
	require.NoError(err)
	dst, err := store.Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(dst.Close()) })
	assert.Equal(linkageKeys(t, st, id), linkageKeys(t, dst, id))
	assert.Equal(linkageOutcome(t, st, id), linkageOutcome(t, dst, id))
}

// An archive from before the column: every email starts pending and the
// repair derives it.
func TestEmailLinkageSchemaUpgrade(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "upgrade@example.test")
	id := f.persist(attrMail{raw: "In-Reply-To: <retained@example.test>\r\n\r\n"})
	_, err := f.st.DB().Exec(`DROP TABLE email_link_keys`)
	require.NoError(err)
	_, err = f.st.DB().Exec(`DROP INDEX idx_messages_email_linkage_pending`)
	require.NoError(err)
	_, err = f.st.DB().Exec(`ALTER TABLE messages DROP COLUMN email_linkage_outcome`)
	require.NoError(err)
	_, err = f.st.DB().Exec(`UPDATE archive_metadata SET value = '10' WHERE key='schema_version'`)
	require.NoError(err)
	require.NoError(f.st.InitSchemaContext(t.Context()))
	assert.Equal("pending", linkageOutcome(t, f.st, id))
	sum, err := f.st.RepairEmailLinkageContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.EqualValues(1, sum.Scanned)
	assert.Equal(map[string][]string{"reply": {"retained@example.test"}}, linkageKeys(t, f.st, id))
}

func TestEmailLinkageRepairOrdersMessageBeforeRawPG(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := requirePostgreSQLStore(t)
	f := newAttrFixtureOn(t, st, "imap", "ordering@example.test")
	id := f.persist(attrMail{sourceMsgKey: "ordered", raw: "In-Reply-To: <before@example.test>\r\n\r\n"})
	_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET email_linkage_outcome = NULL WHERE id = ?`), id)
	require.NoError(err)
	held, err := st.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	t.Cleanup(func() { _ = held.Rollback() })
	_, err = held.ExecContext(t.Context(), `SELECT id FROM messages WHERE id = $1 FOR UPDATE`, id)
	require.NoError(err)
	repairDone := make(chan error, 1)
	go func() { _, err := st.RepairEmailLinkageContext(t.Context(), f.source.ID, nil); repairDone <- err }()
	pid := waitForLockWait(t, st, "FROM messages WHERE id =", "repair must reach the held message row")
	assert.False(holdsRelationLock(t, st, pid, "message_raw"), "raw is not read until the message row is locked")
	persistDone := make(chan error, 1)
	go func() {
		_, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "ordered", MessageType: "email"}, RawMIME: []byte("In-Reply-To: <after@example.test>\r\n\r\n")})
		persistDone <- err
	}()
	waitForLockWait(t, st, "UPDATE sources SET updated_at", "persist waits on repair's source gate")
	require.NoError(held.Commit())
	assertNoDeadlock(t, waitResult(t, repairDone, "linkage repair"))
	assertNoDeadlock(t, waitResult(t, persistDone, "concurrent persist"))
	assert.Equal(map[string][]string{"reply": {"after@example.test"}}, linkageKeys(t, st, id))
}
