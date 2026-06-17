package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func newRFC822Message(
	t *testing.T, f *storetest.Fixture, sourceMessageID, rfc822ID string,
) int64 {
	t.Helper()
	id, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  f.ConvID,
		SourceID:        f.Source.ID,
		SourceMessageID: sourceMessageID,
		RFC822MessageID: sql.NullString{
			String: rfc822ID, Valid: rfc822ID != "",
		},
		MessageType:  "email",
		SizeEstimate: 1000,
	})
	requirepkg.NoError(t, err, "UpsertMessage")
	return id
}

func TestStore_FindDuplicatesByRFC822ID(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	idA := newRFC822Message(t, f, "src-a", "rfc822-shared")
	idB := newRFC822Message(t, f, "src-b", "rfc822-shared")
	_ = newRFC822Message(t, f, "src-c", "rfc822-unique")

	groups, err := f.Store.FindDuplicatesByRFC822ID()
	require.NoError(err, "FindDuplicatesByRFC822ID")
	require.Len(groups, 1)
	assert.Equal("rfc822-shared", groups[0].RFC822MessageID, "key")
	assert.Equal(2, groups[0].Count, "count")

	_, err = f.Store.MergeDuplicates(idA, []int64{idB}, "batch-test")
	require.NoError(err, "MergeDuplicates")

	groups, err = f.Store.FindDuplicatesByRFC822ID()
	require.NoError(err, "FindDuplicatesByRFC822ID after merge")
	assert.Empty(groups, "groups after merge")
}

func TestStore_GetDuplicateGroupMessages_SentLabel(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	idInbox := newRFC822Message(t, f, "inbox-copy", "rfc822-sent")
	idSent := newRFC822Message(t, f, "sent-copy", "rfc822-sent")

	labels := f.EnsureLabels(
		map[string]string{"SENT": "Sent", "INBOX": "Inbox"}, "system",
	)
	require.NoError(f.Store.LinkMessageLabel(idInbox, labels["INBOX"]), "link INBOX")
	require.NoError(f.Store.LinkMessageLabel(idSent, labels["SENT"]), "link SENT")

	rows, err := f.Store.GetDuplicateGroupMessages("rfc822-sent")
	require.NoError(err, "GetDuplicateGroupMessages")
	require.Len(rows, 2)

	var sentRow, inboxRow *store.DuplicateMessageRow
	for i := range rows {
		switch rows[i].ID {
		case idSent:
			sentRow = &rows[i]
		case idInbox:
			inboxRow = &rows[i]
		}
	}
	require.NotNil(sentRow, "sent row missing")
	require.NotNil(inboxRow, "inbox row missing")
	assert.True(sentRow.HasSentLabel, "sent row: HasSentLabel")
	assert.False(inboxRow.HasSentLabel, "inbox row: HasSentLabel")
}

func TestStore_MergeDuplicates_UnionsLabels(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	idKeep := newRFC822Message(t, f, "keep", "rfc822-merge")
	idDrop := newRFC822Message(t, f, "drop", "rfc822-merge")

	labels := f.EnsureLabels(
		map[string]string{"INBOX": "Inbox", "IMPORTANT": "Important", "WORK": "Work"}, "user",
	)
	require.NoError(f.Store.LinkMessageLabel(idKeep, labels["INBOX"]), "link INBOX to keep")
	require.NoError(f.Store.LinkMessageLabel(idDrop, labels["IMPORTANT"]), "link IMPORTANT to drop")
	require.NoError(f.Store.LinkMessageLabel(idDrop, labels["WORK"]), "link WORK to drop")

	result, err := f.Store.MergeDuplicates(idKeep, []int64{idDrop}, "batch-labels")
	require.NoError(err, "MergeDuplicates")
	assert.Equal(2, result.LabelsTransferred, "labelsTransferred")

	f.AssertLabelCount(idKeep, 3)
	assertDedupDeleted(t, f.Store, idDrop, true)

	restored, err := f.Store.UndoDedup("batch-labels")
	require.NoError(err, "UndoDedup")
	assert.Equal(int64(1), restored, "restored")
	assertDedupDeleted(t, f.Store, idDrop, false)
}

