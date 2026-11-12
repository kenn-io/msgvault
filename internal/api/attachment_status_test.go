package api

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestHydrateSyncRunStatusSettledFileObligations(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "status@example.org")
	require.NoError(err)
	runID, err := st.StartSync(source.ID, "full")
	require.NoError(err)
	record := func(run int64, id, phase, status, kind, message string) {
		require.NoError(st.RecordSyncRunItem(store.SyncRunItem{SyncRunID: run, SourceMessageID: id, Phase: phase, Status: status, ErrorKind: kind, ErrorMessage: message}))
	}
	record(runID, "real-fetch", "fetch", store.SyncRunItemStatusError, "fetch_error", "provider unavailable")
	record(runID, "restored", "ingest", store.SyncRunItemStatusError, "attachment_error", store.SyncRunItemFilesPendingMessage)
	record(runID, "restored", "ingest", store.SyncRunItemStatusError, "attachment_error", "save attachments: storage unavailable")
	record(runID, "restored", "fetch", store.SyncRunItemStatusResolved, "acquired", "acquired")
	kinds := []string{"attachment_error", "remote_image_error", "attachment_remote_image_error"}
	for i := range sourceStatusItemErrorLimit + 2 {
		id := fmt.Sprintf("saved-%d", i)
		record(runID, id, "ingest", store.SyncRunItemStatusError, kinds[i%len(kinds)], store.SyncRunItemFilesPendingMessage)
		record(runID, id, "fetch", store.SyncRunItemStatusResolved, "acquired", "acquired")
	}
	record(runID, "active", "ingest", store.SyncRunItemStatusError, "attachment_remote_image_error", store.SyncRunItemFilesPendingMessage)
	// A matching provider ID in another source must not acknowledge this work.
	other, err := st.GetOrCreateSource("gmail", "other-status@example.org")
	require.NoError(err)
	otherRun, err := st.StartSync(other.ID, "full")
	require.NoError(err)
	record(otherRun, "active", "fetch", store.SyncRunItemStatusResolved, "acquired", "acquired")
	require.NoError(st.CompleteSync(otherRun, ""))
	require.NoError(st.UpdateSyncCheckpoint(runID, &store.Checkpoint{ErrorsCount: 2}))
	require.NoError(st.FailSync(runID, "interrupted after committing RAW"))
	run, err := st.GetLatestSyncContext(t.Context(), source.ID, 0)
	require.NoError(err)
	hydrate := func() *SyncRunStatus {
		status := syncRunStatus(run)
		require.NoError((&Server{}).hydrateSyncRunStatus(t.Context(), st, status))
		return status
	}
	rawBefore, err := st.CountSyncRunItems(runID, "")
	require.NoError(err)
	rawItemsBefore, err := st.ListSyncRunItems(runID, "", int(rawBefore))
	require.NoError(err)
	status := hydrate()
	assert.Equal(int64(2), status.ErrorsCount, "synthetic obligations must not inflate actual error totals")
	ids := make([]string, 0, len(status.ItemErrors))
	for _, item := range status.ItemErrors {
		ids = append(ids, item.SourceMessageID)
	}
	assert.ElementsMatch([]string{"active", "real-fetch", "restored"}, ids, "settled markers must be excluded before the display limit")
	debt, err := st.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	replayIDs := make([]string, 0, len(debt))
	for _, item := range debt {
		replayIDs = append(replayIDs, item.SourceMessageID)
	}
	assert.Contains(replayIDs, "active", "pending work remains replay-selectable")
	assert.NotContains(replayIDs, "saved-0")
	// A later attempt can finish the files even if that attempt then stops.
	retryID, err := st.StartSync(source.ID, "incremental")
	require.NoError(err)
	record(retryID, "active", "fetch", store.SyncRunItemStatusResolved, "acquired", "acquired")
	require.NoError(st.FailSync(retryID, "synthetic later interruption"))
	status = hydrate()
	ids = ids[:0]
	for _, item := range status.ItemErrors {
		ids = append(ids, item.SourceMessageID)
	}
	assert.ElementsMatch([]string{"real-fetch", "restored"}, ids, "rehydration must see the later settlement and retain real historical failures")
	assert.Equal(status.ItemErrors, hydrate().ItemErrors, "projection is repeatable")
	rawAfter, err := st.CountSyncRunItems(runID, "")
	require.NoError(err)
	assert.Equal(rawBefore, rawAfter, "hydration must not remove or rewrite durable records")
	rawItems, err := st.ListSyncRunItems(runID, "", int(rawBefore))
	require.NoError(err)
	assert.Equal(rawItemsBefore, rawItems, "durable outcomes and their diagnostics remain unchanged")
	debt, err = st.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	for _, item := range debt {
		assert.NotEqual("active", item.SourceMessageID)
	}
}
