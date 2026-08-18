package cmd

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/textutil"
)

func TestRepairDisplayNamesBumpsParticipantRevisionWithTheRepair(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t,
		"inserts invalid UTF-8 bytes into a TEXT column; PostgreSQL rejects them")
	st := testutil.NewTestStore(t)

	_, err := st.DB().Exec(`
		INSERT INTO participants (email_address, display_name)
		VALUES (?, ?)
	`, "repair-display@example.com", "Repair\xffName")
	require.NoError(err)
	before, err := st.ParticipantDisplayNameRevision()
	require.NoError(err)

	stats := &repairStats{}
	require.NoError(repairDisplayNames(st, stats))
	after, err := st.ParticipantDisplayNameRevision()
	require.NoError(err)
	assert.Equal(before+1, after,
		"the participant repair and its cache revision must commit together")

	var repaired string
	require.NoError(st.DB().QueryRow(`
		SELECT display_name FROM participants
		WHERE email_address = ?
	`, "repair-display@example.com").Scan(&repaired))
	assert.True(utf8.ValidString(repaired))
}

// TestRepairOtherStrings_LogsScanErrors verifies that scan errors during
// repairOtherStrings are counted in stats.skippedRows rather than silently
// swallowed. We trigger scan errors by recreating the labels table with a
// TEXT id column and inserting a non-numeric id that can't be scanned into int64.
func TestRepairOtherStrings_LogsScanErrors(t *testing.T) {
	require := require.New(t)
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
	assert.Equal(t, 1, stats.skippedRows, "skippedRows")
}

// TestRepairDisplayNames_LogsScanErrors verifies that scan errors during
// repairDisplayNames are counted in stats.skippedRows. We trigger scan errors
// by recreating the participants table with a TEXT id column.
func TestRepairDisplayNames_LogsScanErrors(t *testing.T) {
	require := require.New(t)
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
	assert.Equal(t, 1, stats.skippedRows, "skippedRows")
}

// TestRepairEncoding_NoScanErrors verifies that normal data produces
// zero skipped rows.
func TestRepairEncoding_NoScanErrors(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)

	stats := &repairStats{}

	_, err := repairMessageFields(st, stats)
	require.NoError(err, "repairMessageFields")
	require.NoError(repairDisplayNames(st, stats), "repairDisplayNames")
	require.NoError(repairOtherStrings(st, stats), "repairOtherStrings")

	assert.Zero(t, stats.skippedRows, "skippedRows should be 0 for valid data")
}

// TestRepairMessageFields_ReturnsReembedNeededIDs guards the re-embedding
// hook: when any field that feeds the embedder (subject, body_text,
// body_html) is repaired, the affected message id must appear in the
// returned slice so the caller can mark it for re-embedding via
// ResetEmbedGen (embed_gen -> NULL), which the scan-and-fill worker then
// re-picks up. Snippet-only repairs must NOT appear because the embedder
// doesn't read snippet.
func TestRepairMessageFields_ReturnsReembedNeededIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require := require.New(t)
	assert := assert.New(t)
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

