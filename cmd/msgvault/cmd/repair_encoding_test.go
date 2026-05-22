package cmd

import (
	"fmt"
	"testing"

	"go.kenn.io/msgvault/internal/testutil"
)

// TestRepairOtherStrings_LogsScanErrors verifies that scan errors during
// repairOtherStrings are counted in stats.skippedRows rather than silently
// swallowed. We trigger scan errors by recreating the labels table with a
// TEXT id column and inserting a non-numeric id that can't be scanned into int64.
func TestRepairOtherStrings_LogsScanErrors(t *testing.T) {
	st := testutil.NewTestStore(t)
	db := st.DB()

	// Disable foreign keys so we can recreate the labels table
	if _, err := db.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}

	// Drop labels and recreate with TEXT id (not INTEGER PRIMARY KEY).
	// Scanning a non-numeric TEXT value into int64 triggers a scan error.
	if _, err := db.Exec("DROP TABLE IF EXISTS labels"); err != nil {
		t.Fatalf("drop labels: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE labels (
		id TEXT, source_id INTEGER, source_label_id TEXT,
		name TEXT NOT NULL, label_type TEXT, color TEXT
	)`); err != nil {
		t.Fatalf("create labels: %v", err)
	}

	// Insert a row with non-numeric id → Scan into int64 will fail
	if _, err := db.Exec(`INSERT INTO labels (id, source_id, source_label_id, name, label_type)
		VALUES ('not-a-number', 1, 'lbl1', 'Test Label', 'user')`); err != nil {
		t.Fatalf("insert bad label: %v", err)
	}

	stats := &repairStats{}
	err := repairOtherStrings(st, stats)
	if err != nil {
		t.Fatalf("repairOtherStrings returned error: %v", err)
	}

	// Before fix: skippedRows == 0 (scan error silently swallowed)
	// After fix: skippedRows == 1 (scan error counted)
	if stats.skippedRows != 1 {
		t.Errorf("skippedRows = %d, want 1", stats.skippedRows)
	}
}

// TestRepairDisplayNames_LogsScanErrors verifies that scan errors during
// repairDisplayNames are counted in stats.skippedRows. We trigger scan errors
// by recreating the participants table with a TEXT id column.
func TestRepairDisplayNames_LogsScanErrors(t *testing.T) {
	st := testutil.NewTestStore(t)
	db := st.DB()

	if _, err := db.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}

	// Drop participants and recreate with TEXT id
	if _, err := db.Exec("DROP TABLE IF EXISTS participants"); err != nil {
		t.Fatalf("drop participants: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE participants (
		id TEXT, email_address TEXT, phone_number TEXT,
		display_name TEXT, domain TEXT, canonical_id TEXT,
		created_at DATETIME, updated_at DATETIME
	)`); err != nil {
		t.Fatalf("create participants: %v", err)
	}

	// Insert a row with non-numeric id → Scan into int64 will fail
	if _, err := db.Exec(`INSERT INTO participants (id, email_address, display_name, domain)
		VALUES ('bad-id', 'test@example.com', 'Test User', 'example.com')`); err != nil {
		t.Fatalf("insert bad participant: %v", err)
	}

	stats := &repairStats{}
	err := repairDisplayNames(st, stats)
	if err != nil {
		t.Fatalf("repairDisplayNames returned error: %v", err)
	}

	// Before fix: skippedRows == 0 (scan error silently swallowed)
	// After fix: skippedRows == 1 (scan error counted)
	if stats.skippedRows != 1 {
		t.Errorf("skippedRows = %d, want 1", stats.skippedRows)
	}
}

// TestRepairEncoding_NoScanErrors verifies that normal data produces
// zero skipped rows.
func TestRepairEncoding_NoScanErrors(t *testing.T) {
	st := testutil.NewTestStore(t)

	stats := &repairStats{}

	if _, err := repairMessageFields(st, stats); err != nil {
		t.Fatalf("repairMessageFields: %v", err)
	}
	if err := repairDisplayNames(st, stats); err != nil {
		t.Fatalf("repairDisplayNames: %v", err)
	}
	if err := repairOtherStrings(st, stats); err != nil {
		t.Fatalf("repairOtherStrings: %v", err)
	}

	if stats.skippedRows != 0 {
		t.Errorf("skippedRows = %d, want 0 for valid data", stats.skippedRows)
	}
}

