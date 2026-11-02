package sync

import (
	"context"
	"log/slog"
	gosync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

// cancelAfterMessageCommitHandler cancels synchronously on the first committed
// transaction that leaves a message row behind, which is the message persist.
// The writes that follow it then run with an already-canceled context.
type cancelAfterMessageCommitHandler struct {
	slog.Handler

	mu               gosync.Mutex
	cancel           context.CancelFunc
	messageCommitted func() bool
	canceled         bool
}

func (*cancelAfterMessageCommitHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *cancelAfterMessageCommitHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message != "sql tx commit" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.canceled && h.messageCommitted() {
		h.cancel()
		h.canceled = true
	}
	return nil
}

// Not parallel: it replaces the process-wide slog default and SQL log options.
func TestFullSyncCanceledAfterMessageCommitFinishesAttachmentAndSearch(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 12345
	env.Mock.AddMessage("msg-cancel-after-commit", testemail.NewMessage().
		Subject("Cancel after commit").
		Body("postcommitsyncneedle body.").
		WithAttachment("report.bin", "application/octet-stream", []byte("attachment bytes")).
		Bytes(), []string{"INBOX"})
	withAttachmentsDir(t, env)

	db := env.Store.DB()
	queryCtx := context.WithoutCancel(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler := &cancelAfterMessageCommitHandler{
		Handler: slog.DiscardHandler,
		cancel:  cancel,
		messageCommitted: func() bool {
			var count int
			require.NoError(db.QueryRowContext(queryCtx, `SELECT COUNT(*) FROM messages`).Scan(&count))
			return count > 0
		},
	}
	// A commit over the slow threshold logs "sql tx slow" in place of the
	// line the handler waits for.
	store.ConfigureSQLLogging(store.SQLLogOptions{SlowMs: -1})
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })

	_, err := env.Syncer.Full(ctx, testEmail)
	require.ErrorIs(err, context.Canceled, "sync surfaces the cancellation")
	require.True(handler.canceled, "cancellation landed after the message commit")

	var messageID int64
	var hasAttachments bool
	var attachmentCount int
	require.NoError(db.QueryRowContext(t.Context(), `
		SELECT id, has_attachments, attachment_count FROM messages
		WHERE source_message_id = ?`, "msg-cancel-after-commit",
	).Scan(&messageID, &hasAttachments, &attachmentCount))
	assert.True(hasAttachments, "has_attachments")
	assert.Equal(1, attachmentCount, "attachment_count")

	var storedAttachments int
	require.NoError(db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM attachments WHERE message_id = ?`, messageID,
	).Scan(&storedAttachments))
	assert.Equal(1, storedAttachments, "attachment rows")

	var indexed int
	require.NoError(db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'postcommitsyncneedle'`,
	).Scan(&indexed))
	assert.Equal(1, indexed, "search index rows")
}
