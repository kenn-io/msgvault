package signalandroid

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

// changeFixture applies statements to a fixture database, as a newer backup
// of the same account would differ.
func changeFixture(t *testing.T, dir string, stmts ...string) {
	t.Helper()
	require := require.New(t)
	db, err := sql.Open("sqlite3", filepath.Join(dir, "database.sqlite"))
	require.NoError(err)
	defer func() { require.NoError(db.Close()) }()
	for _, stmt := range stmts {
		_, err := db.Exec(stmt)
		require.NoError(err, stmt)
	}
}

func countRows(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRow(st.Rebind(query), args...).Scan(&n))
	return n
}

func TestRerunReplacesReactions(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	runImport(t, st, dir, t.TempDir())

	// Alice changes her reaction on the group message and withdraws the one
	// on the edited draft.
	changeFixture(t, dir,
		`UPDATE reaction SET emoji = '`+"\U0001F602"+`' WHERE message_id = 200`,
		`DELETE FROM reaction WHERE message_id = 106`)
	summary := runImport(t, st, dir, t.TempDir())

	assert.Equal(int64(1), summary.ReactionsAdded)
	reactionsOf := func(id string) []string {
		rows, err := st.DB().Query(st.Rebind(`
			SELECT r.reaction_value FROM reactions r JOIN messages m ON m.id = r.message_id
			WHERE m.source_message_id = ? ORDER BY r.reaction_value`), id)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		var values []string
		for rows.Next() {
			var v string
			require.NoError(t, rows.Scan(&v))
			values = append(values, v)
		}
		require.NoError(t, rows.Err())
		return values
	}
	assert.Equal([]string{"\U0001F602"}, reactionsOf("signal:group:ABEiM0RVZneImaq7zN3u_wARIjNEVWZ3iJmqu8zd7v8:"+signalfixture.BobACI+":"+ms(signalfixture.TGroup)))
	assert.Empty(reactionsOf("signal:aci:0f0f0f0f-0000-4000-8000-000000000002:" + signalfixture.OwnACI + ":" + ms(signalfixture.TEditOld)))
}

func TestRerunClearsEmptiedBody(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	runImport(t, st, dir, t.TempDir())

	changeFixture(t, dir, `UPDATE message SET body = NULL WHERE _id = 300`)
	runImport(t, st, dir, t.TempDir())

	got := importedMessages(t, st)
	assert.Empty(got["signal:aci:0f0f0f0f-0000-4000-8000-000000000004:"+signalfixture.CarolACI+":"+ms(signalfixture.TCarol)].body)
	assert.Equal(0, countRows(t, st, `
		SELECT COUNT(*) FROM messages m WHERE m.source_message_id = ? AND m.snippet IS NOT NULL`,
		"signal:aci:0f0f0f0f-0000-4000-8000-000000000004:"+signalfixture.CarolACI+":"+ms(signalfixture.TCarol)))
}

func TestImportVerifiesOwnerFromBackupEvidence(t *testing.T) {
	const dropOutgoing = `DELETE FROM message WHERE (type & 31) IN (21, 22, 23, 24)`
	for _, tc := range []struct {
		name, phone, wantError string
		changes                []string
		added                  int64
		checkNoSource          bool
	}{
		{name: "full backup of another number", phone: "+15555550199", wantError: "--phone does not match"},
		{name: "incoming-only backup of another number", phone: "+15555550199", changes: []string{dropOutgoing}, wantError: "--phone does not match", checkNoSource: true},
		{name: "incoming-only backup of the right number", phone: signalfixture.OwnPhone, changes: []string{dropOutgoing}, added: 4},
		{name: "group-only backup", phone: signalfixture.OwnPhone, changes: []string{dropOutgoing, `DELETE FROM message WHERE thread_id <> 11`}, added: 1},
		{name: "owner number not recorded", phone: signalfixture.OwnPhone, changes: []string{`UPDATE recipient SET e164 = NULL WHERE _id = 1`}, wantError: "cannot verify --phone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			dir := t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, tc.changes...)
			opts := DefaultOptions()
			opts.Phone = tc.phone
			st := testutil.NewTestStore(t)
			summary, err := NewImporter(st, nil).Import(context.Background(), path, opts)
			if tc.wantError != "" {
				require.Error(err)
				assert.Contains(err.Error(), tc.wantError)
			} else {
				require.NoError(err)
				assert.Equal(tc.added, summary.MessagesAdded)
			}
			if tc.checkNoSource {
				assert.Equal(0, countRows(t, st, `SELECT COUNT(*) FROM sources WHERE source_type = ?`, SourceType))
			}
		})
	}
}

