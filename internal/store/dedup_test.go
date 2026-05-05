package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/testutil"
	"github.com/wesm/msgvault/internal/testutil/storetest"
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
	testutil.MustNoErr(t, err, "UpsertMessage")
	return id
}

func TestStore_FindDuplicatesByRFC822ID(t *testing.T) {
	f := storetest.New(t)
	idA := newRFC822Message(t, f, "src-a", "rfc822-shared")
	idB := newRFC822Message(t, f, "src-b", "rfc822-shared")
	_ = newRFC822Message(t, f, "src-c", "rfc822-unique")

	groups, err := f.Store.FindDuplicatesByRFC822ID()
	testutil.MustNoErr(t, err, "FindDuplicatesByRFC822ID")
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if groups[0].RFC822MessageID != "rfc822-shared" {
		t.Errorf("key = %q, want rfc822-shared", groups[0].RFC822MessageID)
	}
	if groups[0].Count != 2 {
		t.Errorf("count = %d, want 2", groups[0].Count)
	}

	_, err = f.Store.MergeDuplicates(idA, []int64{idB}, "batch-test")
	testutil.MustNoErr(t, err, "MergeDuplicates")

	groups, err = f.Store.FindDuplicatesByRFC822ID()
	testutil.MustNoErr(t, err, "FindDuplicatesByRFC822ID after merge")
	if len(groups) != 0 {
		t.Errorf("groups after merge = %d, want 0", len(groups))
	}
}

func TestStore_GetDuplicateGroupMessages_SentLabel(t *testing.T) {
	f := storetest.New(t)
	idInbox := newRFC822Message(t, f, "inbox-copy", "rfc822-sent")
	idSent := newRFC822Message(t, f, "sent-copy", "rfc822-sent")

	labels := f.EnsureLabels(
		map[string]string{"SENT": "Sent", "INBOX": "Inbox"}, "system",
	)
	testutil.MustNoErr(t, f.Store.LinkMessageLabel(idInbox, labels["INBOX"]), "link INBOX")
	testutil.MustNoErr(t, f.Store.LinkMessageLabel(idSent, labels["SENT"]), "link SENT")

	rows, err := f.Store.GetDuplicateGroupMessages("rfc822-sent")
	testutil.MustNoErr(t, err, "GetDuplicateGroupMessages")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}

	var sentRow, inboxRow *store.DuplicateMessageRow
	for i := range rows {
		switch rows[i].ID {
		case idSent:
			sentRow = &rows[i]
		case idInbox:
			inboxRow = &rows[i]
		}
	}
	if sentRow == nil || inboxRow == nil {
		t.Fatalf("missing rows: sent=%v inbox=%v", sentRow, inboxRow)
	}
	if !sentRow.HasSentLabel {
		t.Errorf("sent row: HasSentLabel = false, want true")
	}
	if inboxRow.HasSentLabel {
		t.Errorf("inbox row: HasSentLabel = true, want false")
	}
}

func TestStore_MergeDuplicates_UnionsLabels(t *testing.T) {
	f := storetest.New(t)
	idKeep := newRFC822Message(t, f, "keep", "rfc822-merge")
	idDrop := newRFC822Message(t, f, "drop", "rfc822-merge")

	labels := f.EnsureLabels(
		map[string]string{"INBOX": "Inbox", "IMPORTANT": "Important", "WORK": "Work"}, "user",
	)
	testutil.MustNoErr(t, f.Store.LinkMessageLabel(idKeep, labels["INBOX"]), "link INBOX to keep")
	testutil.MustNoErr(t, f.Store.LinkMessageLabel(idDrop, labels["IMPORTANT"]), "link IMPORTANT to drop")
	testutil.MustNoErr(t, f.Store.LinkMessageLabel(idDrop, labels["WORK"]), "link WORK to drop")

	result, err := f.Store.MergeDuplicates(idKeep, []int64{idDrop}, "batch-labels")
	testutil.MustNoErr(t, err, "MergeDuplicates")
	if result.LabelsTransferred != 2 {
		t.Errorf("labelsTransferred = %d, want 2", result.LabelsTransferred)
	}

	f.AssertLabelCount(idKeep, 3)
	assertDedupDeleted(t, f.Store, idDrop, true)

	restored, err := f.Store.UndoDedup("batch-labels")
	testutil.MustNoErr(t, err, "UndoDedup")
	if restored != 1 {
		t.Errorf("restored = %d, want 1", restored)
	}
	assertDedupDeleted(t, f.Store, idDrop, false)
}