func TestRepairConversationPreviews_RestoresPreviewStrandedByEarlierRepair(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "inserts invalid UTF-8 bytes into TEXT columns; PostgreSQL rejects them")
	st := testutil.NewTestStore(t)
	db := st.DB()

	broken := strings.Repeat("a", 198) + "\xe2\x80"
	want := strings.Repeat("a", 198)
	_, err := db.Exec(`INSERT INTO sources
		(id, source_type, identifier, created_at, updated_at)
		VALUES (1, ?, 'alice@example.com/primary', datetime('now'), datetime('now'))`, gcal.SourceType)
	require.NoError(err, "insert source")
	_, err = db.Exec(`INSERT INTO conversations
		(id, source_id, source_conversation_id, conversation_type, title, last_message_preview, created_at, updated_at)
		VALUES (1, 1, 'calendar-conversation', ?, 'Calendar', ?, datetime('now'), datetime('now'))`,
		gcal.ConversationType, broken)
	require.NoError(err, "insert conversation with stranded preview")
	_, err = db.Exec(`INSERT INTO messages
		(id, conversation_id, source_id, source_message_id, message_type, snippet, sent_at, size_estimate)
		VALUES (10, 1, 1, 'repair-10', ?, ?, datetime('now'), 1000)`,
		gcal.MessageTypeCalendarEvent, want)
	require.NoError(err, "insert previously repaired message")

	stats := &repairStats{}
	require.NoError(repairConversationPreviews(st, stats), "repair stranded conversation preview")
	var got string
	require.NoError(db.QueryRow(`SELECT last_message_preview FROM conversations WHERE id = 1`).Scan(&got),
		"read repaired conversation preview")
	assert.Equal(want, got, "preview is rederived from the previously repaired latest message")
	assert.Equal(1, stats.convPreviews, "convPreviews")

	require.NoError(repairConversationPreviews(st, stats), "rerun preview repair")
	assert.Equal(1, stats.convPreviews, "rerun is idempotent")
}

func TestRepairConversationPreviews_UsesLatestMessageAfterCollidingRepairs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "inserts invalid UTF-8 bytes into TEXT columns; PostgreSQL rejects them")
	st := testutil.NewTestStore(t)
	db := st.DB()

	broken := strings.Repeat("c", 198) + "\xe2\x80"
	calendarBody := strings.Repeat("c", 198) + "\u2014after-boundary"
	_, err := db.Exec(`INSERT INTO sources
		(id, source_type, identifier, created_at, updated_at)
		VALUES (1, ?, 'alice@example.com/primary', datetime('now'), datetime('now'))`, gcal.SourceType)
	require.NoError(err, "insert source")
	_, err = db.Exec(`INSERT INTO conversations
		(id, source_id, source_conversation_id, conversation_type, title, last_message_preview, created_at, updated_at)
		VALUES (1, 1, 'calendar-conversation', ?, 'Calendar', ?, datetime('now'), datetime('now'))`,
		gcal.ConversationType, broken)
	require.NoError(err, "insert conversation")
	_, err = db.Exec(`INSERT INTO messages
		(id, conversation_id, source_id, source_message_id, message_type, snippet, sent_at, size_estimate)
		VALUES (10, 1, 1, 'older-generic', 'email', ?, datetime('now'), 1000),
		       (20, 1, 1, 'latest-calendar', ?, ?, datetime('now', '+1 minute'), 1000)`,
		broken, gcal.MessageTypeCalendarEvent, broken)
	require.NoError(err, "insert colliding damaged snippets")
	_, err = db.Exec(`INSERT INTO message_bodies (message_id, body_text)
		VALUES (10, 'generic body'), (20, ?)`, calendarBody)
	require.NoError(err, "insert message bodies")

	stats := &repairStats{}
	_, err = repairMessageFields(st, stats)
	require.NoError(err, "repair colliding message snippets")
	require.NoError(repairConversationPreviews(st, stats), "repair conversation preview")

	var older, latest, preview string
	require.NoError(db.QueryRow(`SELECT snippet FROM messages WHERE id = 10`).Scan(&older), "read older snippet")
	require.NoError(db.QueryRow(`SELECT snippet FROM messages WHERE id = 20`).Scan(&latest), "read latest snippet")
	require.NoError(db.QueryRow(`SELECT last_message_preview FROM conversations WHERE id = 1`).Scan(&preview),
		"read conversation preview")
	assert.Equal(textutil.EnsureUTF8(broken), older, "generic repair keeps its existing behavior")
	assert.Equal(strings.Repeat("c", 198), latest, "calendar repair uses canonical body")
	assert.Equal(latest, preview, "conversation preview follows the latest message after all repairs")
}

