package cmd

import (
	"fmt"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestRepairOtherStrings_LogsScanErrors verifies that scan errors during
// repairOtherStrings are counted in stats.skippedRows rather than silently
// swallowed. We trigger scan errors by recreating the labels table with a
// TEXT id column and inserting a non-numeric id that can't be scanned into int64.
func TestRepairOtherStrings_LogsScanErrors(t *testing.T) {
	require := requirepkg.New(t)
	testutil.SkipIfPostgres(t, "uses PRAGMA foreign_keys=OFF and recreates labels with TEXT id to trigger a SQLite scan error; PG enforces FK + types differently")
	st := testutil.NewTestStore(t)
	db := st.DB()

	// Disable foreign keys so we can recreate the labels table
	_, err := db.Exec("PRAGMA foreign_keys = OFF")
	require.NoError(err, "disable foreign keys")

	// Drop labels and recreate with TEXT id (not INTEGER PRIMARY KEY).
	// Scanning a non-numeric TEXT value into int64 triggers a scan error.
	_, err = db.Exec("DROP TABLE IF EXISTS labels")
	require.NoError(err, "drop labels")
	_, err = db.Exec(`CREATE TABLE labels (
		id TEXT, source_id INTEGER, source_label_id TEXT,
		name TEXT NOT NULL, label_type TEXT, color TEXT
	)`)
	require.NoError(err, "create labels")

	// Insert a row with non-numeric id → Scan into int64 will fail
	_, err = db.Exec(`INSERT INTO labels (id, source_id, source_label_id, name, label_type)
		VALUES ('not-a-number', 1, 'lbl1', 'Test Label', 'user')`)
	require.NoError(err, "insert bad label")

	stats := &repairStats{}
	require.NoError(repairOtherStrings(st, stats), "repairOtherStrings")

	// Before fix: skippedRows == 0 (scan error silently swallowed)
	// After fix: skippedRows == 1 (scan error counted)
	assertpkg.Equal(t, 1, stats.skippedRows, "skippedRows")
}

// TestRepairDisplayNames_LogsScanErrors verifies that scan errors during
// repairDisplayNames are counted in stats.skippedRows. We trigger scan errors
// by recreating the participants table with a TEXT id column.
func TestRepairDisplayNames_LogsScanErrors(t *testing.T) {
	require := requirepkg.New(t)
	testutil.SkipIfPostgres(t, "uses PRAGMA foreign_keys=OFF and recreates a table with mismatched id type to trigger a SQLite scan error; PG enforces types differently")
	st := testutil.NewTestStore(t)
	db := st.DB()

	_, err := db.Exec("PRAGMA foreign_keys = OFF")
	require.NoError(err, "disable foreign keys")

	// Drop participants and recreate with TEXT id
	_, err = db.Exec("DROP TABLE IF EXISTS participants")
	require.NoError(err, "drop participants")
	_, err = db.Exec(`CREATE TABLE participants (
		id TEXT, email_address TEXT, phone_number TEXT,
		display_name TEXT, domain TEXT, canonical_id TEXT,
		created_at DATETIME, updated_at DATETIME
	)`)
	require.NoError(err, "create participants")

	// Insert a row with non-numeric id → Scan into int64 will fail
	_, err = db.Exec(`INSERT INTO participants (id, email_address, display_name, domain)
		VALUES ('bad-id', 'test@example.com', 'Test User', 'example.com')`)
	require.NoError(err, "insert bad participant")

	stats := &repairStats{}
	require.NoError(repairDisplayNames(st, stats), "repairDisplayNames")

	// Before fix: skippedRows == 0 (scan error silently swallowed)
	// After fix: skippedRows == 1 (scan error counted)
	assertpkg.Equal(t, 1, stats.skippedRows, "skippedRows")
}

// TestRepairEncoding_NoScanErrors verifies that normal data produces
// zero skipped rows.
func TestRepairEncoding_NoScanErrors(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)

	stats := &repairStats{}

	_, err := repairMessageFields(st, stats)
	require.NoError(err, "repairMessageFields")
	require.NoError(repairDisplayNames(st, stats), "repairDisplayNames")
	require.NoError(repairOtherStrings(st, stats), "repairOtherStrings")

	assertpkg.Zero(t, stats.skippedRows, "skippedRows should be 0 for valid data")
}

