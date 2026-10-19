package meetingimport

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMeetingImportBindingFailureFailsSyncWithoutPanic(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	failImportRunBindingAfterAttempt(t, st)
	importer := NewImporter(st, Hooks{})
	var importErr error
	panicValue := recoverImportPanic(func() {
		_, importErr = importer.Import(t.Context(), validImportRequest(t))
	})

	assert.Nil(panicValue, "meeting import should return the binding error without panicking")
	assert.ErrorContains(importErr, "bind meeting import: invalid_import_run")
	var status string
	require.NoError(st.DB().QueryRow(`SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
	assert.Equal(store.SyncStatusFailed, status)
}

func failImportRunBindingAfterAttempt(t *testing.T, st *store.Store) {
	t.Helper()
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION fail_import_run_binding() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN UPDATE import_runs SET completed=TRUE WHERE id=NEW.import_run_id; RETURN NEW; END $$`)
		Require.NoError(t, err)
		_, err = st.DB().Exec(`CREATE TRIGGER fail_import_run_binding AFTER INSERT ON import_run_attempts
			FOR EACH ROW EXECUTE FUNCTION fail_import_run_binding()`)
		Require.NoError(t, err)
		return
	}
	_, err := st.DB().Exec(`CREATE TRIGGER fail_import_run_binding AFTER INSERT ON import_run_attempts
		BEGIN UPDATE import_runs SET completed=TRUE WHERE id=NEW.import_run_id; END`)
	Require.NoError(t, err)
}

func recoverImportPanic(run func()) (panicValue any) {
	defer func() { panicValue = recover() }()
	run()
	return nil
}

func TestNativeMeetingImportOwnsFreshSilentLogicalRuns(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	caller := st.WithIngestContext(store.IngestContext{Mode: store.IngestLive})
	native := NewImporter(caller, Hooks{})
	var prior Result
	for range 2 {
		result, err := native.Import(t.Context(), validImportRequest(t))
		require.NoError(err)
		if prior.MessageID != 0 {
			assert.Equal(prior.MessageID, result.MessageID)
		}
		prior = result
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM import_runs WHERE import_format='meeting_import' AND completed=TRUE AND notify_imports=FALSE`: 2,
		`SELECT COUNT(*) FROM sync_runs WHERE status='completed'`:                                                           2,
		`SELECT COUNT(*) FROM messages`:                 1,
		`SELECT COUNT(*) FROM import_message_decisions`: 1,
		`SELECT COUNT(*) FROM mcp_event_log`:            0,
	} {
		var got int
		require.NoError(st.DB().QueryRow(query).Scan(&got))
		assert.Equal(want, got, query)
	}
}