func TestRepairMessageFields_RegeneratesOnlyInvalidCalendarSnippetFromCanonicalBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "inserts invalid UTF-8 bytes into TEXT columns; PostgreSQL rejects them")
	st := testutil.NewTestStore(t)
	db := st.DB()

	_, err := db.Exec(`INSERT INTO sources (id, source_type, identifier, created_at, updated_at)
		VALUES (1, ?, 'alice@example.com/primary', datetime('now'), datetime('now')),
		       (2, 'gmail', 'alice@example.com', datetime('now'), datetime('now'))`, gcal.SourceType)
	require.NoError(err, "insert sources")
	_, err = db.Exec(`INSERT INTO conversations
		(id, source_id, source_conversation_id, conversation_type, title, created_at, updated_at)
		VALUES (1, 1, 'calendar-conversation', ?, 'Calendar', datetime('now'), datetime('now')),
		       (2, 2, 'email-conversation', 'email_thread', 'Email', datetime('now'), datetime('now'))`,
		gcal.ConversationType)
	require.NoError(err, "insert conversations")

	canonicalBody := strings.Repeat("a", 198) + "\u2014after-boundary"
	bodyPtr := func(value string) *string { return &value }
	rows := []struct {
		id           int64
		conversation int64
		source       int64
		messageType  string
		snippet      string
		body         *string
	}{
		{10, 1, 1, gcal.MessageTypeCalendarEvent, strings.Repeat("a", 198) + "\xe2\x80", &canonicalBody},
		{20, 2, 2, "email", "mail\xff", bodyPtr("email canonical body")},
		{30, 1, 1, gcal.MessageTypeCalendarEvent, "missing\xff", nil},
		{40, 1, 1, gcal.MessageTypeCalendarEvent, "invalid-body\xff", bodyPtr("body\xff")},
		{50, 1, 1, gcal.MessageTypeCalendarEvent, "keep existing preview", &canonicalBody},
		{60, 1, 1, gcal.MessageTypeCalendarEvent, strings.Repeat("a", 198) + "\xe2", &canonicalBody},
		{70, 1, 1, gcal.MessageTypeCalendarEvent, strings.Repeat("b", 198) + "\xe2\x80", &canonicalBody},
	}
	for _, row := range rows {
		_, execErr := db.Exec(`INSERT INTO messages
			(id, conversation_id, source_id, source_message_id, message_type, snippet, sent_at, size_estimate)
			VALUES (?, ?, ?, ?, ?, ?, datetime('now'), 1000)`,
			row.id, row.conversation, row.source, fmt.Sprintf("repair-%d", row.id), row.messageType, row.snippet)
		require.NoError(execErr, "insert message %d", row.id)
		if row.body != nil {
			_, execErr = db.Exec(`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`, row.id, *row.body)
			require.NoError(execErr, "insert body %d", row.id)
		}
	}
	_, err = db.Exec(`UPDATE messages SET sent_at = datetime('now', '+1 minute') WHERE id = 10`)
	require.NoError(err, "make repaired calendar message the conversation's latest")
	_, err = db.Exec(`UPDATE conversations SET last_message_preview = ? WHERE id = 1`, rows[0].snippet)
	require.NoError(err, "store copied damaged calendar preview")
	_, err = db.Exec(`UPDATE conversations SET last_message_preview = 'sibling sentinel' WHERE id = 2`)
	require.NoError(err, "store unrelated conversation preview")

	stats := &repairStats{}
	reembedNeededIDs, err := repairMessageFields(st, stats)
	require.NoError(err, "repair message fields")
	require.NoError(repairConversationPreviews(st, stats), "repair copied conversation preview")

	got := make(map[int64]string)
	resultRows, err := db.Query(`SELECT id, snippet FROM messages WHERE id IN (10, 20, 30, 40, 50, 60, 70)`)
	require.NoError(err, "query repaired snippets")
	defer func() { assert.NoError(resultRows.Close()) }()
	for resultRows.Next() {
		var id int64
		var snippet string
		require.NoError(resultRows.Scan(&id, &snippet), "scan repaired snippet")
		got[id] = snippet
	}
	require.NoError(resultRows.Err(), "iterate repaired snippets")

	assert.Equal(strings.Repeat("a", 198), got[10], "calendar snippet derives from intact canonical body")
	assert.Equal(textutil.EnsureUTF8("mail\xff"), got[20], "non-calendar snippet retains generic repair")
	assert.Equal(textutil.EnsureUTF8("missing\xff"), got[30], "missing calendar body retains generic repair")
	assert.Equal(textutil.EnsureUTF8("invalid-body\xff"), got[40], "invalid calendar body retains generic repair")
	assert.Equal("keep existing preview", got[50], "valid calendar snippet is not backfilled")
	assert.Equal(textutil.EnsureUTF8(strings.Repeat("a", 198)+"\xe2"), got[60],
		"wrong-length calendar snippet retains generic repair")
	assert.Equal(textutil.EnsureUTF8(strings.Repeat("b", 198)+"\xe2\x80"), got[70],
		"200-byte non-prefix calendar snippet retains generic repair")
	var conversationPreview string
	require.NoError(db.QueryRow(`SELECT last_message_preview FROM conversations WHERE id = 1`).Scan(&conversationPreview),
		"read repaired conversation preview")
	assert.Equal(strings.Repeat("a", 198), conversationPreview,
		"copied conversation preview follows the repaired latest message")
	require.NoError(db.QueryRow(`SELECT last_message_preview FROM conversations WHERE id = 2`).Scan(&conversationPreview),
		"read unrelated conversation preview")
	assert.Equal("sibling sentinel", conversationPreview,
		"repair does not overwrite a preview that is not the damaged snippet")
	for id, snippet := range got {
		assert.True(utf8.ValidString(snippet), "message %d snippet must be valid UTF-8", id)
	}
	assert.Equal(6, stats.snippets, "only invalid snippets count as repaired")
	assert.Equal(1, stats.convPreviews, "only the copied invalid conversation preview is repaired")
	assert.Contains(reembedNeededIDs, int64(40), "invalid body repair still requests re-embedding")
	assert.NotContains(reembedNeededIDs, int64(10), "snippet-only regeneration does not request re-embedding")
}

