package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

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
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var count int
		require.NoError(db.QueryRowContext(queryCtx, `SELECT COUNT(*) FROM messages`).Scan(&count))
		return count > 0
	})

	_, err := env.Syncer.Full(ctx, testEmail)
	require.ErrorIs(err, context.Canceled, "sync surfaces the cancellation")
	require.True(canceled(), "cancellation landed after the message commit")

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
