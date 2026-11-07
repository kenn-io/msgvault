package signalandroid

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

func TestSameTimestampDifferentChats(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body) VALUES (202, `+ms(signalfixture.TReply)+`, `+ms(signalfixture.TReply+1)+`, 11, 1, 5, `+ms(signalfixture.TypeOutgoing)+`, 'Same time, another chat')`)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, int64(8), summary.MessagesAdded)
	assert.Len(t, importedMessages(t, st), 8)
}

func TestReplyQuotesLatestRevision(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE message SET quote_id = `+ms(signalfixture.TEditNew)+` WHERE _id = 103`)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, "signal:aci:0f0f0f0f-0000-4000-8000-000000000002:self:"+ms(signalfixture.TEditOld), importedMessages(t, st)["signal:aci:0f0f0f0f-0000-4000-8000-000000000002:peer:"+ms(signalfixture.TQuoteDraft)].replyTo)
	assert.Zero(t, summary.RepliesUnresolved)
}

type cancelProgress struct {
	NullProgress

	cancel context.CancelFunc
}

func (p cancelProgress) OnProgress(_, _, _ int64) { p.cancel() }

type cancelChatProgress struct {
	NullProgress

	cancel context.CancelFunc
}

func (p cancelChatProgress) OnChatStart(string, string, int) { p.cancel() }

func TestInterruptedFinalGroupChatReturnsCancellation(t *testing.T) {
	st, dir := testutil.NewTestStore(t), t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `DELETE FROM thread WHERE _id <> 11`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := DefaultOptions()
	opts.Phone = signalfixture.OwnPhone
	_, err := NewImporter(st, cancelChatProgress{cancel: cancel}).Import(ctx, path, opts)
	require.ErrorIs(t, err, context.Canceled)
}

func TestInterruptedImportKeepsConversationState(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir := testutil.NewTestStore(t), t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := DefaultOptions()
	opts.Phone, opts.BatchSize = signalfixture.OwnPhone, 2
	summary, err := NewImporter(st, cancelProgress{cancel: cancel}).Import(ctx, path, opts)
	require.ErrorIs(err, context.Canceled)
	require.NotNil(summary)
	assert.Equal(int64(2), summary.MessagesAdded)
	assert.Equal(2, countRows(t, st, `SELECT COUNT(*) FROM messages`))
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT SUM(message_count) FROM conversations`).Scan(&count))
	assert.Equal(2, count)
}

// This test replaces the process-wide SQL logger and must run serially.
func TestImportCancelAfterLastMessageCommitFailsSync(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir := testutil.NewSQLiteTestStore(t), t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	opts := DefaultOptions()
	opts.Phone = signalfixture.OwnPhone
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	queryCtx := context.WithoutCancel(t.Context())
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var count int
		require.NoError(st.DB().QueryRowContext(queryCtx, `SELECT COUNT(*) FROM messages`).Scan(&count))
		return count == 7
	})

	_, err := NewImporter(st, nil).Import(ctx, path, opts)
	require.ErrorIs(err, context.Canceled)
	require.True(canceled(), "cancellation must land after the last message commits")

	var status string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
	assert.Equal("failed", status)
	var counted, stored int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(SUM(message_count), 0) FROM conversations`).Scan(&counted))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&stored))
	assert.Equal(stored, counted)
}

