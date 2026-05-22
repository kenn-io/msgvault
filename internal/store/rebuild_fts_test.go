package store_test

import (
	"database/sql"
	"testing"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestStore_RebuildFTS_HappyPath verifies RebuildFTS on a healthy database
// recreates the FTS index with correct searchable content.
func TestStore_RebuildFTS_HappyPath(t *testing.T) {
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	msgID1 := f.CreateMessage("rebuild-msg-1")
	testutil.MustNoErr(t, f.Store.UpsertMessageBody(msgID1,
		sql.NullString{String: "apple pie filling", Valid: true}, sql.NullString{}),
		"UpsertMessageBody 1")

	pid1 := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	testutil.MustNoErr(t, f.Store.ReplaceMessageRecipients(msgID1, "from",
		[]int64{pid1}, []string{"Alice"}), "ReplaceMessageRecipients")

	msgID2 := f.CreateMessage("rebuild-msg-2")
	testutil.MustNoErr(t, f.Store.UpsertMessageBody(msgID2,
		sql.NullString{String: "banana bread recipe", Valid: true}, sql.NullString{}),
		"UpsertMessageBody 2")

	n, err := f.Store.RebuildFTS(nil)
	testutil.MustNoErr(t, err, "RebuildFTS")
	if n != 2 {
		t.Errorf("RebuildFTS rows = %d, want 2", n)
	}

	var count int
	testutil.MustNoErr(t, f.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'banana'").Scan(&count),
		"FTS MATCH banana")
	if count != 1 {
		t.Errorf("match 'banana' = %d, want 1", count)
	}

	testutil.MustNoErr(t, f.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'alice'").Scan(&count),
		"FTS MATCH alice")
	if count != 1 {
		t.Errorf("match 'alice' = %d, want 1", count)
	}
}

// TestStore_RebuildFTS_BypassesAvailabilityFlag verifies the critical
// guarantee that RebuildFTS ignores the cached fts5Available flag. A corrupt
// FTS5 shadow table causes the availability probe to fail, which is exactly
// when the rebuild is needed — BackfillFTS would short-circuit here, but
// RebuildFTS must not.
func TestStore_RebuildFTS_BypassesAvailabilityFlag(t *testing.T) {
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	msgID := f.CreateMessage("rebuild-bypass")
	testutil.MustNoErr(t, f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "cherry tart dessert", Valid: true}, sql.NullString{}),
		"UpsertMessageBody")

	// Force the cached flag false to simulate a probe that saw a corrupt
	// shadow table and returned false at InitSchema time.
	store.SetFTS5AvailableForTest(f.Store, false)

	n, err := f.Store.RebuildFTS(nil)
	testutil.MustNoErr(t, err, "RebuildFTS")
	if n != 1 {
		t.Errorf("RebuildFTS rows = %d, want 1", n)
	}

	if !f.Store.FTS5Available() {
		t.Error("FTS5Available() = false after rebuild, want true")
	}

	var count int
	testutil.MustNoErr(t, f.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'cherry'").Scan(&count),
		"FTS MATCH cherry")
	if count != 1 {
		t.Errorf("match 'cherry' = %d, want 1", count)
	}
}

// TestStore_RebuildFTS_AfterTableDropped verifies that RebuildFTS recreates
// messages_fts from scratch when the table is missing entirely — the
// post-DROP state from the manual recovery procedure in issue #287.
func TestStore_RebuildFTS_AfterTableDropped(t *testing.T) {
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	msgID := f.CreateMessage("rebuild-dropped")
	testutil.MustNoErr(t, f.Store.UpsertMessageBody(msgID,
		sql.NullString{String: "date square confection", Valid: true}, sql.NullString{}),
		"UpsertMessageBody")

	_, err := f.Store.DB().Exec("DROP TABLE messages_fts")
	testutil.MustNoErr(t, err, "DROP TABLE messages_fts")

	n, err := f.Store.RebuildFTS(nil)
	testutil.MustNoErr(t, err, "RebuildFTS")
	if n != 1 {
		t.Errorf("RebuildFTS rows = %d, want 1", n)
	}

	var count int
	testutil.MustNoErr(t, f.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'confection'").Scan(&count),
		"FTS MATCH confection")
	if count != 1 {
		t.Errorf("match 'confection' = %d, want 1", count)
	}
}

// TestStore_RebuildFTS_ReportsProgress verifies the progress callback is
// invoked with monotonic (done, total) values.
func TestStore_RebuildFTS_ReportsProgress(t *testing.T) {
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	ids := f.CreateMessages(3)
	for i, id := range ids {
		testutil.MustNoErr(t, f.Store.UpsertMessageBody(id,
			sql.NullString{String: "progress body", Valid: true}, sql.NullString{}),
			"UpsertMessageBody")
		_ = i
	}

	var calls int
	var lastDone, lastTotal int64
	_, err := f.Store.RebuildFTS(func(done, total int64) {
		calls++
		if total <= 0 {
			t.Errorf("progress total = %d, want > 0", total)
		}
		if done < lastDone {
			t.Errorf("progress done went backwards: %d -> %d", lastDone, done)
		}
		lastDone, lastTotal = done, total
	})
	testutil.MustNoErr(t, err, "RebuildFTS")

	if calls == 0 {
		t.Error("progress callback never invoked")
	}
	if lastDone != lastTotal {
		t.Errorf("final progress = (%d/%d), want done == total", lastDone, lastTotal)
	}
}
