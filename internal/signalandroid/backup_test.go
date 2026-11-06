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

func TestImportRejectsDuplicateAttachmentFiles(t *testing.T) {
	for _, tc := range []struct {
		name, rowID string
		longText    bool
		fileCount   int
		wantStored  int64
	}{
		{name: "media", rowID: "500", fileCount: 2},
		{name: "three media files", rowID: "500", fileCount: 3},
		{name: "long text", rowID: "503", longText: true, fileCount: 2, wantStored: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewTestStore(t), t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			if tc.longText {
				changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 300, 'text/x-signal-plain')`)
			}
			for _, suffix := range []string{"-1", "1", "2"}[:tc.fileCount] {
				require.NoError(os.WriteFile(filepath.Join(dir, "Attachment_"+tc.rowID+"_"+suffix+".bin"), []byte("ambiguous export "+suffix), 0o600))
			}

			progress := &duplicateAttachmentProgress{}
			opts := DefaultOptions()
			opts.Phone, opts.AttachmentFilesDir, opts.AttachmentsDir = signalfixture.OwnPhone, dir, t.TempDir()
			summary, err := NewImporter(st, progress).Import(t.Context(), filepath.Join(dir, "database.sqlite"), opts)
			require.NoError(err)

			assert.Equal(int64(1), summary.Errors)
			require.Len(progress.errors, 1)
			require.ErrorContains(progress.errors[0], "multiple attachment files")
			assert.Equal(int64(2), summary.AttachmentsMissing)
			assert.Equal(tc.wantStored, summary.AttachmentsStored)
			assert.Equal(int(tc.wantStored), countRows(t, st, `SELECT COUNT(*) FROM attachments`))
			if tc.longText {
				id := "signal:aci:" + signalfixture.CarolACI + ":peer:" + ms(signalfixture.TCarol)
				assert.Equal("Carol here", importedMessages(t, st)[id].body)
			}
		})
	}
}

type duplicateAttachmentProgress struct {
	NullProgress

	errors []error
}

func (p *duplicateAttachmentProgress) OnError(err error) {
	p.errors = append(p.errors, err)
}

func TestLongTextHandling(t *testing.T) {
	for _, tc := range []struct {
		name, full, want, messageID, presentSearch, absentSearch string
		initial, revision                                        []string
		rerun                                                    bool
		fileState                                                string
		wantErrors, wantMissing                                  int64
	}{
		{name: "file missing", wantMissing: 2, fileState: "missing", want: "Carol here", presentSearch: "Carol"},
		{name: "empty file", wantMissing: 2, rerun: true, fileState: "empty", full: "Carol here archivedsuffix", want: "Carol here archivedsuffix", presentSearch: "archivedsuffix"},
		{name: "unreadable file", wantMissing: 2, rerun: true, fileState: "unreadable", full: "Carol here archivedsuffix", want: "Carol here archivedsuffix", presentSearch: "archivedsuffix", wantErrors: 1},
		{
			name: "edit changes revision date", wantMissing: 1, rerun: true, fileState: "present", full: "Carol here originalsuffix", want: "Carol EDITED short",
			revision: []string{
				`UPDATE message SET latest_revision_id = 301 WHERE _id = 300`,
				`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body, original_message_id) SELECT 301, date_sent + 1, date_received + 1, thread_id, from_recipient_id, to_recipient_id, type, 'Carol EDITED short', 300 FROM message WHERE _id = 300`,
				`INSERT INTO attachment (_id, message_id, content_type) VALUES (504, 301, 'text/x-signal-plain')`,
			},
			presentSearch: "EDITED", absentSearch: "originalsuffix",
		},
		{
			name: "edit with same preview", wantMissing: 1, rerun: true, fileState: "missing", full: "Carol here obsoleteending", want: "Carol here",
			revision: []string{
				`UPDATE message SET latest_revision_id = 301 WHERE _id = 300`,
				`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body, original_message_id) SELECT 301, date_sent + 1, date_received + 1, thread_id, from_recipient_id, to_recipient_id, type, body, 300 FROM message WHERE _id = 300`,
				`UPDATE attachment SET message_id = 301 WHERE _id = 503`,
			},
			absentSearch: "obsoleteending",
		},
		{
			name: "contact rename", wantMissing: 2, rerun: true, fileState: "missing", full: "Hi \uFFFC, look searchablesuffix", want: "Hi @Alice Example, look searchablesuffix\n\nBeach",
			messageID:     "signal:group:" + signalfixture.GroupB64 + ":" + signalfixture.BobACI + ":" + ms(signalfixture.TGroup),
			initial:       []string{`UPDATE attachment SET message_id = 200 WHERE _id = 503`},
			revision:      []string{`UPDATE recipient SET system_joined_name = 'Renamed Example' WHERE _id = 2`},
			presentSearch: "searchablesuffix",
		},
		{
			name: "raw preview changed", wantMissing: 2, rerun: true, fileState: "missing", full: "Carol here archivedsuffix", want: "Carol changed",
			revision:      []string{`UPDATE message SET body = 'Carol changed' WHERE _id = 300`},
			presentSearch: "changed", absentSearch: "archivedsuffix",
		},
		{
			name: "restored row IDs", wantMissing: 1, rerun: true, fileState: "missing", full: "Carol here archivedsuffix", want: "Carol here archivedsuffix",
			revision:      []string{`UPDATE message SET _id = 900 WHERE _id = 300`, `UPDATE attachment SET message_id = 900 WHERE _id = 503`},
			presentSearch: "archivedsuffix",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewSQLiteTestStore(t), t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 300, 'text/x-signal-plain')`)
			changeFixture(t, dir, tc.initial...)
			file, storage := filepath.Join(dir, "Attachment_503_-1.bin"), t.TempDir()
			if tc.rerun {
				require.NoError(os.WriteFile(file, []byte(tc.full), 0o600))
				runImport(t, st, dir, storage)
				changeFixture(t, dir, tc.revision...)
			}
			var progress ImportProgress
			switch tc.fileState {
			case "missing":
				if tc.rerun {
					require.NoError(os.Remove(file))
				}
			case "empty":
				require.NoError(os.WriteFile(file, nil, 0o600))
			case "unreadable":
				progress = longTextReadFailureProgress{onStart: func() {
					// Discovery must see a file before reading it fails.
					require.NoError(os.Remove(file))
					require.NoError(os.Mkdir(file, 0o700))
				}}
			}
			opts := DefaultOptions()
			opts.Phone, opts.AttachmentFilesDir, opts.AttachmentsDir = signalfixture.OwnPhone, dir, storage
			summary, err := NewImporter(st, progress).Import(t.Context(), filepath.Join(dir, "database.sqlite"), opts)
			require.NoError(err)
			assert.Equal(tc.wantErrors, summary.Errors)
			assert.Equal(int64(7), summary.MessagesAdded+summary.MessagesUpdated)
			assert.Equal(tc.wantMissing, summary.AttachmentsMissing)
			id := tc.messageID
			if id == "" {
				id = "signal:aci:" + signalfixture.CarolACI + ":peer:" + ms(signalfixture.TCarol)
			}
			assert.Equal(tc.want, importedMessages(t, st)[id].body)
			if tc.presentSearch != "" {
				assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, tc.presentSearch))
			}
			if tc.absentSearch != "" {
				assert.Zero(countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, tc.absentSearch))
			}
		})
	}
}