func assertDedupDeleted(
	t *testing.T, st *store.Store, msgID int64, wantDeleted bool,
) {
	t.Helper()
	var deletedAt sql.NullTime
	err := st.DB().QueryRow(
		st.Rebind("SELECT deleted_at FROM messages WHERE id = ?"), msgID,
	).Scan(&deletedAt)
	requirepkg.NoError(t, err, "query deleted_at")
	if wantDeleted {
		assertpkg.True(t, deletedAt.Valid, "message %d: deleted_at should be set", msgID)
	} else {
		assertpkg.False(t, deletedAt.Valid, "message %d: deleted_at should be NULL", msgID)
	}
}

func TestStore_BackfillRFC822IDs_EmptyTable(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	count, err := f.Store.CountMessagesWithoutRFC822ID()
	require.NoError(err, "CountMessagesWithoutRFC822ID")
	assert.Equal(int64(0), count, "empty-table count")

	updated, _, err := f.Store.BackfillRFC822IDs(nil, nil)
	require.NoError(err, "BackfillRFC822IDs")
	assert.Equal(int64(0), updated, "updated")
}

func TestStore_CountActiveMessages(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	_ = newRFC822Message(t, f, "a", "id-a")
	idB := newRFC822Message(t, f, "b", "id-b")

	total, err := f.Store.CountActiveMessages()
	require.NoError(err, "CountActiveMessages")
	assert.Equal(int64(2), total, "active")

	_, err = f.Store.MergeDuplicates(
		newRFC822Message(t, f, "c", "id-c"),
		[]int64{idB},
		"batch-count",
	)
	require.NoError(err, "MergeDuplicates")

	total, err = f.Store.CountActiveMessages()
	require.NoError(err, "CountActiveMessages after merge")
	assert.Equal(int64(2), total, "active after merge")
}

func TestStore_BackfillRFC822IDs_ParsesFromRawMIME(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	id := newRFC822Message(t, f, "needs-backfill", "")

	rawMIME := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <unique-123@example.com>\r\nSubject: Backfill test\r\n\r\nBody text")
	require.NoError(f.Store.UpsertMessageRaw(id, rawMIME),
		"UpsertMessageRaw",
	)

	count, err := f.Store.CountMessagesWithoutRFC822ID()
	require.NoError(err, "CountMessagesWithoutRFC822ID")
	require.Equal(int64(1), count, "count without rfc822")

	updated, _, err := f.Store.BackfillRFC822IDs(nil, nil)
	require.NoError(err, "BackfillRFC822IDs")
	require.Equal(int64(1), updated, "updated")

	var rfc822ID string
	err = f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT rfc822_message_id FROM messages WHERE id = ?"), id,
	).Scan(&rfc822ID)
	require.NoError(err, "scan rfc822_message_id")
	assert.Equal("unique-123@example.com", rfc822ID, "rfc822_message_id")

	count, err = f.Store.CountMessagesWithoutRFC822ID()
	require.NoError(err, "CountMessagesWithoutRFC822ID after backfill")
	assert.Equal(int64(0), count, "count after backfill")
}

