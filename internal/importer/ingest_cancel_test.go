package importer

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/email"
)

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
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var count int
		require.NoError(db.QueryRowContext(queryCtx, `SELECT COUNT(*) FROM messages`).Scan(&count))
		return count > 0
	})

	err = IngestRawMessage(
		ctx, st,
		src.ID, "owner@example.test", filepath.Join(t.TempDir(), "attachments"),
		nil, "source-msg-cancel", "fakehash",
		raw, time.Time{}, slog.New(slog.DiscardHandler),
		nil)
	require.ErrorIs(err, context.Canceled, "reply-header write still observes the cancellation")
	require.True(canceled(), "cancellation landed after the message commit")

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
