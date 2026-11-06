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

func TestLongTextAttachment(t *testing.T) {
	for _, tc := range []struct {
		name, full, body, messageID, search string
		changes                             []string
	}{
		{
			name: "short edit", full: "Original long text", body: "Short replacement",
			messageID: "signal:aci:" + signalfixture.AliceACI + ":" + signalfixture.OwnACI + ":" + ms(signalfixture.TEditOld),
			search:    "replacement",
			changes:   []string{`INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 102, 'text/x-signal-plain')`, `UPDATE message SET body = 'Short replacement' WHERE _id = 106`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewSQLiteTestStore(t), t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, tc.changes...)
			require.NoError(os.WriteFile(filepath.Join(dir, "Attachment_503_-1.bin"), []byte(tc.full), 0o600))
			runImport(t, st, dir, t.TempDir())
			want := tc.body
			if want == "" {
				want = tc.full
			}
			assert.Equal(want, importedMessages(t, st)[tc.messageID].body)
			assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, tc.search))
		})
	}
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
			assert.ErrorContains(err, tc.wantError)
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
	assert.Equal(t, "signal:aci:0f0f0f0f-0000-4000-8000-000000000002:"+signalfixture.OwnACI+":"+ms(signalfixture.TEditOld), importedMessages(t, st)["signal:aci:0f0f0f0f-0000-4000-8000-000000000002:"+signalfixture.AliceACI+":"+ms(signalfixture.TQuoteDraft)].replyTo)
	assert.Zero(t, summary.RepliesUnresolved)
}

type cancelProgress struct {
	NullProgress

	cancel context.CancelFunc
}

func (p cancelProgress) OnProgress(_, _, _ int64) { p.cancel() }

func TestInterruptedImportKeepsConversationCount(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := DefaultOptions()
	opts.Phone, opts.BatchSize = signalfixture.OwnPhone, 2
	_, err := NewImporter(st, cancelProgress{cancel: cancel}).Import(ctx, path, opts)
	require.ErrorIs(err, context.Canceled)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT SUM(message_count) FROM conversations`).Scan(&count))
	assert.Equal(2, countRows(t, st, `SELECT COUNT(*) FROM messages`))
	assert.Equal(2, count)
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages WHERE reply_to_message_id IS NOT NULL`))
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
				require.NoError(st.DB().QueryRow(`SELECT snippet FROM messages WHERE source_message_id = ?`, "signal:aci:"+signalfixture.CarolACI+":"+signalfixture.CarolACI+":"+ms(signalfixture.TCarol)).Scan(&snippet))
				assert.Equal("Carol here", snippet)
			}
			assert.Equal(0, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'Changed'`))
			assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM reactions WHERE reaction_value = '👍'`))
		})
	}
}
