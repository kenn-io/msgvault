package importer

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/email"
)

// cancelAfterMessageCommitHandler cancels synchronously on the first committed
// transaction that leaves a message row behind, which is the message persist.
// The writes that follow it then run with an already-canceled context.
type cancelAfterMessageCommitHandler struct {
	slog.Handler

	mu               sync.Mutex
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
func TestIngestRawMessage_CanceledAfterMessageCommitFinishesAttachmentAndSearch(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource("test", "owner@example.test")
	require.NoError(err, "get/create source")
	raw := email.NewMessage().
		From("Sender Example <sender@example.test>").
		To("owner@example.test").
		Subject("Cancel after commit").
		Body("postcommitimportneedle body.").
		WithAttachment("report.bin", "application/octet-stream", []byte("attachment bytes")).
		Bytes()

	db := st.DB()
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

	err = IngestRawMessage(
		ctx, st,
		src.ID, "owner@example.test", filepath.Join(t.TempDir(), "attachments"),
		nil, "source-msg-cancel", "fakehash",
		raw, time.Time{}, slog.New(slog.DiscardHandler),
	)
	require.ErrorIs(err, context.Canceled, "reply-header write still observes the cancellation")
	require.True(handler.canceled, "cancellation landed after the message commit")

	var messageID int64
	var hasAttachments bool
	var attachmentCount int
	require.NoError(db.QueryRowContext(t.Context(), `
		SELECT id, has_attachments, attachment_count FROM messages
		WHERE source_message_id = ?`, "source-msg-cancel",
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
		`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'postcommitimportneedle'`,
	).Scan(&indexed))
	assert.Equal(1, indexed, "search index rows")
}
