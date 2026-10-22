package store_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func linkageState(t *testing.T, st *store.Store, id int64) (int, string, string) {
	t.Helper()
	var version int
	var outcome, hash string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT extraction_version, outcome, raw_sha256 FROM email_link_key_state WHERE message_id = ?`), id).Scan(&version, &outcome, &hash))
	return version, outcome, hash
}

func linkageKeys(t *testing.T, st *store.Store, id int64) map[string][]string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`SELECT kind, value FROM email_link_keys WHERE message_id = ? ORDER BY kind, ordinal`), id)
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
	version, outcome, hash := linkageState(t, f.st, id)
	assert.Equal(mime.EmailLinkageVersion, version)
	assert.Equal("complete", outcome)
	assert.Equal(fmt.Sprintf("%x", sha256.Sum256([]byte(raw))), hash)
	assert.Equal(map[string][]string{"self": {"Self@example.test"}, "reply": {"reply@example.test"}, "reference": {"root@example.test", "reply@example.test"}}, linkageKeys(t, f.st, id))
	for range 2 {
		require.NoError(f.st.UpsertMessageRawWithFormat(id, []byte("Message-ID: <replacement@example.test>\r\n\r\n"), "mime"))
		assert.Equal(map[string][]string{"self": {"replacement@example.test"}}, linkageKeys(t, f.st, id))
	}
	require.NoError(f.st.UpsertMessageRawWithFormat(id, []byte(`{"text":"chat"}`), "chat_json"))
	assert.Empty(linkageKeys(t, f.st, id))
	_, outcome, _ = linkageState(t, f.st, id)
	assert.Equal("unsupported_raw", outcome)
}

func TestEmailLinkagePersistenceRollsBack(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "gmail", "rollback@example.test")
	_, err := f.st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "rollback", MessageType: "email"},
		RawMIME: []byte("Message-ID: <rollback@example.test>\r\n\r\n"), LabelIDs: []int64{9999999},
	})
	require.Error(err)
	var count int
	require.NoError(f.st.DB().QueryRow(`SELECT COUNT(*) FROM email_link_key_state`).Scan(&count))
	assert.Zero(count)
	require.NoError(f.st.DB().QueryRow(`SELECT COUNT(*) FROM email_link_keys`).Scan(&count))
	assert.Zero(count)
}

func TestEmailLinkageRepairCoverageAndScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "repair@example.test")
	other := newAttrFixtureOn(t, f.st, "imap", "other@example.test")
	ids := make([]int64, 0, 6)
	for range 6 {
		ids = append(ids, f.persist(attrMail{raw: "Message-ID: <repair@example.test>\r\n\r\n"}))
	}
	outside := other.persist(attrMail{raw: "Message-ID: <outside@example.test>\r\n\r\n"})
	_, err := f.st.DB().Exec(`DELETE FROM email_link_keys`)
	require.NoError(err)
	_, err = f.st.DB().Exec(`DELETE FROM email_link_key_state`)
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), ids[0])
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE message_raw SET encryption_version = 1 WHERE message_id = ?`), ids[1])
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE message_raw SET raw_data = ? WHERE message_id = ?`), []byte("corrupt zlib"), ids[2])
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE message_raw SET raw_format = 'chat_json' WHERE message_id = ?`), ids[3])
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET message_type = '' WHERE id = ?`), ids[4])
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	var checkpoints int
	sum, err := f.st.RepairEmailLinkageContext(ctx, f.source.ID, func(store.EmailLinkageRepairSummary) { checkpoints++; cancel() })
	require.ErrorIs(err, context.Canceled)
	assert.EqualValues(6, sum.Scanned)
	assert.EqualValues(4, sum.Incomplete)
	assert.Equal(1, checkpoints)
	for i, want := range []string{"missing_raw", "encrypted_raw", "invalid_raw", "unsupported_raw", "complete", "complete"} {
		_, got, _ := linkageState(t, f.st, ids[i])
		assert.Equal(want, got)
	}
	var count int
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM email_link_key_state WHERE message_id = ?`), outside).Scan(&count))
	assert.Zero(count)
	sum, err = f.st.RepairEmailLinkageContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.Zero(sum.Scanned)
	sum, err = f.st.RepairEmailLinkageContext(t.Context(), other.source.ID, nil)
	require.NoError(err)
	assert.EqualValues(1, sum.Scanned)
}

