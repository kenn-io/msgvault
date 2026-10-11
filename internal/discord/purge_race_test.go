package discord

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// A purge that lands after an import reads its resume state must not let the
// import checkpoint the purged channel's old cursor again.
func TestPurgeCannotInterleaveWithLoadedDiscordResumeState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	api := newImporterFakeAPI(importerTestChannel("300", "selected"))
	messageID := importerTestSnowflake(t, time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC), 1)
	api.messages["300"] = []Message{importerTestMessage(messageID, "300", "retained astronomy history")}
	imp := newTestImporter(st, api)
	opts := ImportOptions{GuildID: "200"}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	source, err := st.GetSourceByIdentifier("200")
	require.NoError(err)

	var racedPurge error
	imp.resumeLoaded = func(sourceID int64) {
		racedPurge = st.PurgeChannelContext(t.Context(), sourceID, "300")
	}
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	imp.resumeLoaded = nil
	require.ErrorIs(racedPurge, store.ErrSyncAlreadyActive, "purge must not interleave with a running import")
	require.NoError(st.PurgeChannelContext(t.Context(), source.ID, "300"))

	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT count(*) FROM messages WHERE source_id=? AND source_message_id=?`), source.ID, messageID).Scan(&count))
	assert.Equal(1, count, "a later import must restore purged history")
}
