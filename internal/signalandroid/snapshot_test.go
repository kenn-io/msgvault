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

func TestBatchReadFailurePreservesArchivedText(t *testing.T) {
	for _, tc := range []struct{ name, change, wantError string }{
		{"attachments", `UPDATE attachment SET width = 'invalid' WHERE _id = 500`, "read Signal attachments"},
		{"mentions", `UPDATE mention SET range_start = 'invalid' WHERE message_id = 200`, "read Signal mentions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewSQLiteTestStore(t), t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, `DELETE FROM message WHERE thread_id <> 11`, `INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 200, 'text/x-signal-plain')`)
			require.NoError(os.WriteFile(filepath.Join(dir, "Attachment_503_-1.bin"), []byte("Hi \uFFFC, look searchablesuffix"), 0o600))
			storage := t.TempDir()
			runImport(t, st, dir, storage)
			before := importedMessages(t, st)
			id := "signal:group:" + signalfixture.GroupB64 + ":" + signalfixture.BobACI + ":" + ms(signalfixture.TGroup)
			require.Equal("Hi @Alice Example, look searchablesuffix\n\nBeach", before[id].body)

			changeFixture(t, dir, tc.change)
			opts := DefaultOptions()
			opts.Phone, opts.AttachmentFilesDir, opts.AttachmentsDir = signalfixture.OwnPhone, dir, storage
			summary, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			require.ErrorContains(err, tc.wantError)
			assert.Zero(summary.MessagesUpdated)
			assert.Equal(before, importedMessages(t, st))
			assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchablesuffix'`))
			assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'Alice'`))
		})
	}
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

func TestInterruptedImportKeepsConversationCount(t *testing.T) {
	for _, tc := range []struct {
		name      string
		changes   []string
		batchSize int
	}{
		{name: "conversation count", batchSize: 2},
		{name: "historical sender", batchSize: 1, changes: []string{
			`DELETE FROM message WHERE thread_id <> 11`,
			`DELETE FROM group_membership WHERE recipient_id = 3`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewTestStore(t), t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, tc.changes...)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			opts := DefaultOptions()
			opts.Phone, opts.BatchSize = signalfixture.OwnPhone, tc.batchSize
			summary, err := NewImporter(st, cancelProgress{cancel: cancel}).Import(ctx, path, opts)
			require.ErrorIs(err, context.Canceled)
			require.NotNil(summary)
			assert.Equal(int64(tc.batchSize), summary.MessagesAdded)
			assert.Equal(tc.batchSize, countRows(t, st, `SELECT COUNT(*) FROM messages`))
			if tc.name == "historical sender" {
				assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM conversation_participants cp JOIN messages m ON m.conversation_id = cp.conversation_id AND m.sender_id = cp.participant_id`))
			} else {
				var count int
				require.NoError(st.DB().QueryRow(`SELECT SUM(message_count) FROM conversations`).Scan(&count))
				assert.Equal(2, count)
				assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages WHERE reply_to_message_id IS NOT NULL`))
			}
		})
	}
}

func TestImportSnapshotRollsBackAttachmentAndReactionFailures(t *testing.T) {
	for _, tc := range []struct{ name, trigger string }{
		{"body", `CREATE TRIGGER fail_body BEFORE UPDATE ON message_bodies WHEN NEW.body_text = 'Changed text' BEGIN SELECT RAISE(ABORT, 'body failure'); END`},
		{"attachment", `CREATE TRIGGER fail_attachment BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT, 'attachment failure'); END`},
		{"reaction", `CREATE TRIGGER fail_reaction BEFORE INSERT ON reactions WHEN NEW.reaction_value = 'x' BEGIN SELECT RAISE(ABORT, 'reaction failure'); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			dir := t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			attachmentsDir := t.TempDir()
			runImport(t, st, dir, attachmentsDir)
			before := importedMessages(t, st)
			if tc.name == "body" {
				changeFixture(t, dir, `UPDATE message SET body = 'Changed text' WHERE _id = 300`)
			} else {
				changeFixture(t, dir, `UPDATE message SET body = 'Changed group text' WHERE _id = 200`, `UPDATE reaction SET emoji = 'x' WHERE message_id = 200`, `INSERT INTO attachment (_id, message_id, content_type) VALUES (504, 200, 'image/jpeg')`)
				require.NoError(os.WriteFile(filepath.Join(dir, "Attachment_504_-1.bin"), []byte("new photo"), 0o600))
			}
			_, err := st.DB().Exec(tc.trigger)
			require.NoError(err)
			summary := runImport(t, st, dir, attachmentsDir)
			assert.Equal(int64(1), summary.Errors)
			assert.Equal(int64(6), summary.MessagesUpdated)
			assert.Equal(before, importedMessages(t, st))
			if tc.name == "body" {
				var snippet string
				require.NoError(st.DB().QueryRow(`SELECT snippet FROM messages WHERE source_message_id = ?`, "signal:aci:"+signalfixture.CarolACI+":peer:"+ms(signalfixture.TCarol)).Scan(&snippet))
				assert.Equal("Carol here", snippet)
			}
			assert.Equal(0, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'Changed'`))
			assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM reactions WHERE reaction_value = '👍'`))
		})
	}
}
