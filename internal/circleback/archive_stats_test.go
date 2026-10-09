package circleback

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestCirclebackCountsWriteWhenStatsMaintenanceFails pins that a committed
// meeting still counts as added when conversation-stat maintenance fails.
func TestCirclebackCountsWriteWhenStatsMaintenanceFails(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to fail conversation stat maintenance")
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
		transcripts: map[string]json.RawMessage{"42": json.RawMessage(transcript42)},
	}
	imp, st := newTestImporter(t, f)
	_, err := st.DB().Exec(`
		CREATE TRIGGER fail_circleback_stats
		BEFORE UPDATE OF participant_count ON conversations
		BEGIN
			SELECT RAISE(ABORT, 'forced stats failure');
		END
	`)
	require.NoError(err)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})

	require.Error(err)
	require.NotNil(sum)
	assert.EqualValues(1, sum.MeetingsAdded)
	var messages int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(1, messages)
}