func assertDedupDeleted(
	t *testing.T, st *store.Store, msgID int64, wantDeleted bool,
) {
	t.Helper()
	var deletedAt sql.NullTime
	err := st.DB().QueryRow(
		"SELECT deleted_at FROM messages WHERE id = ?", msgID,
	).Scan(&deletedAt)
	testutil.MustNoErr(t, err, "query deleted_at")
	if wantDeleted && !deletedAt.Valid {
		t.Errorf("message %d: deleted_at should be set", msgID)
	}
	if !wantDeleted && deletedAt.Valid {
		t.Errorf("message %d: deleted_at should be NULL", msgID)
	}
}

func TestStore_BackfillRFC822IDs_EmptyTable(t *testing.T) {
	f := storetest.New(t)
	count, err := f.Store.CountMessagesWithoutRFC822ID()
	testutil.MustNoErr(t, err, "CountMessagesWithoutRFC822ID")
	if count != 0 {
		t.Errorf("empty-table count = %d, want 0", count)
	}

	updated, _, err := f.Store.BackfillRFC822IDs(nil, nil)
	testutil.MustNoErr(t, err, "BackfillRFC822IDs")
	if updated != 0 {
		t.Errorf("updated = %d, want 0", updated)
	}
}

func TestStore_CountActiveMessages(t *testing.T) {
	f := storetest.New(t)
	_ = newRFC822Message(t, f, "a", "id-a")
	idB := newRFC822Message(t, f, "b", "id-b")

	total, err := f.Store.CountActiveMessages()
	testutil.MustNoErr(t, err, "CountActiveMessages")
	if total != 2 {
		t.Errorf("active = %d, want 2", total)
	}

	_, err = f.Store.MergeDuplicates(
		newRFC822Message(t, f, "c", "id-c"),
		[]int64{idB},
		"batch-count",
	)
	testutil.MustNoErr(t, err, "MergeDuplicates")

	total, err = f.Store.CountActiveMessages()
	testutil.MustNoErr(t, err, "CountActiveMessages after merge")
	if total != 2 {
		t.Errorf("active after merge = %d, want 2", total)
	}
}

func TestStore_BackfillRFC822IDs_ParsesFromRawMIME(t *testing.T) {
	f := storetest.New(t)

	id := newRFC822Message(t, f, "needs-backfill", "")

	rawMIME := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <unique-123@example.com>\r\nSubject: Backfill test\r\n\r\nBody text")
	testutil.MustNoErr(t,
		f.Store.UpsertMessageRaw(id, rawMIME),
		"UpsertMessageRaw",
	)

	count, err := f.Store.CountMessagesWithoutRFC822ID()
	testutil.MustNoErr(t, err, "CountMessagesWithoutRFC822ID")
	if count != 1 {
		t.Fatalf("count without rfc822 = %d, want 1", count)
	}

	updated, _, err := f.Store.BackfillRFC822IDs(nil, nil)
	testutil.MustNoErr(t, err, "BackfillRFC822IDs")
	if updated != 1 {
		t.Fatalf("updated = %d, want 1", updated)
	}

	var rfc822ID string
	err = f.Store.DB().QueryRow(
		"SELECT rfc822_message_id FROM messages WHERE id = ?", id,
	).Scan(&rfc822ID)
	testutil.MustNoErr(t, err, "scan rfc822_message_id")
	if rfc822ID != "unique-123@example.com" {
		t.Errorf("rfc822_message_id = %q, want unique-123@example.com", rfc822ID)
	}

	count, err = f.Store.CountMessagesWithoutRFC822ID()
	testutil.MustNoErr(t, err, "CountMessagesWithoutRFC822ID after backfill")
	if count != 0 {
		t.Errorf("count after backfill = %d, want 0", count)
	}
}