func TestRerunRecordsUpdatedMessagesOnSyncRun(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	runImport(t, st, dir, t.TempDir())
	changeFixture(t, dir, `UPDATE message SET body = 'Carol again' WHERE _id = 300`)
	summary := runImport(t, st, dir, t.TempDir())

	var processed, added, updated int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT messages_processed, messages_added, messages_updated FROM sync_runs
		WHERE source_id = ? ORDER BY id DESC LIMIT 1`), summary.SourceID).Scan(&processed, &added, &updated))
	assert.Equal(summary.MessagesProcessed, processed)
	assert.Equal(int64(0), added)
	assert.Equal(int64(6), updated)
	assert.Equal(summary.MessagesUpdated, updated)
}

func TestSendStateFollowsSignal(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	failed := 24 | pushMessageBit | secureMessageBit
	changeFixture(t, dir, `UPDATE message SET type = `+ms(failed)+` WHERE _id = 101`)
	runImport(t, st, dir, t.TempDir())

	isSent := func() bool {
		var sent bool
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT is_sent FROM messages WHERE source_message_id = ?`),
			"signal:aci:0f0f0f0f-0000-4000-8000-000000000002:"+signalfixture.OwnACI+":"+ms(signalfixture.TReply)).Scan(&sent))
		return sent
	}
	assert.False(isSent(), "a failed send did not leave the phone")

	changeFixture(t, dir, `UPDATE message SET type = `+ms(signalfixture.TypeOutgoing)+` WHERE _id = 101`)
	runImport(t, st, dir, t.TempDir())
	assert.True(isSent(), "a later backup shows it sent")
}

func TestRerunLongTextRemovalKeepsMediaOccurrence(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir := testutil.NewTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type, display_order) VALUES (503, 200, 'text/x-signal-plain', -1)`)
	require.NoError(os.WriteFile(filepath.Join(dir, "Attachment_503_-1.bin"), []byte("Original long text"), 0o600))
	storage := t.TempDir()
	runImport(t, st, dir, storage)
	changeFixture(t, dir, `DELETE FROM attachment WHERE _id = 503`)
	summary := runImport(t, st, dir, storage)
	assert.Zero(summary.AttachmentsStored)
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
	var count int
	require.NoError(st.DB().QueryRow(`SELECT attachment_count FROM messages WHERE has_attachments = TRUE`).Scan(&count))
	assert.Equal(1, count)
}

func TestRerunAttachmentOrderKeepsOccurrences(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir := testutil.NewTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	storage := t.TempDir()
	runImport(t, st, dir, storage)
	changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type, file_name, display_order) VALUES (503, 200, 'image/png', 'other.png', -1)`)
	require.NoError(os.WriteFile(filepath.Join(dir, "Attachment_503_-1.bin"), []byte("other photo"), 0o600))
	summary := runImport(t, st, dir, storage)
	assert.Equal(int64(1), summary.AttachmentsStored)
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM attachments WHERE filename = 'other.png'`))
	assert.Equal(2, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
	changeFixture(t, dir, `UPDATE attachment SET display_order = 1 WHERE _id = 503`)
	summary = runImport(t, st, dir, storage)
	assert.Zero(summary.AttachmentsStored)
	assert.Equal(2, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
	// Identical files in two positions still represent two occurrences.
	changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type, display_order) VALUES (504, 200, 'image/jpeg', -2)`)
	require.NoError(os.WriteFile(filepath.Join(dir, "Attachment_504_-1.bin"), []byte("photo bytes"), 0o600))
	summary = runImport(t, st, dir, storage)
	assert.Equal(int64(1), summary.AttachmentsStored)
	assert.Equal(3, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
	assert.Equal(2, countRows(t, st, `SELECT COUNT(*) FROM attachments WHERE content_hash = '2a6927613ddc903379f3e02145f22074ccf048c7753cfa1ed8ee26c746682997'`))
	summary = runImport(t, st, dir, storage)
	assert.Zero(summary.AttachmentsStored)
	assert.Equal(3, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
}

func TestUnresolvedRepliesCountOnlyThisRun(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE message SET quote_id = 1700000999999, quote_author = 2 WHERE _id = 300`)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, int64(1), summary.RepliesUnresolved)
	changeFixture(t, dir, `DELETE FROM message WHERE _id = 300`)
	summary = runImport(t, st, dir, t.TempDir())
	assert.Zero(t, summary.RepliesUnresolved)
}
