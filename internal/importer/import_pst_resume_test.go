package importer

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func syntheticPSTAbortBody(t *testing.T, st *store.Store) func() {
	t.Helper()
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION synthetic_pst_abort_body() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic PST body failure'; END $$`)
		Require.NoError(t, err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_pst_abort_body BEFORE INSERT ON message_bodies FOR EACH ROW EXECUTE FUNCTION synthetic_pst_abort_body()`)
		Require.NoError(t, err)
		return func() {
			_, err := st.DB().Exec(`DROP TRIGGER synthetic_pst_abort_body ON message_bodies`)
			Require.NoError(t, err)
		}
	}
	_, err := st.DB().Exec(`CREATE TRIGGER synthetic_pst_abort_body BEFORE INSERT ON message_bodies BEGIN SELECT RAISE(ABORT,'synthetic PST body failure'); END`)
	Require.NoError(t, err)
	return func() { _, err := st.DB().Exec(`DROP TRIGGER synthetic_pst_abort_body`); Require.NoError(t, err) }
}

func TestNativePSTResumeRetainsLogicalRunAndFrozenChoice(t *testing.T) {
	for _, checkpointFailure := range []bool{false, true} {
		name := "with_checkpoint"
		if checkpointFailure {
			name = "without_checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			configureNativePSTImports(t, st, true, true)
			dropBody := syntheticPSTAbortBody(t, st)
			dropCheckpoint := func() {}
			if checkpointFailure {
				if st.IsPostgreSQL() {
					_, err := st.DB().Exec(`CREATE FUNCTION synthetic_pst_abort_checkpoint() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic PST checkpoint failure'; END $$`)
					require.NoError(err)
					_, err = st.DB().Exec(`CREATE TRIGGER synthetic_pst_abort_checkpoint BEFORE UPDATE OF cursor_before ON sync_runs FOR EACH ROW EXECUTE FUNCTION synthetic_pst_abort_checkpoint()`)
					require.NoError(err)
					dropCheckpoint = func() {
						_, err := st.DB().Exec(`DROP TRIGGER synthetic_pst_abort_checkpoint ON sync_runs`)
						Require.NoError(t, err)
					}
				} else {
					_, err := st.DB().Exec(`CREATE TRIGGER synthetic_pst_abort_checkpoint BEFORE UPDATE OF cursor_before ON sync_runs BEGIN SELECT RAISE(ABORT,'synthetic PST checkpoint failure'); END`)
					require.NoError(err)
					dropCheckpoint = func() { _, err := st.DB().Exec(`DROP TRIGGER synthetic_pst_abort_checkpoint`); Require.NoError(t, err) }
				}
			}
			path := nativeSyntheticPST(t)
			opts := PstImportOptions{Identifier: "owner@example.net", NotifyImports: true}
			first, err := ImportPst(t.Context(), st, path, opts)
			require.NoError(err)
			require.NotNil(first)
			require.True(first.HardErrors)
			require.NotEmpty(first.ImportRunID)
			for _, table := range []string{"messages", "message_bodies", "message_raw", "message_recipients", "conversation_participants", "import_message_decisions", "mcp_event_log"} {
				var n int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
				assert.Zero(n, "failed canonical write must not leave "+table)
			}
			dropBody()
			dropCheckpoint()
			changed := opts
			changed.NotifyImports = false
			_, err = ImportPst(t.Context(), st, path, changed)
			require.Error(err, "resume must not rewrite the durable notification choice")
			recovered, err := ImportPst(t.Context(), st, path, opts)
			require.NoError(err)
			require.NotNil(recovered)
			require.False(recovered.HardErrors)
			assert.True(recovered.WasResumed)
			assert.Equal(first.ImportRunID, recovered.ImportRunID)
			for query, want := range map[string]int{
				`SELECT COUNT(*) FROM import_runs WHERE completed=TRUE AND notify_imports=TRUE`:      1,
				`SELECT COUNT(*) FROM import_run_attempts`:                                           2,
				`SELECT COUNT(*) FROM import_message_decisions`:                                      1,
				`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`: 1,
			} {
				var got int
				require.NoError(st.DB().QueryRow(query).Scan(&got))
				assert.Equal(want, got, query)
			}
		})
	}
}