func TestStore_BackfillRFC822IDs_DoesNotOvercountRolledBackBatch(t *testing.T) {
	f := storetest.New(t)

	idA := newRFC822Message(t, f, "needs-backfill-a", "")
	idB := newRFC822Message(t, f, "needs-backfill-b", "")

	rawA := []byte("From: alice@example.com\r\nMessage-ID: <unique-a@example.com>\r\n\r\nBody")
	rawB := []byte("From: bob@example.com\r\nMessage-ID: <unique-b@example.com>\r\n\r\nBody")
	testutil.MustNoErr(t, f.Store.UpsertMessageRaw(idA, rawA), "UpsertMessageRaw A")
	testutil.MustNoErr(t, f.Store.UpsertMessageRaw(idB, rawB), "UpsertMessageRaw B")

	_, err := f.Store.DB().Exec(fmt.Sprintf(`
		CREATE TRIGGER fail_backfill_second_message
		BEFORE UPDATE OF rfc822_message_id ON messages
		WHEN NEW.id = %d
		BEGIN
			SELECT RAISE(FAIL, 'forced backfill failure');
		END
	`, idB))
	testutil.MustNoErr(t, err, "create trigger")

	updated, failed, err := f.Store.BackfillRFC822IDs(nil, nil)
	if err == nil {
		t.Fatal("expected backfill error, got nil")
	}
	if updated != 0 {
		t.Fatalf("updated = %d, want 0 after rollback", updated)
	}
	if failed != 0 {
		t.Fatalf("failed = %d, want 0", failed)
	}

	var count int64
	err = f.Store.DB().QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE rfc822_message_id IS NOT NULL AND rfc822_message_id != ''
	`).Scan(&count)
	testutil.MustNoErr(t, err, "count backfilled rows")
	if count != 0 {
		t.Fatalf("backfilled rows = %d, want 0 after rollback", count)
	}
}

func TestStore_MergeDuplicates_BackfillsRawMIME(t *testing.T) {
	f := storetest.New(t)

	idSurvivor := newRFC822Message(t, f, "survivor", "rfc822-mime-backfill")
	idDuplicate := newRFC822Message(t, f, "duplicate", "rfc822-mime-backfill")

	rawData := []byte("From: alice@example.com\r\nSubject: Test\r\n\r\nBody")
	testutil.MustNoErr(t,
		f.Store.UpsertMessageRaw(idDuplicate, rawData),
		"UpsertMessageRaw on duplicate",
	)

	_, err := f.Store.GetMessageRaw(idSurvivor)
	if err == nil {
		t.Fatal("survivor should not have raw MIME before merge")
	}

	result, err := f.Store.MergeDuplicates(
		idSurvivor, []int64{idDuplicate}, "batch-mime",
	)
	testutil.MustNoErr(t, err, "MergeDuplicates")
	if result.RawMIMEBackfilled != 1 {
		t.Errorf("RawMIMEBackfilled = %d, want 1", result.RawMIMEBackfilled)
	}

	got, err := f.Store.GetMessageRaw(idSurvivor)
	testutil.MustNoErr(t, err, "GetMessageRaw survivor after merge")
	if len(got) == 0 {
		t.Error("survivor raw MIME should not be empty after backfill")
	}
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
	f := storetest.New(t)

	mxid := "@Alice:matrix.org"
	pid := f.EnsureParticipant(mxid, "", "")

	id := newRFC822Message(t, f, "msg-mxid", "rfc822-mxid")

	if _, err := f.Store.DB().Exec(
		f.Store.Rebind(`INSERT INTO message_recipients
			(message_id, participant_id, recipient_type)
			VALUES (?, ?, 'from')`),
		id, pid,
	); err != nil {
		t.Fatalf("insert from recipient: %v", err)
	}

	rows, err := f.Store.GetDuplicateGroupMessages("rfc822-mxid")
	testutil.MustNoErr(t, err, "GetDuplicateGroupMessages")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].FromEmail != mxid {
		t.Errorf("FromEmail = %q, want %q (case must be preserved)", rows[0].FromEmail, mxid)
	}
}

// TestStore_GetAllRawMIMECandidates_PreservesFromCase mirrors
// TestStore_GetDuplicateGroupMessages_PreservesFromCase but covers the
// content-hash candidate path. Both queries had the same SQL `LOWER()`
// problem before iter12; both fixes need regression coverage so a
// future refactor that reintroduces lowercasing in either query is
// caught. Iter13 claude follow-up.
func TestStore_GetAllRawMIMECandidates_PreservesFromCase(t *testing.T) {
	f := storetest.New(t)

	mxid := "@Bob:matrix.org"
	pid := f.EnsureParticipant(mxid, "", "")

	id := newRFC822Message(t, f, "msg-mxid-raw", "rfc822-mxid-raw")

	if _, err := f.Store.DB().Exec(
		f.Store.Rebind(`INSERT INTO message_recipients
			(message_id, participant_id, recipient_type)
			VALUES (?, ?, 'from')`),
		id, pid,
	); err != nil {
		t.Fatalf("insert from recipient: %v", err)
	}

	// GetAllRawMIMECandidates only returns messages that have a raw
	// MIME row, so synthesize one.
	testutil.MustNoErr(t,
		f.Store.UpsertMessageRaw(id, []byte("From: "+mxid+"\r\n\r\nbody")),
		"UpsertMessageRaw",
	)

	cands, err := f.Store.GetAllRawMIMECandidates()
	testutil.MustNoErr(t, err, "GetAllRawMIMECandidates")
	var got *store.ContentHashCandidate
	for i := range cands {
		if cands[i].ID == id {
			got = &cands[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("test message %d not in candidates: %+v", id, cands)
	}
	if got.FromEmail != mxid {
		t.Errorf("FromEmail = %q, want %q (case must be preserved)", got.FromEmail, mxid)
	}
}
