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

func TestMissingLongTextOnRerun(t *testing.T) {
	for _, tc := range []struct {
		name, full, body, messageID, presentSearch, absentSearch string
		initial, revision                                        []string
		firstMissing                                             bool
		checkMissing                                             bool
	}{
		{
			name: "missing on rerun", full: strings.Repeat("long text ", 300) + "searchablesuffix",
			initial:       []string{`UPDATE message SET body = 'long text ' WHERE _id = 300`},
			presentSearch: "searchablesuffix", checkMissing: true,
		},
		{
			name: "missing on first import", firstMissing: true, body: "Carol here", checkMissing: true,
		},
		{
			name: "edited latest preview", full: "Carol here originalsuffix", body: "Carol EDITED short",
			revision: []string{
				`UPDATE message SET latest_revision_id = 301 WHERE _id = 300`,
				`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body, original_message_id) SELECT 301, date_sent + 1, date_received + 1, thread_id, from_recipient_id, to_recipient_id, type, 'Carol EDITED short', 300 FROM message WHERE _id = 300`,
				`INSERT INTO attachment (_id, message_id, content_type) VALUES (504, 301, 'text/x-signal-plain')`,
			},
			presentSearch: "EDITED", absentSearch: "originalsuffix",
		},
		{
			name: "edit with same preview", full: "Carol here obsoleteending", body: "Carol here",
			revision: []string{
				`UPDATE message SET latest_revision_id = 301 WHERE _id = 300`,
				`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body, original_message_id) SELECT 301, date_sent + 1, date_received + 1, thread_id, from_recipient_id, to_recipient_id, type, body, 300 FROM message WHERE _id = 300`,
				`UPDATE attachment SET message_id = 301 WHERE _id = 503`,
			},
			absentSearch: "obsoleteending",
		},
		{
			name: "contact rename", full: "Hi \uFFFC, look searchablesuffix",
			messageID:     "signal:group:" + signalfixture.GroupB64 + ":" + signalfixture.BobACI + ":" + ms(signalfixture.TGroup),
			initial:       []string{`UPDATE attachment SET message_id = 200 WHERE _id = 503`},
			revision:      []string{`UPDATE recipient SET system_joined_name = 'Renamed Example' WHERE _id = 2`},
			presentSearch: "searchablesuffix",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewSQLiteTestStore(t), t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 300, 'text/x-signal-plain')`)
			changeFixture(t, dir, tc.initial...)
			file, storage := filepath.Join(dir, "Attachment_503_-1.bin"), t.TempDir()
			if !tc.firstMissing {
				require.NoError(os.WriteFile(file, []byte(tc.full), 0o600))
			}
			summary := runImport(t, st, dir, storage)
			if !tc.firstMissing {
				// The latest-preview case keeps the original file, but the new revision's file is missing.
				if tc.name != "edited latest preview" {
					require.NoError(os.Remove(file))
				}
				changeFixture(t, dir, tc.revision...)
				summary = runImport(t, st, dir, storage)
			}
			id := tc.messageID
			if id == "" {
				id = "signal:aci:" + signalfixture.CarolACI + ":" + signalfixture.CarolACI + ":" + ms(signalfixture.TCarol)
			}
			body := importedMessages(t, st)[id].body
			if tc.name == "contact rename" {
				assert.Contains(body, "searchablesuffix")
			} else {
				want := tc.body
				if want == "" {
					want = tc.full
				}
				assert.Equal(want, body)
			}
			if tc.checkMissing {
				assert.Equal(int64(2), summary.AttachmentsMissing)
			}
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
		name, data string
		wantError  bool
	}{
		{"supported", "VERSION:uint32:330\n", false},
		{"future", "VERSION:uint32:331\n", true},
		{"old", "VERSION:uint32:184\n", true},
		{"malformed", "VERSION:uint32:garbage\n", true},
		{"missing", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewTestStore(t), t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, `PRAGMA user_version = 0`)
			versionPath := filepath.Join(dir, "DatabaseVersion.sbf")
			if tc.data != "" {
				require.NoError(os.WriteFile(versionPath, []byte(tc.data), 0o600))
			} else {
				_ = os.Remove(versionPath)
			}
			opts := DefaultOptions()
			opts.Phone = signalfixture.OwnPhone
			_, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			if tc.wantError {
				require.ErrorIs(err, ErrUnsupportedDatabase)
				if tc.name == "future" {
					assert.Contains(err.Error(), "331")
				}
				assert.Zero(countRows(t, st, `SELECT COUNT(*) FROM sources`))
			} else {
				require.NoError(err)
				assert.Len(importedMessages(t, st), 6)
			}
		})
	}
}

func TestPhoneAppearsForArchivedSender(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir := testutil.NewTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	runImport(t, st, dir, t.TempDir())
	changeFixture(t, dir, `DELETE FROM message WHERE _id = 200`, `UPDATE recipient SET e164 = '+15555550102' WHERE _id = 3`)
	runImport(t, st, dir, t.TempDir())
	pid, _, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, signalfixture.BobACI)
	require.NoError(err)
	var sender int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT sender_id FROM messages WHERE source_message_id = ?`), "signal:group:"+signalfixture.GroupB64+":"+signalfixture.BobACI+":"+ms(signalfixture.TGroup)).Scan(&sender))
	assert.Equal(pid, sender)
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM conversation_participants cp JOIN participants p ON p.id = cp.participant_id WHERE p.id = ?`, pid))
	assert.Equal(0, countRows(t, st, `SELECT COUNT(*) FROM participants WHERE id = ? AND phone_number IS NULL`, sender))
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
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT p.source_message_id FROM messages m JOIN messages p ON p.id = m.reply_to_message_id WHERE m.source_message_id = ?`), "signal:aci:"+signalfixture.AliceACI+":"+signalfixture.OwnACI+":"+ms(signalfixture.TReply)).Scan(&parent))
	assert.Equal(t, "signal:aci:"+signalfixture.AliceACI+":"+signalfixture.AliceACI+":"+ms(signalfixture.TAlice1), parent)
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
