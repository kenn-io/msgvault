package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// A purge that lands after an import reads its resume state must not let the
// import checkpoint the purged channel's old coverage again.
func TestPurgeCannotInterleaveWithLoadedSlackResumeState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := testWorkspace(t)
	imp, opts := testImporter(t, f)
	opts.ChannelIDs = []string{"C01"}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	source, err := imp.store.GetSourceByIdentifier("T01:UME")
	require.NoError(err)

	var racedPurge error
	imp.resumeLoaded = func(sourceID int64) {
		racedPurge = imp.store.PurgeChannelContext(t.Context(), sourceID, "C01")
	}
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	imp.resumeLoaded = nil
	require.ErrorIs(racedPurge, store.ErrSyncAlreadyActive, "purge must not interleave with a running import")
	require.NoError(imp.store.PurgeChannelContext(t.Context(), source.ID, "C01"))

	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(imp.store.DB().QueryRow(imp.store.Rebind(
		`SELECT count(*) FROM messages WHERE source_id=? AND source_message_id=?`), source.ID, "C01:"+ts(0)).Scan(&count))
	assert.Equal(1, count, "a later import must restore purged history")
}