// TestRepairMessageFields_ReturnsReembedNeededIDs guards the re-embedding
// hook: when any field that feeds the embedder (subject, body_text,
// body_html) is repaired, the affected message id must appear in the
// returned slice so the caller can re-enqueue it against
// pending_embeddings. Snippet-only repairs must NOT appear because the
// embedder doesn't read snippet.
func TestRepairMessageFields_ReturnsReembedNeededIDs(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	testutil.SkipIfPostgres(t, "inserts invalid UTF-8 bytes into TEXT columns; SQLite stores them permissively, PG rejects with invalid_text_representation")
	st := testutil.NewTestStore(t)
	db := st.DB()

	// Insert a source and conversation so FKs are satisfied.
	_, err := db.Exec(
		`INSERT INTO sources (id, source_type, identifier, created_at, updated_at)
		 VALUES (1, 'test', 'test@example.com', datetime('now'), datetime('now'))`)
	require.NoError(err, "insert source")
	_, err = db.Exec(
		`INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title, created_at, updated_at)
		 VALUES (1, 1, 'conv-1', 'email_thread', 'title', datetime('now'), datetime('now'))`)
	require.NoError(err, "insert conversation")

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
		_, execErr := db.Exec(
			`INSERT INTO messages (id, conversation_id, source_id, source_message_id,
			 message_type, subject, snippet, sent_at, size_estimate)
			 VALUES (?, 1, 1, ?, 'email', ?, ?, datetime('now'), 1000)`,
			ins.id, fmt.Sprintf("src-%d", ins.id), ins.subject, ins.snippet)
		require.NoError(execErr, "insert message %d", ins.id)
		_, execErr = db.Exec(
			`INSERT INTO message_bodies (message_id, body_text, body_html) VALUES (?, ?, ?)`,
			ins.id, ins.bodyText, ins.bodyHTML)
		require.NoError(execErr, "insert body %d", ins.id)
	}

	stats := &repairStats{}
	ids, err := repairMessageFields(st, stats)
	require.NoError(err, "repairMessageFields")

	gotSet := map[int64]bool{}
	for _, id := range ids {
		gotSet[id] = true
	}
	for _, want := range []int64{10, 20, 30} {
		assert.True(gotSet[want], "msg %d missing from reembedNeededIDs, got: %v", want, ids)
	}
	assert.False(gotSet[40], "msg 40 (snippet-only repair) must NOT be in reembedNeededIDs, got: %v", ids)
	assert.False(gotSet[50], "msg 50 (no repair) must NOT be in reembedNeededIDs, got: %v", ids)
}

// TestRepairOtherStrings_FixesNewColumns verifies that repairOtherStrings
// repairs invalid UTF-8 in source_conversation_id, email_address, and domain.
func TestRepairOtherStrings_FixesNewColumns(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	testutil.SkipIfPostgres(t, "inserts invalid UTF-8 bytes into TEXT columns; SQLite stores them permissively, PG rejects with invalid_text_representation")
	st := testutil.NewTestStore(t)
	db := st.DB()

	// Insert a source so foreign key constraints are satisfied.
	_, err := db.Exec(
		`INSERT INTO sources (id, source_type, identifier, created_at, updated_at)
		 VALUES (1, 'test', 'test@example.com', datetime('now'), datetime('now'))`,
	)
	require.NoError(err, "insert source")

	// Insert conversation with invalid UTF-8 in source_conversation_id.
	_, err = db.Exec(
		`INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title, created_at, updated_at)
		 VALUES (1, 1, ?, 'email_thread', 'valid title', datetime('now'), datetime('now'))`,
		"conv-\x80\x81\x82",
	)
	require.NoError(err, "insert conversation")

	// Insert participant with invalid UTF-8 in email_address and domain.
	_, err = db.Exec(
		`INSERT INTO participants (id, email_address, domain, created_at, updated_at)
		 VALUES (1, ?, ?, datetime('now'), datetime('now'))`,
		"user\xFE@example.com", "example\xFF.com",
	)
	require.NoError(err, "insert participant")

	stats := &repairStats{}
	require.NoError(repairOtherStrings(st, stats), "repairOtherStrings")

	assert.Equal(1, stats.convSourceIDs, "convSourceIDs")
	assert.Equal(1, stats.emailAddrs, "emailAddrs")
	assert.Equal(1, stats.domains, "domains")
}