// TestRepairOtherStrings_RefreshesOwnershipAtomicallyPerBatch pins the email
// column's derived-state contract: each committed batch settles attribution
// and advances the identity revisions in the SAME transaction as the email
// rewrite. Repairs commit in 1,000-row batches, so 1,500 broken emails must
// advance the account-identity revision once per batch — and a rewrite can
// never commit without its refresh, which mattered because repaired emails
// are valid UTF-8 and a rerun's scan can no longer discover them.
func TestRepairOtherStrings_RefreshesOwnershipAtomicallyPerBatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	testutil.SkipIfPostgres(t, "inserts invalid UTF-8 bytes into TEXT columns; SQLite stores them permissively, PG rejects with invalid_text_representation")
	st := testutil.NewTestStore(t)
	db := st.DB()

	const broken = 1500
	for start := 0; start < broken; start += 500 {
		values := make([]string, 0, 500)
		args := make([]any, 0, 500)
		for i := start; i < min(start+500, broken); i++ {
			values = append(values, "(?, datetime('now'), datetime('now'))")
			args = append(args, fmt.Sprintf("user%d\xFE@example.com", i))
		}
		_, err := db.Exec(
			`INSERT INTO participants (email_address, created_at, updated_at) VALUES `+
				strings.Join(values, ","), args...)
		require.NoError(err, "seed broken participant emails")
	}

	revisionBefore, err := st.AccountIdentityRevision()
	require.NoError(err)
	stats := &repairStats{}
	require.NoError(repairOtherStrings(st, stats), "repairOtherStrings")
	assert.Equal(broken, stats.emailAddrs)

	revisionAfter, err := st.AccountIdentityRevision()
	require.NoError(err)
	assert.Equal(revisionBefore+2, revisionAfter,
		"each committed batch must advance the revision with its rewrite")

	var stillBroken int
	require.NoError(db.QueryRow(
		`SELECT COUNT(*) FROM participants
		 WHERE email_address LIKE '%' || CAST(x'FE' AS TEXT) || '%'`,
	).Scan(&stillBroken))
	assert.Zero(stillBroken, "every seeded email must be repaired")
}
