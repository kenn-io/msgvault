package granola

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

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
