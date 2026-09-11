package importer

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
)

func TestImportEmlxPreservesMessageID(t *testing.T) {
	for _, tc := range []struct{ name, header, want string }{
		{"canonical", "<Case-ID@example.test>", "Case-ID@example.test"},
		{"bare", "bare@example.test", "bare@example.test"},
		{"missing", "", ""},
		{"empty brackets", "<>", ""},
		{"nested brackets", "<<bad@example.test>>", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st, tmp := openTestStore(t)
			root := filepath.Join(tmp, "Inbox.mbox")
			raw := email.NewMessage().From("sender@example.test").To("recipient@example.test").
				Header("Message-ID", tc.header).Body("body").Bytes()
			mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
			result, err := ImportEmlxDir(t.Context(), st, root, EmlxImportOptions{Identifier: "recipient@example.test"})
			requirements.NoError(err)
			assertions.Equal(int64(1), result.MessagesAdded)
			var got sql.NullString
			requirements.NoError(st.DB().QueryRow(`SELECT rfc822_message_id FROM messages`).Scan(&got))
			assertions.Equal(sql.NullString{String: tc.want, Valid: tc.want != ""}, got)
		})
	}
}

func TestImportEmlxLinksReplyRegardlessOfOrder(t *testing.T) {
	for _, childFirst := range []bool{false, true} {
		name := "parent first"
		if childFirst {
			name = "child first"
		}
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st, tmp := openTestStore(t)
			root := filepath.Join(tmp, "Inbox.mbox")
			parent := email.NewMessage().From("sender@example.test").To("recipient@example.test").
				Header("Message-ID", "<parent@example.test>").Subject("Parent").Body("parent body").Bytes()
			child := email.NewMessage().From("sender@example.test").To("recipient@example.test").
				Header("Message-ID", "<child@example.test>").Header("In-Reply-To", "<parent@example.test>").
				Subject("Reply").Body("child body").Bytes()
			first, second := parent, child
			if childFirst {
				first, second = child, parent
			}
			mkMailboxDir(t, root, map[string][]byte{"1.emlx": first, "2.emlx": second})
			result, err := ImportEmlxDir(t.Context(), st, root, EmlxImportOptions{Identifier: "recipient@example.test"})
			requirements.NoError(err)
			assertions.Equal(int64(2), result.MessagesAdded)
			var parentID int64
			requirements.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE subject = 'Parent'`).Scan(&parentID))
			var reply sql.NullInt64
			requirements.NoError(st.DB().QueryRow(`SELECT reply_to_message_id FROM messages WHERE subject = 'Reply'`).Scan(&reply))
			assertions.Equal(sql.NullInt64{Int64: parentID, Valid: true}, reply)
		})
	}
}

func TestImportEmlxRepairsMissingMessageIDOnReimport(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := email.NewMessage().From("sender@example.test").To("recipient@example.test").
		Header("Message-ID", "<old@example.test>").Body("original body").
		WithAttachment("original.txt", "text/plain", []byte("original attachment")).Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	opts := EmlxImportOptions{Identifier: "recipient@example.test", AttachmentsDir: filepath.Join(tmp, "attachments")}
	_, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	var originalID int64
	requirements.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&originalID))
	_, err = st.DB().Exec(`UPDATE messages SET rfc822_message_id = NULL, metadata = '{"other":"keep"}' WHERE id = ?`, originalID)
	requirements.NoError(err)
	// Snapshot the content-bearing rows to prove repair does not rewrite them.
	snapshot := func() [][][]any {
		t.Helper()
		var result [][][]any
		for _, statement := range []string{
			`SELECT source_id, source_message_id, conversation_id, subject, content_changed_at FROM messages WHERE id = ?`,
			`SELECT * FROM message_raw WHERE message_id = ?`,
			`SELECT * FROM message_bodies WHERE message_id = ?`,
			`SELECT * FROM message_labels WHERE message_id = ? ORDER BY label_id`,
			`SELECT * FROM attachments WHERE message_id = ? ORDER BY id`,
		} {
			table := func() [][]any {
				rows, err := st.DB().Query(statement, originalID)
				requirements.NoError(err)
				defer func() { _ = rows.Close() }()
				columns, err := rows.Columns()
				requirements.NoError(err)
				var table [][]any
				for rows.Next() {
					values := make([]any, len(columns))
					ptrs := make([]any, len(columns))
					for i := range values {
						ptrs[i] = &values[i]
					}
					requirements.NoError(rows.Scan(ptrs...))
					table = append(table, values)
				}
				requirements.NoError(rows.Err())
				requirements.NotEmpty(table, statement)
				return table
			}()
			result = append(result, table)
		}
		return result
	}
	originalContent := snapshot()
	before, err := st.DerivedDataRevision()
	requirements.NoError(err)
	result, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	assertions.Equal(int64(0), result.MessagesAdded)
	var gotID int64
	var rfcID sql.NullString
	var metadata string
	requirements.NoError(st.DB().QueryRow(`SELECT id, rfc822_message_id, metadata FROM messages`).Scan(&gotID, &rfcID, &metadata))
	assertions.Equal(originalID, gotID)
	assertions.Equal(sql.NullString{String: "old@example.test", Valid: true}, rfcID)
	assertions.JSONEq(`{"other":"keep"}`, metadata)
	after, err := st.DerivedDataRevision()
	requirements.NoError(err)
	assertions.Greater(after, before)
	_, err = ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	again, err := st.DerivedDataRevision()
	requirements.NoError(err)
	assertions.Equal(after, again)
	assertions.Equal(originalContent, snapshot())
}

func TestImportEmlxLinksReplyWhenParentArrivesLater(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	child := email.NewMessage().From("sender@example.test").Header("Message-ID", "<child@example.test>").
		Header("In-Reply-To", "<parent@example.test>").Subject("Reply").Body("reply").Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": child})
	opts := EmlxImportOptions{Identifier: "recipient@example.test"}
	_, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	var reply sql.NullInt64
	requirements.NoError(st.DB().QueryRow(`SELECT reply_to_message_id FROM messages WHERE subject = 'Reply'`).Scan(&reply))
	assertions.False(reply.Valid)
	parent := email.NewMessage().From("sender@example.test").Header("Message-ID", "<parent@example.test>").Subject("Parent").Body("parent").Bytes()
	mkEmlx(t, filepath.Join(root, "Messages"), "2.emlx", parent)
	_, err = ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	var parentID int64
	requirements.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE subject = 'Parent'`).Scan(&parentID))
	requirements.NoError(st.DB().QueryRow(`SELECT reply_to_message_id FROM messages WHERE subject = 'Reply'`).Scan(&reply))
	assertions.Equal(sql.NullInt64{Int64: parentID, Valid: true}, reply)
}

