package signalandroid

import (
	"context"
	"database/sql"
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
	importWith := func(t *testing.T, phone string, stmts ...string) (*ImportSummary, *store.Store, error) {
		t.Helper()
		dir := t.TempDir()
		signalfixture.Write(t, dir, signalfixture.Current, 0)
		changeFixture(t, dir, stmts...)
		opts := DefaultOptions()
		opts.Phone = phone
		st := testutil.NewTestStore(t)
		summary, err := NewImporter(st, nil).Import(context.Background(), filepath.Join(dir, "database.sqlite"), opts)
		return summary, st, err
	}
	const dropOutgoing = `DELETE FROM message WHERE (type & 31) IN (21, 22, 23, 24)`

	t.Run("incoming-only backup of another number", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		_, st, err := importWith(t, "+15555550199", dropOutgoing)
		require.Error(err)
		assert.Contains(err.Error(), "--phone does not match")
		assert.Equal(0, countRows(t, st, `SELECT COUNT(*) FROM sources WHERE source_type = ?`, SourceType))
	})
	t.Run("incoming-only backup of the right number", func(t *testing.T) {
		summary, _, err := importWith(t, signalfixture.OwnPhone, dropOutgoing)
		require.NoError(t, err)
		assert.Equal(t, int64(4), summary.MessagesAdded)
	})
	t.Run("group-only backup", func(t *testing.T) {
		summary, _, err := importWith(t, signalfixture.OwnPhone, dropOutgoing, `DELETE FROM message WHERE thread_id <> 11`)
		require.NoError(t, err)
		assert.Equal(t, int64(1), summary.MessagesAdded)
	})
	t.Run("owner number not recorded", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		_, _, err := importWith(t, signalfixture.OwnPhone, `UPDATE recipient SET e164 = NULL WHERE _id = 1`)
		require.Error(err)
		assert.Contains(err.Error(), "cannot verify --phone")
	})
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
