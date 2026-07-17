package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedLiveMessages inserts n messages (one source, conversation, participant,
// and a 'from' recipient each) so ListMessages exercises its real joins. Every
// other message is marked deleted_from_source so the live-messages predicate is
// selective, mirroring an archive that retains source-deleted messages.
func seedLiveMessages(tb testing.TB, s *Store, n int) {
	tb.Helper()
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'bench@example.com')`)
	require.NoError(tb, err)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type)
		 VALUES (1, 1, 'conv1', 'email')`)
	require.NoError(tb, err)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO participants (id, email_address, display_name) VALUES (1, 'sender@example.com', 'Sender')`)
	require.NoError(tb, err)

	tx, err := s.db.Begin()
	require.NoError(tb, err)
	msgStmt, err := tx.Prepare(`INSERT INTO messages
		(id, conversation_id, source_id, source_message_id, message_type,
		 sent_at, subject, snippet, size_estimate, deleted_from_source_at)
		VALUES (?, 1, 1, ?, 'email', ?, ?, ?, ?, ?)`)
	require.NoError(tb, err)
	defer func() { _ = msgStmt.Close() }()
	recStmt, err := tx.Prepare(`INSERT INTO message_recipients
		(message_id, participant_id, recipient_type) VALUES (?, 1, 'from')`)
	require.NoError(tb, err)
	defer func() { _ = recStmt.Close() }()

	base := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		sentAt := base.Add(time.Duration(i) * time.Minute)
		var deleted any
		if i%2 == 0 {
			deleted = sentAt
		}
		_, err = msgStmt.Exec(i, fmt.Sprintf("m%d", i), sentAt,
			fmt.Sprintf("Subject %d", i), fmt.Sprintf("snippet %d", i), 1000+i, deleted)
		require.NoError(tb, err)
		_, err = recStmt.Exec(i)
		require.NoError(tb, err)
	}
	require.NoError(tb, tx.Commit())
}

func explainPlan(t *testing.T, s *Store, sql string, args ...any) string {
	t.Helper()
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+sql, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var a, c, d int
		var detail string
		require.NoError(t, rows.Scan(&a, &c, &d, &detail))
		b.WriteString(detail)
		b.WriteString("\n")
	}
	require.NoError(t, rows.Err())
	return b.String()
}

// TestListMessages_UsesLiveIndex verifies the ListMessages COUNT and page
// queries are served by partial live-message indexes instead of a full table
// scan + temp-B-tree sort. This locks in the perf fix for GET /api/v1/messages,
// which was seconds per page on a multi-GB SQLite archive.
func TestListMessages_UsesLiveIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := t.TempDir()
	s, err := OpenForTest(filepath.Join(dir, "list.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	seedLiveMessages(t, s, 2000)

	// The index InitSchema created must exist.
	var idxCount int
	require.NoError(s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_messages_live_sent_at'`,
	).Scan(&idxCount))
	assert.Equal(1, idxCount, "idx_messages_live_sent_at should be created by InitSchema")

	countSQL := "SELECT COUNT(*) FROM messages WHERE " + LiveMessagesWhere("", true)
	countPlan := explainPlan(t, s, countSQL)
	assert.Contains(countPlan, "USING INDEX idx_messages_live_",
		"COUNT should use the partial index, not a full scan:\n%s", countPlan)

	pageSQL := fmt.Sprintf(`SELECT m.id FROM messages m
		WHERE %s
		ORDER BY COALESCE(m.sent_at, m.received_at, m.internal_date) DESC, m.id DESC
		LIMIT 20 OFFSET 0`, LiveMessagesWhere("m", true))
	pagePlan := explainPlan(t, s, pageSQL)
	assert.Contains(pagePlan, "idx_messages_live_sent_at",
		"page query should use the partial index:\n%s", pagePlan)
	assert.NotContains(pagePlan, "TEMP B-TREE",
		"page query should walk the index in order, not sort:\n%s", pagePlan)

	// The public API path returns the right rows: live messages only,
	// newest first, with the total excluding source-deleted rows.
	msgs, total, err := s.ListMessages(0, 20)
	require.NoError(err)
	assert.Equal(int64(1000), total, "1000 of 2000 messages are live")
	require.Len(msgs, 20)
	assert.Equal(int64(1999), msgs[0].ID, "newest live message id (2000 is deleted)")
	assert.Equal(int64(1997), msgs[1].ID)
}

// TestSearchMessages_DateBoundsUseInstantIndex verifies that timezone-correct
// date predicates can seek into a partial expression index. Without the
// matching index, julianday(COALESCE(...)) forces date-filtered result counts
// to scan every live message in large SQLite archives.
func TestSearchMessages_DateBoundsUseInstantIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	s, err := OpenForTest(filepath.Join(t.TempDir(), "search-date.db"))
	require.NoError(err)
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema())

	seedLiveMessages(t, s, 2000)

	const indexName = "idx_messages_live_timestamp_julianday"
	var idxCount int
	require.NoError(s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, indexName,
	).Scan(&idxCount))
	assert.Equal(1, idxCount, "%s should be created by InitSchema", indexName)

	countSQL := `SELECT COUNT(*) FROM messages m
		WHERE ` + LiveMessagesWhere("m", true) + `
		AND julianday(COALESCE(m.sent_at, m.received_at, m.internal_date)) >= julianday(?)`
	countPlan := explainPlan(t, s, countSQL, "2015-01-01T16:00:00-05:00")
	assert.Contains(countPlan, indexName,
		"date-filtered COUNT should seek the instant index:\n%s", countPlan)
}
