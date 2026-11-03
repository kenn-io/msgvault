package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

func queueFixture(t *testing.T) (*store.Store, store.MetaCommentActionClaim, int64) {
	t.Helper()
	st, c := actionStoreFixture(t)
	var conv int64
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT conversation_id FROM messages WHERE id=?"), c.MessageID).Scan(&conv))
	root, err := st.UpsertMessage(&store.Message{SourceID: c.SourceID, ConversationID: conv, SourceMessageID: "100_200", MessageType: "facebook_comments"})
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET reply_to_message_id=?,sent_at=? WHERE id=?"), root, time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), c.MessageID)
	require.NoError(t, err)
	return st, c, root
}
func TestCommentTriageSwap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, c, _ := queueFixture(t)
	for _, want := range []store.CommentTriageState{store.CommentTriageHandled, store.CommentTriageSkipped, store.CommentTriageOpen} {
		got, err := st.SetCommentTriageContext(t.Context(), c.MessageID, want, nil)
		require.NoError(err)
		assert.Equal(want, got)
		got, err = st.CommentTriageContext(t.Context(), c.MessageID)
		require.NoError(err)
		assert.Equal(want, got)
		var n int
		require.NoError(st.DB().QueryRow(st.Rebind("SELECT count(*) FROM message_labels WHERE message_id=?"), c.MessageID).Scan(&n))
		if want == store.CommentTriageOpen {
			assert.Zero(n)
		} else {
			assert.Equal(1, n)
		}
	}
}
func TestCommentTriageExpectConflict(t *testing.T) {
	st, c, _ := queueFixture(t)
	expect := store.CommentTriageHandled
	_, err := st.SetCommentTriageContext(t.Context(), c.MessageID, store.CommentTriageSkipped, &expect)
	require.ErrorIs(t, err, store.ErrCommentTriageConflict)
	got, err := st.CommentTriageContext(t.Context(), c.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.CommentTriageOpen, got)
}
func TestCommentTriageRejectsNonCommentRow(t *testing.T) {
	st, _, root := queueFixture(t)
	_, err := st.SetCommentTriageContext(t.Context(), root, store.CommentTriageHandled, nil)
	require.ErrorIs(t, err, store.ErrCommentTriageInvalid)
	_, err = st.SetCommentTriageContext(t.Context(), root, store.CommentTriageState("invalid"), nil)
	require.ErrorIs(t, err, store.ErrCommentTriageInvalid)
}
func TestCommentQueueFiltersAndOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, c, root := queueFixture(t)
	var conv int64
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT conversation_id FROM messages WHERE id=?"), root).Scan(&conv))
	earlier, err := st.UpsertMessage(&store.Message{SourceID: c.SourceID, ConversationID: conv, SourceMessageID: "100_200_400", MessageType: "facebook_comments", ReplyToMessageID: sql.NullInt64{Int64: root, Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), Valid: true}})
	require.NoError(err)
	f := store.CommentQueueFilter{MessageTypes: []string{"facebook_comments"}, Limit: 1}
	rows, err := st.ListCommentQueueContext(t.Context(), f)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(earlier, rows[0].MessageID)
	f.BeforeID = earlier
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(c.MessageID, rows[0].MessageID)
	f.BeforeID = c.MessageID
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(err)
	assert.Empty(rows)
	_, err = st.SetCommentTriageContext(t.Context(), c.MessageID, store.CommentTriageHandled, nil)
	require.NoError(err)
	f.BeforeID = 0
	f.State = "handled"
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(c.MessageID, rows[0].MessageID)
	require.NoError(st.MarkMessageDeletedContext(t.Context(), c.SourceID, c.TargetCommentID))
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(err)
	assert.Empty(rows)
	f.State = "all"
	f.Limit = 200
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(err)
	require.Len(rows, 2)
	assert.True(rows[1].DeletedFromSource)
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET is_from_me=TRUE WHERE id=?"), earlier)
	require.NoError(err)
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(err)
	require.Len(rows, 1)
	f.SourceID = c.SourceID + 100
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(err)
	assert.Empty(rows)
}
func TestCommentQueueSearchLabel(t *testing.T) {
	require := require.New(t)
	st, c, _ := queueFixture(t)
	_, err := st.SetCommentTriageContext(t.Context(), c.MessageID, store.CommentTriageHandled, nil)
	require.NoError(err)
	rows, _, err := st.SearchMessagesQueryContext(t.Context(), search.Parse("label:Handled"), 0, 10)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(t, c.MessageID, rows[0].ID)
}
func TestCommentQueuePageRepliedAndWindow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, c, _ := queueFixture(t)
	f := store.CommentQueueFilter{MessageTypes: []string{"facebook_comments"}}
	rows, err := st.ListCommentQueueContext(t.Context(), f)
	require.NoError(err)
	require.Len(rows, 1)
	require.NotNil(rows[0].PrivateReplyOpenUntil)
	assert.Equal(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), rows[0].PrivateReplyOpenUntil.UTC())
	var conv int64
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT conversation_id FROM messages WHERE id=?"), c.MessageID).Scan(&conv))
	_, err = st.UpsertMessage(&store.Message{SourceID: c.SourceID, ConversationID: conv, SourceMessageID: "100_200_500", MessageType: "facebook_comments", IsFromMe: true, ReplyToMessageID: sql.NullInt64{Int64: c.MessageID, Valid: true}})
	require.NoError(err)
	c.Action = "private_reply"
	claimAction(t, st, c)
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(err)
	require.Len(rows, 1)
	assert.True(rows[0].PageReplied)
	assert.Nil(rows[0].PrivateReplyOpenUntil)
}

