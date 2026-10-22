package imazingcsv

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

// Characterize the first allocation consumer's transaction and privacy contract:
// a mandatory body failure consumes no allocation, and a retry can still commit.
func TestImportImazingOccurrenceAllocationRollsBackWithMessage(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	row := []string{"Synthetic Contact", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "Synthetic rollback body", "", ""}
	export := newTestExport(t, [][]string{row})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION synthetic_imazing_body_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic body failure'; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_imazing_body_abort BEFORE INSERT ON message_bodies FOR EACH ROW EXECUTE FUNCTION synthetic_imazing_body_abort()`)
		require.NoError(err)
	} else {
		_, err := st.DB().Exec(`CREATE TRIGGER synthetic_imazing_body_abort BEFORE INSERT ON message_bodies BEGIN SELECT RAISE(ABORT,'synthetic body failure'); END`)
		require.NoError(err)
	}
	_, err := importer.ImportPath(t.Context(), export)
	require.ErrorContains(err, "synthetic body failure")
	for _, table := range []string{"messages", "message_bodies", "import_occurrence_allocations", "import_message_decisions", "mcp_event_log"} {
		var count int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
		assert.Zero(count, "failed message must not consume %s", table)
	}
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_imazing_body_abort ON message_bodies`)
		require.NoError(err)
		_, err = st.DB().Exec(`DROP FUNCTION synthetic_imazing_body_abort()`)
	} else {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_imazing_body_abort`)
	}
	require.NoError(err)
	_, err = importer.ImportPath(t.Context(), export)
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM import_occurrence_allocations").Scan(&count))
	assert.Equal(1, count)
	var evidence string
	require.NoError(st.DB().QueryRow("SELECT evidence_hashes FROM import_occurrence_allocations").Scan(&evidence))
	assert.NotContains(evidence, "Synthetic rollback body")
	assert.NotContains(evidence, "Synthetic Contact")
	assert.NotContains(evidence, "messages.csv")
}
