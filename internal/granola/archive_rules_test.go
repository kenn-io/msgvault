package granola

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func archiveStrings(t *testing.T, st *store.Store, query string) []string {
	t.Helper()
	rows, err := st.DB().Query(query)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var values []string
	for rows.Next() {
		var value string
		require.NoError(t, rows.Scan(&value))
		values = append(values, value)
	}
	require.NoError(t, rows.Err())
	return values
}

// TestGranolaCountsWriteWhenStatsMaintenanceFails pins that a committed note
// still counts as added when conversation-stat maintenance fails afterwards.
func TestGranolaCountsWriteWhenStatsMaintenanceFails(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to fail conversation stat maintenance")
	assert := assert.New(t)
	require := require.New(t)
	api := &fakeAPI{notes: map[string][]byte{"not_Ab12Cd34Ef56Gh": loadFixture(t, "note_full.json")}}
	imp, st := newTestImporter(t, api)
	_, err := st.DB().Exec(`
		CREATE TRIGGER fail_granola_stats
		BEFORE UPDATE OF participant_count ON conversations
		BEGIN
			SELECT RAISE(ABORT, 'forced stats failure');
		END
	`)
	require.NoError(err)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})

	require.Error(err)
	require.NotNil(sum)
	assert.EqualValues(1, sum.NotesAdded)
	var messages int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(1, messages)
}

// TestGranolaAdoptsArchiverRules covers the meetingarchive rules Granola notes
// gain by saving through Upsert: envelope addresses on recipient rows, trimmed
// display names, invalid addresses dropped, and one FTS entry per attendee.
func TestGranolaAdoptsArchiverRules(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw, err := json.Marshal(map[string]any{
		"id":         "not_Rules1",
		"title":      "Rules Check",
		"owner":      map[string]any{"name": " Bob Jones ", "email": "bob@example.com"},
		"created_at": "2026-06-04T10:00:00Z",
		"updated_at": "2026-06-04T10:30:00Z",
		"attendees": []map[string]any{
			{"name": "Carol Diaz", "email": "carol@example.com"},
			{"name": "Carol Diaz", "email": "Carol@Example.com"},
			{"name": "Not An Address", "email": "not-an-email"},
		},
		"summary_text": "Rules summary.",
	})
	require.NoError(err)
	imp, st := newTestImporter(t, &fakeAPI{notes: map[string][]byte{"not_Rules1": raw}})

	_, err = imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com", AccountEmail: "alice@example.com"})
	require.NoError(err)

	assert.Equal([]string{"from:bob@example.com:Bob Jones", "to:carol@example.com:Carol Diaz"},
		archiveStrings(t, st, `
			SELECT mr.recipient_type || ':' || p.email_address || ':' || COALESCE(mr.display_name, '')
			FROM message_recipients mr
			JOIN participants p ON p.id = mr.participant_id
			ORDER BY 1`))
	assert.Equal([]string{"from:bob@example.com:bob@example.com", "to:carol@example.com:carol@example.com"}, archiveStrings(t, st, `
		SELECT mr.recipient_type || ':' || p.email_address || ':' || COALESCE(mr.email_address, '')
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		ORDER BY 1`))
	var invalid int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM participants WHERE email_address = ?`), "not-an-email").Scan(&invalid))
	assert.Zero(invalid, "a value without @ must not become a participant")
	if st.FTS5Available() && !st.IsPostgreSQL() {
		var to string
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT f.to_addr FROM messages_fts f
			JOIN messages m ON m.id = f.rowid
			WHERE m.source_message_id = ?`), "not_Rules1").Scan(&to))
		assert.Equal("carol@example.com", to)
	}
}
