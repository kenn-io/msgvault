package store

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/stacktest"
)

// A body rewrite holds the message lock until commit. Backfill must acquire
// that same lock before reading, or it can restore a flag from the old HTML
// after the rewrite commits and permanently advance past the stale flag.
func TestHiddenTextBackfillSerializesWithPostgreSQLBodyRewrite(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !IsPostgresURL(dbURL) {
		t.Skip("requires PostgreSQL row locks")
	}
	for _, tc := range []struct {
		name string
		html sql.NullString
	}{
		{"visible", sql.NullString{String: `<p>Visible text</p>`, Valid: true}},
		{"absent", sql.NullString{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := newPGStoreInternal(t, dbURL)
			source, err := st.GetOrCreateSource("gmail", "backfill@example.test")
			require.NoError(err)
			conversation, err := st.EnsureConversation(source.ID, "hidden-text-race", "Synthetic conversation")
			require.NoError(err)
			id, err := st.UpsertMessage(&Message{
				SourceID: source.ID, ConversationID: conversation, SourceMessageID: "hidden-text-race", MessageType: "email",
			})
			require.NoError(err)
			text := sql.NullString{String: "Archived text", Valid: true}
			require.NoError(st.UpsertMessageBody(id, text,
				sql.NullString{String: `<div hidden>Hidden text</div>`, Valid: true}))
			// Simulate an archive with an unfinished backfill and no stored flag.
			_, err = st.db.Exec(`UPDATE messages SET hidden_text_present = FALSE WHERE id = ?`, id)
			require.NoError(err)
			_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationMessagesHiddenTextBackfill)
			require.NoError(err)
			_, err = st.db.Exec(`INSERT INTO archive_metadata (key, value) VALUES (?, ?)`,
				hiddenTextBackfillCursorKey, strconv.FormatInt(id-1, 10))
			require.NoError(err)

			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			writer, err := st.db.BeginTx(ctx, nil)
			require.NoError(err)
			defer func() { _ = writer.Rollback() }()
			var writerPID int
			require.NoError(writer.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID))
			// Use the production body's lock and write path, holding its transaction
			// open so the backfill can only see the previous committed HTML.
			require.NoError(st.lockMeetingEvidenceWith(ctx, writer, id))
			require.NoError(upsertMessageBody(boundQuerier{ctx: ctx, q: writer}, st.dialect, st.fts5Available, id, text, tc.html))
			done := make(chan error, 1)
			go func() { done <- st.backfillMessagesHiddenText(ctx) }()
			poll := time.NewTicker(10 * time.Millisecond)
			defer poll.Stop()
			for {
				var waiting bool
				require.NoError(st.db.QueryRowContext(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
					AND ? = ANY(pg_blocking_pids(pid)))`, writerPID).Scan(&waiting))
				if waiting {
					break
				}
				select {
				case err := <-done:
					require.NoError(err)
					require.FailNow("backfill completed while the body rewrite was uncommitted")
				case <-poll.C:
				case <-ctx.Done():
					stacktest.Dump(t)
					require.NoError(ctx.Err(), "backfill did not wait for the body rewrite")
				}
			}
			require.NoError(writer.Commit())
			select {
			case err := <-done:
				require.NoError(err)
			case <-ctx.Done():
				stacktest.Dump(t)
				require.NoError(ctx.Err(), "backfill did not finish after the body rewrite")
			}
			var hidden bool
			require.NoError(st.db.QueryRowContext(ctx,
				`SELECT hidden_text_present FROM messages WHERE id = ?`, id).Scan(&hidden))
			assert.False(hidden, "backfill must not restore the old HTML's flag")
			var body sql.NullString
			require.NoError(st.db.QueryRowContext(ctx,
				`SELECT body_html FROM message_bodies WHERE message_id = ?`, id).Scan(&body))
			assert.Equal(tc.html, body)
			var cursors int
			require.NoError(st.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM archive_metadata WHERE key = ?`, hiddenTextBackfillCursorKey).Scan(&cursors))
			assert.Equal(0, cursors)
			applied, err := st.IsMigrationAppliedContext(ctx, migrationMessagesHiddenTextBackfill, 1)
			require.NoError(err)
			assert.True(applied, "the completed backfill will not revisit this message")
		})
	}
}