func TestUnavailableAttachmentKeepsMessage(t *testing.T) {
	for _, storageFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "oversize", true: "storage failure"}[storageFailure], func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			dir := t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.Current, 0)
			opts := DefaultOptions()
			opts.Phone, opts.AttachmentFilesDir, opts.AttachmentsDir = signalfixture.OwnPhone, dir, t.TempDir()
			if storageFailure {
				opts.AttachmentsDir = filepath.Join(t.TempDir(), "file")
				require.NoError(os.WriteFile(opts.AttachmentsDir, []byte("file"), 0o600))
			} else {
				opts.MaxAttachmentSize = 3
			}
			summary, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			require.NoError(err)
			assert.Equal(expectedMessages(), importedMessages(t, st))
			assert.Equal(int64(2), summary.AttachmentsMissing)
			assert.Equal(int64(1), summary.Errors)
		})
	}
}

func TestProducerVersionSidecar(t *testing.T) {
	for _, tc := range []struct {
		name, data, wantError string
	}{
		{"future", "VERSION:uint32:331\n", "database version 331 is newer"},
		{"old", "VERSION:uint32:184\n", "database version 184 predates Signal Android database version 185"},
		{"malformed", "VERSION:uint32:garbage\n", "invalid DatabaseVersion.sbf"},
		{"missing", "", "read DatabaseVersion.sbf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewTestStore(t), t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.Current, 0)
			versionPath := filepath.Join(dir, "DatabaseVersion.sbf")
			if tc.data != "" {
				require.NoError(os.WriteFile(versionPath, []byte(tc.data), 0o600))
			} else {
				require.NoError(os.Remove(versionPath))
			}
			opts := DefaultOptions()
			opts.Phone = signalfixture.OwnPhone
			_, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			require.ErrorIs(err, ErrUnsupportedDatabase)
			assert.Contains(err.Error(), tc.wantError)
			assert.Zero(countRows(t, st, `SELECT COUNT(*) FROM sources`))
		})
	}
}