func TestImportEmlxResumesReplyPhaseWithoutReimporting(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := email.NewMessage().From("sender@example.test").Header("Message-ID", "<child@example.test>").
		Header("In-Reply-To", "<parent@example.test>").Body("original body").Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	opts := EmlxImportOptions{Identifier: "recipient@example.test"}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	// Reopen the durable checkpoint at the reply phase, as after an interrupted
	// reply pass. The file is now invalid, so a file-phase restart would fail.
	_, err = st.DB().Exec(`UPDATE sync_runs SET status = 'running', completed_at = NULL WHERE source_id = ?`, first.SourceID)
	requirements.NoError(err)
	requirements.NoError(os.WriteFile(filepath.Join(root, "Messages", "1.emlx"), []byte("invalid emlx"), 0600))
	resumed, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	assertions.True(resumed.WasResumed)
	assertions.Zero(resumed.MessagesProcessed)
	assertions.Zero(resumed.Errors)
	var status string
	requirements.NoError(st.DB().QueryRow(`SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
	assertions.Equal(store.SyncStatusCompleted, status)
}

func TestImportEmlxHeaderFailureDoesNotLoseAttachmentsOnResume(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := email.NewMessage().From("sender@example.test").Header("Message-ID", "<child@example.test>").
		Header("In-Reply-To", "<parent@example.test>").Subject("Thread").Body("searchneedle").
		WithAttachment("fixture.txt", "text/plain", []byte("attachment content")).Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	_, err := st.DB().Exec(`CREATE TRIGGER fail_email_metadata BEFORE UPDATE OF metadata ON messages
  WHEN NEW.metadata LIKE '%email_in_reply_to%' BEGIN SELECT RAISE(ABORT,'synthetic metadata failure'); END`)
	requirements.NoError(err)
	opts := EmlxImportOptions{Identifier: "recipient@example.test", AttachmentsDir: filepath.Join(tmp, "attachments")}
	result, err := ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	assertions.True(result.HardErrors)
	_, err = st.DB().Exec(`DROP TRIGGER fail_email_metadata`)
	requirements.NoError(err)
	result, err = ImportEmlxDir(t.Context(), st, root, opts)
	requirements.NoError(err)
	assertions.False(result.HardErrors)
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE filename = 'fixture.txt'`).Scan(&count))
	assertions.Equal(1, count)
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'searchneedle'`).Scan(&count))
	assertions.Equal(1, count)
}