func TestStore_BackfillRFC822IDs_DoesNotOvercountRolledBackBatch(t *testing.T) {
	require := requirepkg.New(t)
	testutil.SkipIfPostgres(t, "uses SQLite-specific CREATE TRIGGER ... NEW.* / RAISE(FAIL,...) syntax to force a mid-batch rollback")
	f := storetest.New(t)

	idA := newRFC822Message(t, f, "needs-backfill-a", "")
	idB := newRFC822Message(t, f, "needs-backfill-b", "")

	rawA := []byte("From: alice@example.com\r\nMessage-ID: <unique-a@example.com>\r\n\r\nBody")
	rawB := []byte("From: bob@example.com\r\nMessage-ID: <unique-b@example.com>\r\n\r\nBody")
	require.NoError(f.Store.UpsertMessageRaw(idA, rawA), "UpsertMessageRaw A")
	require.NoError(f.Store.UpsertMessageRaw(idB, rawB), "UpsertMessageRaw B")

	_, err := f.Store.DB().Exec(fmt.Sprintf(`
		CREATE TRIGGER fail_backfill_second_message
		BEFORE UPDATE OF rfc822_message_id ON messages
		WHEN NEW.id = %d
		BEGIN
			SELECT RAISE(FAIL, 'forced backfill failure');
		END
	`, idB))
	require.NoError(err, "create trigger")

	updated, failed, err := f.Store.BackfillRFC822IDs(nil, nil)
	require.Error(err, "expected backfill error")
	require.Equal(int64(0), updated, "updated after rollback")
	require.Equal(int64(0), failed, "failed")

	var count int64
	err = f.Store.DB().QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE rfc822_message_id IS NOT NULL AND rfc822_message_id != ''
	`).Scan(&count)
	require.NoError(err, "count backfilled rows")
	require.Equal(int64(0), count, "backfilled rows after rollback")
}

func TestStore_MergeDuplicates_BackfillsRawMIME(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	idSurvivor := newRFC822Message(t, f, "survivor", "rfc822-mime-backfill")
	idDuplicate := newRFC822Message(t, f, "duplicate", "rfc822-mime-backfill")

	rawData := []byte("From: alice@example.com\r\nSubject: Test\r\n\r\nBody")
	require.NoError(f.Store.UpsertMessageRaw(idDuplicate, rawData),
		"UpsertMessageRaw on duplicate",
	)

	_, err := f.Store.GetMessageRaw(idSurvivor)
	require.Error(err, "survivor should not have raw MIME before merge")

	result, err := f.Store.MergeDuplicates(
		idSurvivor, []int64{idDuplicate}, "batch-mime",
	)
	require.NoError(err, "MergeDuplicates")
	assert.Equal(1, result.RawMIMEBackfilled, "RawMIMEBackfilled")

	got, err := f.Store.GetMessageRaw(idSurvivor)
	require.NoError(err, "GetMessageRaw survivor after merge")
	assert.NotEmpty(got, "survivor raw MIME should not be empty after backfill")
}

// TestStore_GetDuplicateGroupMessages_PreservesFromCase verifies that the
// FromEmail field returned by GetDuplicateGroupMessages preserves the
// original case of the sender's address. The query layer must NOT
// blanket-lowercase the address — synthetic identifiers like Matrix
// MXIDs (`@Alice:matrix.org`) and chat handles are case-sensitive in
// the rest of the identity subsystem (NormalizeIdentifierForCompare
// preserves case for non-email shapes), so any pre-lowering in SQL
// would prevent dedup's per-source identity match from finding a
// stored case-mixed identity. Regression test for iter12 codex Medium.
func TestStore_GetDuplicateGroupMessages_PreservesFromCase(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)

	mxid := "@Alice:matrix.org"
	pid := f.EnsureParticipant(mxid, "", "")

	id := newRFC822Message(t, f, "msg-mxid", "rfc822-mxid")

	_, err := f.Store.DB().Exec(
		f.Store.Rebind(`INSERT INTO message_recipients
			(message_id, participant_id, recipient_type)
			VALUES (?, ?, 'from')`),
		id, pid,
	)
	require.NoError(err, "insert from recipient")

	rows, err := f.Store.GetDuplicateGroupMessages("rfc822-mxid")
	require.NoError(err, "GetDuplicateGroupMessages")
	require.Len(rows, 1)
	assertpkg.Equal(t, mxid, rows[0].FromEmail, "FromEmail (case must be preserved)")
}

// TestStore_GetAllRawMIMECandidates_PreservesFromCase mirrors
// TestStore_GetDuplicateGroupMessages_PreservesFromCase but covers the
// content-hash candidate path. Both queries had the same SQL `LOWER()`
// problem before iter12; both fixes need regression coverage so a
// future refactor that reintroduces lowercasing in either query is
// caught. Iter13 claude follow-up.
func TestStore_GetAllRawMIMECandidates_PreservesFromCase(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)

	mxid := "@Bob:matrix.org"
	pid := f.EnsureParticipant(mxid, "", "")

	id := newRFC822Message(t, f, "msg-mxid-raw", "rfc822-mxid-raw")

	_, err := f.Store.DB().Exec(
		f.Store.Rebind(`INSERT INTO message_recipients
			(message_id, participant_id, recipient_type)
			VALUES (?, ?, 'from')`),
		id, pid,
	)
	require.NoError(err, "insert from recipient")

	// GetAllRawMIMECandidates only returns messages that have a raw
	// MIME row, so synthesize one.
	require.NoError(f.Store.UpsertMessageRaw(id, []byte("From: "+mxid+"\r\n\r\nbody")),
		"UpsertMessageRaw",
	)

	cands, err := f.Store.GetAllRawMIMECandidates()
	require.NoError(err, "GetAllRawMIMECandidates")
	var got *store.ContentHashCandidate
	for i := range cands {
		if cands[i].ID == id {
			got = &cands[i]
			break
		}
	}
	require.NotNil(got, "test message %d not in candidates: %+v", id, cands)
	assertpkg.Equal(t, mxid, got.FromEmail, "FromEmail (case must be preserved)")
}