func TestEmailLinkageRepairReprocessesPreviousExtractorVersion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const previousExtractorVersion = 1
	assert.Greater(mime.EmailLinkageVersion, previousExtractorVersion)

	f := newAttrFixture(t, "imap", "old-linkage@example.test")
	raw := "Content-Type: multipart/mixed; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: message/rfc822\r\n \n" +
		"X-Pad: " + strings.Repeat("x", 256<<10) +
		"\n\nMessage-ID: <original@example.test>\r\n\r\nBody\r\n--x--\r\n"
	id := f.persist(attrMail{raw: raw})
	_, outcome, _ := linkageState(t, f.st, id)
	assert.Equal("header_limit", outcome)

	_, err := f.st.DB().Exec(f.st.Rebind(`UPDATE email_link_key_state SET extraction_version = ?, outcome = 'complete' WHERE message_id = ?`), previousExtractorVersion, id)
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`INSERT INTO email_link_keys (message_id, kind, ordinal, value) VALUES (?, 'attached', 0, 'original@example.test')`), id)
	require.NoError(err)
	assert.Equal(map[string][]string{"attached": {"original@example.test"}}, linkageKeys(t, f.st, id))
	sum, err := f.st.RepairEmailLinkageContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.EqualValues(1, sum.Scanned)
	version, outcome, _ := linkageState(t, f.st, id)
	assert.Equal(mime.EmailLinkageVersion, version)
	assert.Equal("header_limit", outcome)
	assert.Empty(linkageKeys(t, f.st, id))
}

func TestEmailLinkageRepairCheckpointAdvancesAndResetsForExtractorVersion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "checkpoint@example.test")
	first := f.persist(attrMail{raw: "Message-ID: <checkpoint@example.test>\r\n\r\n"})

	sum, err := f.st.RepairEmailLinkageContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.Zero(sum.Scanned)
	var checkpointVersion int
	var checkpointID int64
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT extraction_version, last_message_id FROM email_link_key_repair_progress WHERE source_id = ?`), f.source.ID).Scan(&checkpointVersion, &checkpointID))
	assert.Equal(mime.EmailLinkageVersion, checkpointVersion)
	assert.Equal(first, checkpointID)

	var pending int64
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`INSERT INTO messages (source_id, conversation_id, source_message_id, message_type) VALUES (?, ?, ?, 'email') RETURNING id`), f.source.ID, f.conv, "pending-after-checkpoint").Scan(&pending))
	assert.Greater(pending, checkpointID)
	sum, err = f.st.RepairEmailLinkageContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.EqualValues(1, sum.Scanned)
	assert.EqualValues(1, sum.Incomplete)
	_, outcome, _ := linkageState(t, f.st, pending)
	assert.Equal("missing_raw", outcome)

	staleVersion := mime.EmailLinkageVersion - 1
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE email_link_key_repair_progress SET extraction_version = ? WHERE source_id = ?`), staleVersion, f.source.ID)
	require.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE email_link_key_state SET extraction_version = ? WHERE message_id IN (?, ?)`), staleVersion, first, pending)
	require.NoError(err)
	sum, err = f.st.RepairEmailLinkageContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.EqualValues(2, sum.Scanned)
	assert.EqualValues(1, sum.Incomplete)
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT extraction_version, last_message_id FROM email_link_key_repair_progress WHERE source_id = ?`), f.source.ID).Scan(&checkpointVersion, &checkpointID))
	assert.Equal(mime.EmailLinkageVersion, checkpointVersion)
	assert.Equal(pending, checkpointID)
}

func TestEmailLinkageRepairCheckpointInvalidatesAfterSourceMerge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	from := newAttrFixtureOn(t, st, "gmail", "history@example.test")
	into := newAttrFixtureOn(t, st, "gmail", "live@example.test")
	moved := from.persist(attrMail{raw: "Message-ID: <history@example.test>\r\n\r\n"})
	destination := into.persist(attrMail{raw: "Message-ID: <live@example.test>\r\n\r\n"})
	assert.Greater(destination, moved)

	_, err := st.RepairEmailLinkageContext(t.Context(), into.source.ID, nil)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM email_link_keys WHERE message_id = ?`), moved)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM email_link_key_state WHERE message_id = ?`), moved)
	require.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.source.ID, IntoSourceID: into.source.ID})
	require.NoError(err)

	sum, err := st.RepairEmailLinkageContext(t.Context(), into.source.ID, nil)
	require.NoError(err)
	assert.EqualValues(1, sum.Scanned)
	_, outcome, _ := linkageState(t, st, moved)
	assert.Equal("complete", outcome)
	assert.Equal(map[string][]string{"self": {"history@example.test"}}, linkageKeys(t, st, moved))
}