// TestRepairMessageFields_ReturnsReembedNeededIDs guards the re-embedding
// hook: when any field that feeds the embedder (subject, body_text,
// body_html) is repaired, the affected message id must appear in the
// returned slice so the caller can re-enqueue it against
// pending_embeddings. Snippet-only repairs must NOT appear because the
// embedder doesn't read snippet.
func TestRepairMessageFields_ReturnsReembedNeededIDs(t *testing.T) {
	st := testutil.NewTestStore(t)
	db := st.DB()

	// Insert a source and conversation so FKs are satisfied.
	if _, err := db.Exec(
		`INSERT INTO sources (id, source_type, identifier, created_at, updated_at)
		 VALUES (1, 'test', 'test@example.com', datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("insert source: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title, created_at, updated_at)
		 VALUES (1, 1, 'conv-1', 'email_thread', 'title', datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}

	// Message 10: subject has invalid UTF-8 → subject is repaired
	//             (embedder reads subject, so must be re-enqueued).
	// Message 20: body_text has invalid UTF-8 → body is repaired.
	// Message 30: body_html has invalid UTF-8 → body is repaired.
	// Message 40: only snippet has invalid UTF-8 → snippet-only repair
	//             must NOT be in the re-enqueue list (not embedded).
	// Message 50: all fields clean → nothing repaired.
	inserts := []struct {
		id       int64
		subject  string
		bodyText string
		bodyHTML string
		snippet  string
	}{
		{10, "subj\x80bad", "clean body", "", "clean snippet"},
		{20, "clean subject", "body\xFEbad", "", "snip"},
		{30, "clean subject", "", "<p>body\xFFbad</p>", "snip"},
		{40, "clean subject", "clean body", "", "snip\x81bad"},
		{50, "clean subject", "clean body", "", "clean snippet"},
	}
	for _, ins := range inserts {
		if _, err := db.Exec(
			`INSERT INTO messages (id, conversation_id, source_id, source_message_id,
			 message_type, subject, snippet, sent_at, size_estimate)
			 VALUES (?, 1, 1, ?, 'email', ?, ?, datetime('now'), 1000)`,
			ins.id, fmt.Sprintf("src-%d", ins.id), ins.subject, ins.snippet); err != nil {
			t.Fatalf("insert message %d: %v", ins.id, err)
		}
		if _, err := db.Exec(
			`INSERT INTO message_bodies (message_id, body_text, body_html) VALUES (?, ?, ?)`,
			ins.id, ins.bodyText, ins.bodyHTML); err != nil {
			t.Fatalf("insert body %d: %v", ins.id, err)
		}
	}

	stats := &repairStats{}
	ids, err := repairMessageFields(st, stats)
	if err != nil {
		t.Fatalf("repairMessageFields: %v", err)
	}

	gotSet := map[int64]bool{}
	for _, id := range ids {
		gotSet[id] = true
	}
	for _, want := range []int64{10, 20, 30} {
		if !gotSet[want] {
			t.Errorf("msg %d missing from reembedNeededIDs, got: %v", want, ids)
		}
	}
	if gotSet[40] {
		t.Errorf("msg 40 (snippet-only repair) must NOT be in reembedNeededIDs, got: %v", ids)
	}
	if gotSet[50] {
		t.Errorf("msg 50 (no repair) must NOT be in reembedNeededIDs, got: %v", ids)
	}
}

// TestRepairOtherStrings_FixesNewColumns verifies that repairOtherStrings
// repairs invalid UTF-8 in source_conversation_id, email_address, and domain.
func TestRepairOtherStrings_FixesNewColumns(t *testing.T) {
	st := testutil.NewTestStore(t)
	db := st.DB()

	// Insert a source so foreign key constraints are satisfied.
	if _, err := db.Exec(
		`INSERT INTO sources (id, source_type, identifier, created_at, updated_at)
		 VALUES (1, 'test', 'test@example.com', datetime('now'), datetime('now'))`,
	); err != nil {
		t.Fatalf("insert source: %v", err)
	}

	// Insert conversation with invalid UTF-8 in source_conversation_id.
	if _, err := db.Exec(
		`INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title, created_at, updated_at)
		 VALUES (1, 1, ?, 'email_thread', 'valid title', datetime('now'), datetime('now'))`,
		"conv-\x80\x81\x82",
	); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}

	// Insert participant with invalid UTF-8 in email_address and domain.
	if _, err := db.Exec(
		`INSERT INTO participants (id, email_address, domain, created_at, updated_at)
		 VALUES (1, ?, ?, datetime('now'), datetime('now'))`,
		"user\xFE@example.com", "example\xFF.com",
	); err != nil {
		t.Fatalf("insert participant: %v", err)
	}

	stats := &repairStats{}
	if err := repairOtherStrings(st, stats); err != nil {
		t.Fatalf("repairOtherStrings: %v", err)
	}

	if stats.convSourceIDs != 1 {
		t.Errorf(
			"convSourceIDs = %d, want 1", stats.convSourceIDs,
		)
	}
	if stats.emailAddrs != 1 {
		t.Errorf("emailAddrs = %d, want 1", stats.emailAddrs)
	}
	if stats.domains != 1 {
		t.Errorf("domains = %d, want 1", stats.domains)
	}
}