func TestPhoneAppearsForArchivedSender(t *testing.T) {
	for _, tc := range []struct {
		name              string
		initial, revision []string
		wantPhone         bool
	}{
		{name: "phone learned after message expires", revision: []string{`DELETE FROM message WHERE _id = 200`, `UPDATE recipient SET e164 = '+15555550102' WHERE _id = 3`}, wantPhone: true},
		{name: "historical sender absent from membership", initial: []string{`DELETE FROM message WHERE thread_id <> 11`, `DELETE FROM group_membership WHERE recipient_id = 3`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewTestStore(t), t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, tc.initial...)
			runImport(t, st, dir, t.TempDir())
			if len(tc.revision) > 0 {
				changeFixture(t, dir, tc.revision...)
				runImport(t, st, dir, t.TempDir())
			}
			pid, _, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, signalfixture.BobACI)
			require.NoError(err)
			var sender int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT sender_id FROM messages WHERE source_message_id = ?`), "signal:group:"+signalfixture.GroupB64+":"+signalfixture.BobACI+":"+ms(signalfixture.TGroup)).Scan(&sender))
			assert.Equal(pid, sender)
			assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM conversation_participants cp JOIN messages m ON m.conversation_id = cp.conversation_id WHERE m.sender_id = ? AND cp.participant_id = m.sender_id`, pid))
			if tc.wantPhone {
				assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM participants WHERE id = ? AND phone_number = '+15555550102'`, sender))
			}
		})
	}
}

func TestDeferredReplyAfterBackupDropsChild(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE message SET _id = 107 WHERE _id = 100`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := DefaultOptions()
	opts.Phone, opts.BatchSize = signalfixture.OwnPhone, 1
	_, err := NewImporter(st, cancelProgress{cancel: cancel}).Import(ctx, path, opts)
	require.ErrorIs(t, err, context.Canceled)
	changeFixture(t, dir, `DELETE FROM message WHERE _id = 101`)
	runImport(t, st, dir, t.TempDir())
	var parent string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT p.source_message_id FROM messages m JOIN messages p ON p.id = m.reply_to_message_id WHERE m.source_message_id = ?`), "signal:aci:"+signalfixture.AliceACI+":self:"+ms(signalfixture.TReply)).Scan(&parent))
	assert.Equal(t, "signal:aci:"+signalfixture.AliceACI+":peer:"+ms(signalfixture.TAlice1), parent)
}

func TestStatsFailureCannotCompleteImport(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	dir := t.TempDir()
	path := signalfixture.Write(t, dir, signalfixture.Current, 0)
	_, err := st.DB().Exec(`CREATE TRIGGER fail_stats BEFORE UPDATE OF message_count ON conversations BEGIN SELECT RAISE(ABORT, 'stats failure'); END`)
	require.NoError(t, err)
	opts := DefaultOptions()
	opts.Phone = signalfixture.OwnPhone
	_, err = NewImporter(st, nil).Import(t.Context(), path, opts)
	require.ErrorContains(t, err, "stats failure")
	assert.Equal(t, 1, countRows(t, st, `SELECT COUNT(*) FROM sync_runs WHERE status = 'failed'`))
}

func TestNegativeLimit(t *testing.T) {
	opts := DefaultOptions()
	opts.Phone, opts.Limit = signalfixture.OwnPhone, -1
	_, err := NewImporter(testutil.NewTestStore(t), nil).Import(t.Context(), "missing.sqlite", opts)
	require.ErrorContains(t, err, "limit must be non-negative")
}

type longTextReadFailureProgress struct {
	NullProgress

	onStart func()
}

func (p longTextReadFailureProgress) OnStart() { p.onStart() }