func TestEmailLinkageNonEmailRemovesStaleKeys(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "nonemail@example.test")
	id := f.persist(attrMail{raw: "Message-ID: <old@example.test>\r\n\r\n"})
	_, err := f.st.DB().Exec(f.st.Rebind(`UPDATE messages SET message_type = 'chat' WHERE id = ?`), id)
	require.NoError(err)
	require.NoError(f.st.UpsertMessageRawWithFormat(id, []byte("Message-ID: <not-email@example.test>\r\n\r\n"), "mime"))
	assert.Empty(linkageKeys(t, f.st, id))
	var count int
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM email_link_key_state WHERE message_id = ?`), id).Scan(&count))
	assert.Zero(count)
}

func TestEmailLinkageDedupBackfillsRaw(t *testing.T) {
	f := newAttrFixture(t, "imap", "dedup@example.test")
	survivor := f.persist(attrMail{})
	duplicate := f.persist(attrMail{raw: "Message-ID: <shared@example.test>\r\n\r\n"})
	result, err := f.st.MergeDuplicates(survivor, []int64{duplicate}, "email-linkage")
	require.NoError(t, err)
	assert.Equal(t, 1, result.RawMIMEBackfilled)
	assert.Equal(t, map[string][]string{"self": {"shared@example.test"}}, linkageKeys(t, f.st, survivor))
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
	id := f.persist(attrMail{raw: "Message-ID: <copied@example.test>\r\nReferences: <ancestor@example.test>\r\n\r\n"})
	dstDir := t.TempDir()
	_, err = store.CopySubset(srcPath, dstDir, 1, false)
	require.NoError(err)
	dst, err := store.Open(filepath.Join(dstDir, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(dst.Close()) })
	assert.Equal(linkageKeys(t, st, id), linkageKeys(t, dst, id))
	_, outcome, hash := linkageState(t, st, id)
	_, copiedOutcome, copiedHash := linkageState(t, dst, id)
	assert.Equal(outcome, copiedOutcome)
	assert.Equal(hash, copiedHash)
}

func TestEmailLinkageSchemaUpgrade(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", "upgrade@example.test")
	id := f.persist(attrMail{raw: "Message-ID: <retained@example.test>\r\n\r\n"})
	_, err := f.st.DB().Exec(`DROP TABLE email_link_keys`)
	require.NoError(err)
	_, err = f.st.DB().Exec(`DROP TABLE email_link_key_state`)
	require.NoError(err)
	_, err = f.st.DB().Exec(`UPDATE archive_metadata SET value = '10' WHERE key='schema_version'`)
	require.NoError(err)
	require.NoError(f.st.InitSchemaContext(t.Context()))
	sum, err := f.st.RepairEmailLinkageContext(t.Context(), f.source.ID, nil)
	require.NoError(err)
	assert.EqualValues(1, sum.Scanned)
	assert.Equal(map[string][]string{"self": {"retained@example.test"}}, linkageKeys(t, f.st, id))
}

func TestEmailLinkageUnchangedRawDoesNotRewriteKeys(t *testing.T) {
	f := newAttrFixture(t, "imap", "noop@example.test")
	if f.st.IsPostgreSQL() {
		t.Skip("SQLite abort triggers observe writes; shared refresh logic is covered on both backends")
	}
	raw := []byte("Message-ID: <stable@example.test>\r\n\r\nBody")
	id := f.persist(attrMail{raw: string(raw)})
	for _, table := range []string{"email_link_keys", "email_link_key_state"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			_, err := f.st.DB().Exec(fmt.Sprintf(`CREATE TRIGGER guard_%s_%s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT,'unchanged linkage must not rewrite rows'); END`, table, event, event, table))
			require.NoError(t, err)
		}
	}
	require.NoError(t, f.st.UpsertMessageRawWithFormat(id, raw, "mime"))
	assert.Equal(t, map[string][]string{"self": {"stable@example.test"}}, linkageKeys(t, f.st, id))
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

func TestEmailLinkageRepairOrdersMessageBeforeRawPG(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := requirePostgreSQLStore(t)
	f := newAttrFixtureOn(t, st, "imap", "ordering@example.test")
	id := f.persist(attrMail{sourceMsgKey: "ordered", raw: "Message-ID: <before@example.test>\r\n\r\n"})
	_, err := st.DB().Exec(st.Rebind(`DELETE FROM email_link_key_state WHERE message_id = ?`), id)
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
		_, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: f.source.ID, ConversationID: f.conv, SourceMessageID: "ordered", MessageType: "email"}, RawMIME: []byte("Message-ID: <after@example.test>\r\n\r\n")})
		persistDone <- err
	}()
	waitForLockWait(t, st, "UPDATE sources SET updated_at", "persist waits on repair's source gate")
	require.NoError(held.Commit())
	assertNoDeadlock(t, waitResult(t, repairDone, "linkage repair"))
	assertNoDeadlock(t, waitResult(t, persistDone, "concurrent persist"))
	assert.Equal(map[string][]string{"self": {"after@example.test"}}, linkageKeys(t, st, id))
}
