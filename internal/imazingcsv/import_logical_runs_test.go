package imazingcsv

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportImazingRecordsFreshLogicalRuns(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	row := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "sender@example.net", "Synthetic Sender", "", "", "", "Synthetic logical import", "", ""}
	export := newTestExport(t, [][]string{row})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})
	for range 2 {
		_, err := importer.ImportPath(t.Context(), export)
		require.NoError(err)
	}
	rows, err := st.DB().Query(`SELECT r.id,r.import_format,r.notify_imports,r.completed,s.status FROM import_runs r JOIN import_run_attempts a ON a.import_run_id=r.id JOIN sync_runs s ON s.id=a.sync_run_id ORDER BY s.id`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var ids []string
	for rows.Next() {
		var id, format, status string
		var notify, completed bool
		require.NoError(rows.Scan(&id, &format, &notify, &completed, &status))
		assert.NotEmpty(id)
		assert.Equal(SourceType, format)
		assert.False(notify)
		assert.True(completed)
		assert.Equal("completed", status)
		ids = append(ids, id)
	}
	require.NoError(rows.Err())
	require.Len(ids, 2, "each non-resumable native invocation has its own durable logical run")
	assert.NotEqual(ids[0], ids[1])
	var messages, events, decisions int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages))
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM mcp_event_log").Scan(&events))
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM import_message_decisions").Scan(&decisions))
	assert.Equal(1, messages)
	assert.Equal(1, decisions)
	assert.Zero(events, "default-off native runs stay silent across fresh retries")
}

func TestImportImazingFailedLogicalRunIsNotRecovered(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	row := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "Synthetic retry body", "", ""}
	export := newTestExport(t, [][]string{row})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_imazing_run_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic native body failure'; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imazing_run_abort BEFORE INSERT ON message_bodies FOR EACH ROW EXECUTE FUNCTION synthetic_imazing_run_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imazing_run_abort BEFORE INSERT ON message_bodies BEGIN SELECT RAISE(ABORT,'synthetic native body failure'); END`)
	}
	require.NoError(err)
	_, err = importer.ImportPath(t.Context(), export)
	require.ErrorContains(err, "synthetic native body failure")
	var failedID, status string
	var completed bool
	err = st.DB().QueryRow(`SELECT r.id,r.completed,s.status FROM import_runs r JOIN import_run_attempts a ON a.import_run_id=r.id JOIN sync_runs s ON s.id=a.sync_run_id`).Scan(&failedID, &completed, &status)
	require.NoError(err, "a failed native attempt keeps its logical policy without becoming resumable")
	assert.False(completed)
	assert.Equal("failed", status)
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_imazing_run_abort ON message_bodies`)
		require.NoError(err)
		_, err = st.DB().Exec(`DROP FUNCTION synthetic_imazing_run_abort()`)
	} else {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_imazing_run_abort`)
	}
	require.NoError(err)
	_, err = importer.ImportPath(t.Context(), export)
	require.NoError(err)
	var successID string
	require.NoError(st.DB().QueryRow(`SELECT r.id FROM import_runs r JOIN import_run_attempts a ON a.import_run_id=r.id JOIN sync_runs s ON s.id=a.sync_run_id WHERE s.status='completed'`).Scan(&successID))
	assert.NotEqual(failedID, successID)
	var runs int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM import_runs").Scan(&runs))
	assert.Equal(2, runs)
}
