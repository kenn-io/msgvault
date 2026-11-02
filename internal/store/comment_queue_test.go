package store_test

import (
	"database/sql"
	"testing"
	"time"

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
	st, c, _ := queueFixture(t)
	for _, want := range []store.CommentTriageState{store.CommentTriageHandled, store.CommentTriageSkipped, store.CommentTriageOpen} {
		got, err := st.SetCommentTriageContext(t.Context(), c.MessageID, want, nil)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		got, err = st.CommentTriageContext(t.Context(), c.MessageID)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		var n int
		require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT count(*) FROM message_labels WHERE message_id=?"), c.MessageID).Scan(&n))
		if want == store.CommentTriageOpen {
			assert.Zero(t, n)
		} else {
			assert.Equal(t, 1, n)
		}
	}
}
func TestCommentTriageExpectConflict(t *testing.T) {
	st, c, _ := queueFixture(t)
	expect := store.CommentTriageHandled
	_, err := st.SetCommentTriageContext(t.Context(), c.MessageID, store.CommentTriageSkipped, &expect)
	assert.ErrorIs(t, err, store.ErrCommentTriageConflict)
	got, err := st.CommentTriageContext(t.Context(), c.MessageID)
	require.NoError(t, err)
	assert.Equal(t, store.CommentTriageOpen, got)
}
func TestCommentTriageRejectsNonCommentRow(t *testing.T) {
	st, _, root := queueFixture(t)
	_, err := st.SetCommentTriageContext(t.Context(), root, store.CommentTriageHandled, nil)
	assert.ErrorIs(t, err, store.ErrCommentTriageInvalid)
	_, err = st.SetCommentTriageContext(t.Context(), root, store.CommentTriageState("invalid"), nil)
	assert.ErrorIs(t, err, store.ErrCommentTriageInvalid)
}
func TestCommentQueueFiltersAndOrder(t *testing.T) {
	st, c, root := queueFixture(t)
	var conv int64
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT conversation_id FROM messages WHERE id=?"), root).Scan(&conv))
	earlier, err := st.UpsertMessage(&store.Message{SourceID: c.SourceID, ConversationID: conv, SourceMessageID: "100_200_400", MessageType: "facebook_comments", ReplyToMessageID: sql.NullInt64{Int64: root, Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), Valid: true}})
	require.NoError(t, err)
	f := store.CommentQueueFilter{MessageTypes: []string{"facebook_comments"}, Limit: 1}
	rows, err := st.ListCommentQueueContext(t.Context(), f)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, earlier, rows[0].MessageID)
	f.BeforeID = earlier
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, c.MessageID, rows[0].MessageID)
	f.BeforeID = c.MessageID
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(t, err)
	assert.Empty(t, rows)
	_, err = st.SetCommentTriageContext(t.Context(), c.MessageID, store.CommentTriageHandled, nil)
	require.NoError(t, err)
	f.BeforeID = 0
	f.State = "handled"
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, c.MessageID, rows[0].MessageID)
	require.NoError(t, st.MarkMessageDeletedContext(t.Context(), c.SourceID, c.TargetCommentID))
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(t, err)
	assert.Empty(t, rows)
	f.State = "all"
	f.Limit = 200
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.True(t, rows[1].DeletedFromSource)
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET is_from_me=TRUE WHERE id=?"), earlier)
	require.NoError(t, err)
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	f.SourceID = c.SourceID + 100
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(t, err)
	assert.Empty(t, rows)
}
func TestCommentQueueSearchLabel(t *testing.T) {
	st, c, _ := queueFixture(t)
	_, err := st.SetCommentTriageContext(t.Context(), c.MessageID, store.CommentTriageHandled, nil)
	require.NoError(t, err)
	rows, _, err := st.SearchMessagesQueryContext(t.Context(), search.Parse("label:Handled"), 0, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, c.MessageID, rows[0].ID)
}
func TestCommentQueuePageRepliedAndWindow(t *testing.T) {
	st, c, _ := queueFixture(t)
	f := store.CommentQueueFilter{MessageTypes: []string{"facebook_comments"}}
	rows, err := st.ListCommentQueueContext(t.Context(), f)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].PrivateReplyOpenUntil)
	assert.Equal(t, time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC), rows[0].PrivateReplyOpenUntil.UTC())
	var conv int64
	require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT conversation_id FROM messages WHERE id=?"), c.MessageID).Scan(&conv))
	_, err = st.UpsertMessage(&store.Message{SourceID: c.SourceID, ConversationID: conv, SourceMessageID: "100_200_500", MessageType: "facebook_comments", IsFromMe: true, ReplyToMessageID: sql.NullInt64{Int64: c.MessageID, Valid: true}})
	require.NoError(t, err)
	c.Action = "private_reply"
	claimAction(t, st, c)
	rows, err = st.ListCommentQueueContext(t.Context(), f)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].PageReplied)
	assert.Nil(t, rows[0].PrivateReplyOpenUntil)
}

// A waiting SELECT FOR UPDATE uses its original statement snapshot. Read label
// state only after ownership is acquired so expect cannot accept stale labels.
func TestCommentTriageExpectAfterLockWait(t *testing.T) {
	st, c, _ := queueFixture(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL statement snapshot regression")
	}
	label, err := st.EnsureLabel(c.SourceID, "msgvault:triage:handled", "Handled", "user")
	require.NoError(t, err)
	tx, err := st.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	var id int64
	require.NoError(t, tx.QueryRowContext(t.Context(), "SELECT id FROM messages WHERE id=$1 FOR UPDATE", c.MessageID).Scan(&id))
	expect := store.CommentTriageOpen
	done := make(chan error, 1)
	go func() {
		_, err := st.SetCommentTriageContext(t.Context(), c.MessageID, store.CommentTriageSkipped, &expect)
		done <- err
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := st.DB().QueryRow("SELECT EXISTS (SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE a.datname=current_database() AND NOT l.granted)").Scan(&waiting)
		return err == nil && waiting
	}, 10*time.Second, 10*time.Millisecond, "wait for the observable row-lock wait")
	_, err = tx.ExecContext(t.Context(), "INSERT INTO message_labels(message_id,label_id) VALUES ($1,$2)", c.MessageID, label)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	assert.ErrorIs(t, <-done, store.ErrCommentTriageConflict)
}
