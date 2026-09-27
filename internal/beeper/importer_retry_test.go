package beeper

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportRetriesTransientMessageListFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	oldBackoff := fetchRetryBackoff
	fetchRetryBackoff = []time.Duration{0, 0}
	t.Cleanup(func() { fetchRetryBackoff = oldBackoff })

	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	f := newFakeBeeper(t)
	f.addChat(budgetTestChat("!flaky:beeper.local", 5, base))
	f.failMessageListTimes("!flaky:beeper.local", 2)
	imp, _, done := newTestImporter(t, f)
	defer done()

	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err, "two transient failures are absorbed by retries")
	assert.Zero(sum.FetchErrors)
	assert.Equal(5, countBeeperMessages(t, imp))
}

func TestImportGivesUpAfterRetries(t *testing.T) {
	require := require.New(t)
	oldBackoff := fetchRetryBackoff
	fetchRetryBackoff = []time.Duration{0, 0}
	t.Cleanup(func() { fetchRetryBackoff = oldBackoff })

	base := time.Now().Add(-30 * 24 * time.Hour).UTC().Truncate(time.Second)
	f := newFakeBeeper(t)
	f.addChat(budgetTestChat("!flaky:beeper.local", 5, base))
	f.failMessageListTimes("!flaky:beeper.local", 3)
	imp, _, done := newTestImporter(t, f)
	defer done()

	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	var partial *PartialSyncError
	require.ErrorAs(err, &partial)
	assert.EqualValues(t, 1, sum.FetchErrors)
}

func TestTailProbeFailureRemainsDueAndCountsAsFetchError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	oldBackoff := fetchRetryBackoff
	fetchRetryBackoff = []time.Duration{0, 0}
	t.Cleanup(func() { fetchRetryBackoff = oldBackoff })

	f := newFakeBeeper(t)
	chatID := "!tail-retry:beeper.local"
	f.addChat(budgetTestChat(chatID, 2, time.Now().Add(-60*24*time.Hour)))
	imp, st, done := newTestImporter(t, f)
	defer done()
	_, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	src, err := st.GetOrCreateSource(sourceTypeBeeper, "signal")
	require.NoError(err)
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	previousScan := formatWatermark(time.Now().Add(-25 * time.Hour))
	state.LastTailScan = previousScan
	blob, err := state.Marshal()
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE sync_runs SET cursor_after = ? WHERE id = ?`), blob, run.ID)
	require.NoError(err)

	f.failMessageListTimes(chatID, len(fetchRetryBackoff)+1)
	sum, err := imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	var partial *PartialSyncError
	require.ErrorAs(err, &partial)
	assert.EqualValues(1, sum.FetchErrors)
	run, err = st.GetLastSuccessfulSync(src.ID)
	require.NoError(err)
	state, err = LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Equal(previousScan, state.LastTailScan, "a failed probe leaves the scan due")
	assert.NotEqual(state.TailScanStarted, state.Chats[chatID].TailProbed,
		"a failed probe is not marked complete for this scan")

	f.resetRequests()
	_, err = imp.Import(context.Background(), ImportOptions{AccountID: "signal"})
	require.NoError(err)
	assert.True(slices.ContainsFunc(f.requests(), func(req string) bool {
		return strings.Contains(req, "/v1/chats/"+chatID+"/messages") && strings.Contains(req, "direction=before")
	}), "the failed tail probe is retried on the next scheduled run")
}

func TestListMessagesPageDoesNotRetryPermanentErrors(t *testing.T) {
	f := newFakeBeeper(t)
	chat := budgetTestChat("!bad-request:beeper.local", 2, time.Now().Add(-48*time.Hour))
	f.addChat(chat)
	f.setMessageListFailure(chat.ID, true)
	imp, _, done := newTestImporter(t, f)
	defer done()
	_, err := imp.listMessagesPage(t.Context(), ImportOptions{}, chat.ID, "", "")
	require.ErrorContains(t, err, "status 400")
	requests := 0
	for _, request := range f.requests() {
		if strings.HasSuffix(request, "/messages") {
			requests++
		}
	}
	assert.Equal(t, 1, requests, "a rejected page is not retried within the same run")
}
