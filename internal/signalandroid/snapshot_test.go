package signalandroid

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	assert.Equal(t, int64(7), summary.MessagesAdded)
	assert.Len(t, importedMessages(t, st), 7)
	assert.Zero(t, summary.DuplicateIDs)
}

func TestLongTextAttachment(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	full := strings.Repeat("long text ", 300) + "searchablesuffix"
	changeFixture(t, dir, `UPDATE message SET body = 'preview' WHERE _id = 300`, `INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 300, 'text/x-signal-plain')`)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Attachment_503_-1.bin"), []byte(full), 0o600))
	runImport(t, st, dir, t.TempDir())
	assert.Equal(t, full, importedMessages(t, st)["signal:aci:0f0f0f0f-0000-4000-8000-000000000004:"+signalfixture.CarolACI+":"+ms(signalfixture.TCarol)].body)
	assert.Equal(t, 1, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchablesuffix'`))
}

func TestReplyQuotesLatestRevision(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE message SET quote_id = `+ms(signalfixture.TEditNew)+` WHERE _id = 103`)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, "signal:aci:0f0f0f0f-0000-4000-8000-000000000002:"+signalfixture.OwnACI+":"+ms(signalfixture.TEditOld), importedMessages(t, st)["signal:aci:0f0f0f0f-0000-4000-8000-000000000002:"+signalfixture.AliceACI+":"+ms(signalfixture.TQuoteDraft)].replyTo)
	assert.Zero(t, summary.RepliesUnresolved)
}

func TestBodyWriteFailurePreservesOldSnapshot(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	runImport(t, st, dir, t.TempDir())
	changeFixture(t, dir, `UPDATE message SET body = 'Changed text' WHERE _id = 300`)
	_, err := st.DB().Exec(`CREATE TRIGGER fail_body BEFORE UPDATE ON message_bodies WHEN NEW.body_text = 'Changed text' BEGIN SELECT RAISE(ABORT, 'body failure'); END`)
	require.NoError(t, err)
	summary := runImport(t, st, dir, t.TempDir())
	require.Positive(t, summary.Errors)
	assert.Equal(t, "Carol here", importedMessages(t, st)["signal:aci:0f0f0f0f-0000-4000-8000-000000000004:"+signalfixture.CarolACI+":"+ms(signalfixture.TCarol)].body)
	var snippet string
	require.NoError(t, st.DB().QueryRow(`SELECT snippet FROM messages WHERE source_message_id = ?`, "signal:aci:0f0f0f0f-0000-4000-8000-000000000004:"+signalfixture.CarolACI+":"+ms(signalfixture.TCarol)).Scan(&snippet))
	assert.Equal(t, "Carol here", snippet)
	assert.Equal(t, int64(5), summary.MessagesUpdated)
}

type cancelProgress struct {
	NullProgress
	cancel context.CancelFunc
}

func (p cancelProgress) OnProgress(_, _, _ int64) { p.cancel() }

func TestInterruptedImportKeepsConversationCount(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := DefaultOptions()
	opts.Phone, opts.BatchSize = signalfixture.OwnPhone, 2
	_, err := NewImporter(st, cancelProgress{cancel: cancel}).Import(ctx, path, opts)
	require.ErrorIs(t, err, context.Canceled)
	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT SUM(message_count) FROM conversations`).Scan(&count))
	assert.Equal(t, 2, countRows(t, st, `SELECT COUNT(*) FROM messages`))
	assert.Equal(t, 2, count)
}

func TestAttachmentStoreFailureIsError(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	opts := DefaultOptions()
	opts.Phone, opts.AttachmentFilesDir, opts.AttachmentsDir = signalfixture.OwnPhone, dir, t.TempDir()
	opts.MaxAttachmentSize = 3
	summary, err := NewImporter(st, nil).Import(t.Context(), path, opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), summary.Errors)
	assert.Equal(t, int64(2), summary.AttachmentsMissing)
}

func TestImportSnapshotRollsBackAttachmentAndReactionFailures(t *testing.T) {
	for _, tc := range []struct{ name, trigger string }{
		{"attachment", `CREATE TRIGGER fail_attachment BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT, 'attachment failure'); END`},
		{"reaction", `CREATE TRIGGER fail_reaction BEFORE INSERT ON reactions WHEN NEW.reaction_value = 'x' BEGIN SELECT RAISE(ABORT, 'reaction failure'); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewSQLiteTestStore(t)
			dir := t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			attachmentsDir := t.TempDir()
			runImport(t, st, dir, attachmentsDir)
			before := importedMessages(t, st)
			changeFixture(t, dir, `UPDATE message SET body = 'Changed group text' WHERE _id = 200`, `UPDATE reaction SET emoji = 'x' WHERE message_id = 200`, `INSERT INTO attachment (_id, message_id, content_type) VALUES (504, 200, 'image/jpeg')`)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "Attachment_504_-1.bin"), []byte("new photo"), 0o600))
			_, err := st.DB().Exec(tc.trigger)
			require.NoError(t, err)
			summary := runImport(t, st, dir, attachmentsDir)
			assert.Equal(t, int64(1), summary.Errors)
			assert.Equal(t, int64(5), summary.MessagesUpdated)
			assert.Equal(t, before, importedMessages(t, st))
			assert.Equal(t, 0, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'Changed'`))
			assert.Equal(t, 1, countRows(t, st, `SELECT COUNT(*) FROM reactions WHERE reaction_value = '👍'`))
		})
	}
}