func TestImportSnapshotReportsWriteFailures(t *testing.T) {
	for _, stage := range []string{"attachment-read", "mention-read", "body-write", "attachment-write", "reactor-conflict"} {
		t.Run(stage, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir, storage := testutil.NewSQLiteTestStore(t), t.TempDir(), t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.Current, 0)
			readFailure := stage == "attachment-read" || stage == "mention-read"
			if readFailure {
				changeFixture(t, dir, `DELETE FROM message WHERE thread_id <> 11`, `INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 200, 'text/x-signal-plain')`)
				require.NoError(os.WriteFile(filepath.Join(dir, "Attachment_503_-1.bin"), []byte("Hi \uFFFC, look searchablesuffix"), 0o600))
			}
			if stage == "reactor-conflict" {
				// Alice sends the group message; Bob is another group member.
				changeFixture(t, dir, `UPDATE message SET from_recipient_id = 2 WHERE _id = 200`)
			}
			runImport(t, st, dir, storage)
			before := importedMessages(t, st)
			progress := onStartProgress{onStart: func() {}}
			wantError := ""
			switch stage {
			case "attachment-read", "mention-read":
				id := "signal:group:" + signalfixture.GroupB64 + ":" + signalfixture.BobACI + ":" + ms(signalfixture.TGroup)
				require.Equal("Hi @Alice Example, look searchablesuffix\n\nBeach", before[id].body)
				table := "attachment"
				wantError = "read Signal attachments"
				if stage == "mention-read" {
					table = "mention"
					wantError = "read Signal mentions"
				}
				progress.onStart = func() { changeFixture(t, dir, "DROP TABLE "+table) }
			case "body-write":
				changeFixture(t, dir, `UPDATE message SET body = 'Changed text' WHERE _id = 300`)
				_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_body BEFORE UPDATE ON message_bodies WHEN NEW.body_text = 'Changed text' BEGIN SELECT RAISE(ABORT, 'body failure'); END`)
				require.NoError(err)
			case "attachment-write":
				changeFixture(t, dir, `UPDATE message SET body = 'Changed group text' WHERE _id = 200`, `UPDATE reaction SET emoji = 'x' WHERE message_id = 200`, `INSERT INTO attachment (_id, message_id, content_type) VALUES (504, 200, 'image/jpeg')`)
				require.NoError(os.WriteFile(filepath.Join(dir, "Attachment_504_-1.bin"), []byte("new photo"), 0o600))
				_, err := st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_attachment BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT, 'attachment failure'); END`)
				require.NoError(err)
			case "reactor-conflict":
				// A new phone for an ACI-only reactor conflicts with two curated people.
				aciPID, _, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, signalfixture.BobACI)
				require.NoError(err)
				_, _, err = st.CreatePersonFromParticipant(aciPID)
				require.NoError(err)
				phonePID, err := st.EnsureParticipantByPhone(t.Context(), "+15555550102", "Other Example", SourceType)
				require.NoError(err)
				_, _, err = st.CreatePersonFromParticipant(phonePID)
				require.NoError(err)
				changeFixture(t, dir, `DELETE FROM group_membership WHERE recipient_id = 3`, `UPDATE recipient SET e164 = '+15555550102' WHERE _id = 3`, `UPDATE message SET body = 'Changed text' WHERE _id = 200`, `INSERT INTO reaction (message_id, author_id, emoji, date_sent, date_received) VALUES (200, 3, 'x', 1700000300100, 1700000300101)`)
			}
			opts := DefaultOptions()
			opts.Phone, opts.AttachmentFilesDir, opts.AttachmentsDir = signalfixture.OwnPhone, dir, storage
			summary, err := NewImporter(st, progress).Import(t.Context(), path, opts)
			if readFailure {
				require.ErrorContains(err, wantError)
				assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchablesuffix'`))
				assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'Alice'`))
			} else {
				require.NoError(err)
				if stage == "reactor-conflict" {
					require.Positive(summary.Errors)
				} else {
					assert.Equal(int64(1), summary.Errors)
					assert.Equal(int64(6), summary.MessagesUpdated)
				}
			}
			assert.Equal(before, importedMessages(t, st))
			if stage == "body-write" {
				var snippet string
				require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT snippet FROM messages WHERE source_message_id = ?`, "signal:aci:"+signalfixture.CarolACI+":peer:"+ms(signalfixture.TCarol)).Scan(&snippet))
				assert.Equal("Carol here", snippet)
			}
			assert.Equal(0, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'Changed'`))
		})
	}
}
