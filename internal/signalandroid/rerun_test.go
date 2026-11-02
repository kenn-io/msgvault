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
)

const (
	bobPhone = "+15555550102"
	carolACI = "0f0f0f0f-0000-4000-8000-000000000004"
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

func TestRerunMovesNumberOnlyContactToItsACI(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	writeSignalFixture(t, dir, layoutCurrent, 0)
	runImport(t, st, dir, t.TempDir())
	carolChat := "signal:e164:" + carolPhone
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM conversations WHERE source_conversation_id = ?`, carolChat))

	// Signal learns Carol's account ID.
	changeFixture(t, dir, `UPDATE recipient SET aci = '`+carolACI+`' WHERE _id = 4`)
	summary := runImport(t, st, dir, t.TempDir())

	assert.Equal(int64(0), summary.MessagesAdded)
	got := importedMessages(t, st)
	assert.Len(got, 6)
	assert.Equal(importedMessage{conversation: "signal:aci:" + carolACI, body: "Carol here"},
		got[carolACI+":"+ms(tCarol)])
	assert.NotContains(got, carolPhone+":"+ms(tCarol))
	assert.Equal(0, countRows(t, st, `SELECT COUNT(*) FROM conversations WHERE source_conversation_id = ?`, carolChat))
	assert.Equal(3, countRows(t, st, `SELECT COUNT(*) FROM conversations WHERE source_id = ?`, summary.SourceID))
}

func TestRerunMergesACIOnlyContactIntoPhoneParticipant(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	writeSignalFixture(t, dir, layoutCurrent, 0)
	runImport(t, st, dir, t.TempDir())
	aciOnly, hasPhone, err := st.ParticipantByIdentifier(ACIIdentifierType, bobACI)
	require.NoError(err)
	require.NotZero(aciOnly)
	require.False(hasPhone)

	// Bob starts sharing his number.
	changeFixture(t, dir, `UPDATE recipient SET e164 = '`+bobPhone+`' WHERE _id = 3`)
	runImport(t, st, dir, t.TempDir())

	bob, hasPhone, err := st.ParticipantByIdentifier(ACIIdentifierType, bobACI)
	require.NoError(err)
	assert.True(hasPhone)
	assert.Equal(0, countRows(t, st, `SELECT COUNT(*) FROM participants WHERE id = ?`, aciOnly),
		"the ACI-only participant is merged, not left behind")
	assert.Equal(3, countRows(t, st, `
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN conversations c ON c.id = cp.conversation_id
		WHERE c.source_conversation_id = ?`, "signal:group:"+groupB64))
	assert.Equal(1, countRows(t, st, `
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN conversations c ON c.id = cp.conversation_id
		WHERE c.source_conversation_id = ? AND cp.participant_id = ?`, "signal:group:"+groupB64, bob))
}

func TestRerunReplacesReactions(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	writeSignalFixture(t, dir, layoutCurrent, 0)
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
	assert.Equal([]string{"\U0001F602"}, reactionsOf(bobACI+":"+ms(tGroup)))
	assert.Empty(reactionsOf(ownACI + ":" + ms(tEditOld)))
}

func TestRerunClearsEmptiedBody(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	writeSignalFixture(t, dir, layoutCurrent, 0)
	runImport(t, st, dir, t.TempDir())

	changeFixture(t, dir, `UPDATE message SET body = NULL WHERE _id = 300`)
	runImport(t, st, dir, t.TempDir())

	got := importedMessages(t, st)
	assert.Empty(got[carolPhone+":"+ms(tCarol)].body)
	assert.Equal(0, countRows(t, st, `
		SELECT COUNT(*) FROM messages m WHERE m.source_message_id = ? AND m.snippet IS NOT NULL`,
		carolPhone+":"+ms(tCarol)))
}

func TestImportVerifiesOwnerFromBackupEvidence(t *testing.T) {
	importWith := func(t *testing.T, phone string, stmts ...string) (*ImportSummary, *store.Store, error) {
		t.Helper()
		dir := t.TempDir()
		writeSignalFixture(t, dir, layoutCurrent, 0)
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
		summary, _, err := importWith(t, ownPhone, dropOutgoing)
		require.NoError(t, err)
		assert.Equal(t, int64(4), summary.MessagesAdded)
	})
	t.Run("owner cannot be determined", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		_, _, err := importWith(t, ownPhone, dropOutgoing, `DELETE FROM message WHERE thread_id <> 11`)
		require.Error(err)
		assert.Contains(err.Error(), "cannot determine which account")
	})
	t.Run("owner number not recorded", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		_, _, err := importWith(t, ownPhone, `UPDATE recipient SET e164 = NULL WHERE _id = 1`)
		require.Error(err)
		assert.Contains(err.Error(), "cannot verify --phone")
	})
}