// A waiting SELECT FOR UPDATE uses its original statement snapshot. Read label
// state only after ownership is acquired so expect cannot accept stale labels.
func TestCommentTriageExpectAfterLockWait(t *testing.T) {
	requirements := require.New(t)
	st, c, _ := queueFixture(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL statement snapshot regression")
	}
	label, err := st.EnsureLabel(c.SourceID, "msgvault:triage:handled", "Handled", "user")
	requirements.NoError(err)
	tx, err := st.DB().BeginTx(t.Context(), nil)
	requirements.NoError(err)
	defer func() { _ = tx.Rollback() }()
	var id int64
	requirements.NoError(tx.QueryRowContext(t.Context(), "SELECT id FROM messages WHERE id=$1 FOR UPDATE", c.MessageID).Scan(&id))
	expect := store.CommentTriageOpen
	done := make(chan error, 1)
	go func() {
		_, err := st.SetCommentTriageContext(t.Context(), c.MessageID, store.CommentTriageSkipped, &expect)
		done <- err
	}()
	requirements.Eventually(func() bool {
		var waiting bool
		err := st.DB().QueryRow("SELECT EXISTS (SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE a.datname=current_database() AND NOT l.granted)").Scan(&waiting)
		return err == nil && waiting
	}, 10*time.Second, 10*time.Millisecond, "wait for the observable row-lock wait")
	_, err = tx.ExecContext(t.Context(), "INSERT INTO message_labels(message_id,label_id) VALUES ($1,$2)", c.MessageID, label)
	requirements.NoError(err)
	requirements.NoError(tx.Commit())
	assert.ErrorIs(t, <-done, store.ErrCommentTriageConflict)
}

// Commit another WAL writer after triage has read its target. A transaction
// that has not reserved the writer slot can no longer upgrade its snapshot.
func TestCommentTriageReservesSQLiteWriterBeforeReading(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, c, _ := queueFixture(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite WAL snapshot regression")
	}
	st.DB().SetMaxOpenConns(2)
	st.DB().SetMaxIdleConns(2)
	writer, err := st.DB().Conn(t.Context())
	requirements.NoError(err)
	defer func() { _ = writer.Close() }()
	_, err = writer.ExecContext(t.Context(), "PRAGMA busy_timeout=0")
	requirements.NoError(err)
	conn, err := st.DB().Conn(t.Context())
	requirements.NoError(err)
	var attempted bool
	var writerErr error
	var native *sqlite3.SQLiteConn
	requirements.NoError(conn.Raw(func(driver any) error {
		var ok bool
		native, ok = driver.(*sqlite3.SQLiteConn)
		requirements.True(ok)
		native.RegisterAuthorizer(func(op int, table, column, database string) int {
			if !attempted && op == sqlite3.SQLITE_READ && table == "message_labels" {
				attempted = true
				_, writerErr = writer.ExecContext(t.Context(), "UPDATE messages SET snippet='concurrent import' WHERE id=?", c.MessageID)
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	requirements.NoError(conn.Close())
	defer native.RegisterAuthorizer(nil)
	_, err = st.SetCommentTriageContext(t.Context(), c.MessageID, store.CommentTriageHandled, nil)
	requirements.NoError(err)
	requirements.True(attempted)
	var busy sqlite3.Error
	requirements.ErrorAs(writerErr, &busy, "triage must own the writer slot before reading labels")
	assertions.Equal(sqlite3.ErrBusy, busy.Code)
	got, err := st.CommentTriageContext(t.Context(), c.MessageID)
	requirements.NoError(err)
	assertions.Equal(store.CommentTriageHandled, got)
}
