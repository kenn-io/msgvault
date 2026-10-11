package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// A body write that cannot also clear the message's embedding generation
// must not commit: the new text would otherwise keep the old embedding.
func TestUpsertMessageBodyRollsBackWhenEmbeddingResetFails(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("uses a SQLite trigger to fail the embedding reset")
	}
	source, err := st.GetOrCreateSource("slack", "TEXAMPLE:UEXAMPLE")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "CEXAMPLE", "Example")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: "CEXAMPLE:1", MessageType: "slack",
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(id, sql.NullString{String: "original text", Valid: true}, sql.NullString{}))
	_, err = st.DB().Exec(`UPDATE messages SET embed_gen = 42 WHERE id = ?`, id)
	require.NoError(err)
	_, err = st.DB().Exec(`CREATE TRIGGER fail_embed_reset BEFORE UPDATE OF embed_gen ON messages
		WHEN NEW.embed_gen IS NULL BEGIN SELECT RAISE(ABORT, 'synthetic embedding reset failure'); END`)
	require.NoError(err)

	err = st.UpsertMessageBodyContext(t.Context(), id, sql.NullString{String: "edited text", Valid: true}, sql.NullString{})
	require.ErrorContains(err, "synthetic embedding reset failure")

	var body string
	var embedGen sql.NullInt64
	require.NoError(st.DB().QueryRow(`SELECT b.body_text, m.embed_gen FROM message_bodies b
		JOIN messages m ON m.id = b.message_id WHERE b.message_id = ?`, id).Scan(&body, &embedGen))
	assert.Equal(t, "original text", body)
	assert.Equal(t, sql.NullInt64{Int64: 42, Valid: true}, embedGen)
}

// A body write that waits behind a concurrent message write must compare
// against the body that write committed. Otherwise it restores the earlier
// text without invalidating the search document the other write built.
func TestUpsertMessageBodyComparesAfterConcurrentPostgreSQLWrite(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("uses PostgreSQL row locks")
	}
	source, err := st.GetOrCreateSource("slack", "TEXAMPLE:UEXAMPLE")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "CEXAMPLE", "Example")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversation, SourceMessageID: "CEXAMPLE:1", MessageType: "slack",
	})
	require.NoError(err)
	original := sql.NullString{String: "original text", Valid: true}
	require.NoError(st.UpsertMessageBody(id, original, sql.NullString{}))

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	concurrent, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = concurrent.Rollback() }()
	var blockerPID int
	require.NoError(concurrent.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID))
	_, err = concurrent.ExecContext(ctx, `UPDATE messages SET search_fts = to_tsvector('simple', 'concurrent'),
		indexing_version = 1 WHERE id = $1`, id)
	require.NoError(err)
	_, err = concurrent.ExecContext(ctx, `UPDATE message_bodies SET body_text = 'concurrent text' WHERE message_id = $1`, id)
	require.NoError(err)

	done := make(chan error, 1)
	go func() { done <- st.UpsertMessageBodyContext(ctx, id, original, sql.NullString{}) }()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		var waiting bool
		require.NoError(st.DB().QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
			AND $1 = ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&waiting))
		if waiting {
			break
		}
		select {
		case <-poll.C:
		case <-ctx.Done():
			require.NoError(ctx.Err(), "the body write never waited for the concurrent write")
		}
	}
	require.NoError(concurrent.Commit())
	require.NoError(<-done)

	var body string
	var indexed bool
	require.NoError(st.DB().QueryRow(`SELECT b.body_text, m.search_fts IS NOT NULL FROM message_bodies b
		JOIN messages m ON m.id = b.message_id WHERE b.message_id = $1`, id).Scan(&body, &indexed))
	assert.Equal(t, "original text", body)
	assert.False(t, indexed, "the concurrent search document no longer matches the body")
}
