package store_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/stacktest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAcquisitionDiagnosticClassificationAndPaging(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	first := f.StartSync()
	record := func(run int64, id, phase, kind, message, status string) {
		t.Helper()
		require.NoError(f.Store.RecordSyncRunItem(store.SyncRunItem{
			SyncRunID: run, SourceMessageID: id, Phase: phase,
			ErrorKind: kind, ErrorMessage: message, Status: status,
		}))
	}
	for _, id := range []string{"resolved-file", "active-file", "active-synthetic"} {
		record(first, id, "ingest", "attachment_error", store.SyncRunItemFilesPendingMessage, "error")
	}
	for _, id := range []string{"active-real", "old-generation", "gone", "resolved-real"} {
		record(first, id, "fetch", "fetch_error", "request failed for hidden@example.com https://example.com/private", "error")
	}
	require.NoError(f.Store.FailSync(first, "interrupted"))
	boundary, err := f.Store.StartSync(f.Source.ID, "incremental")
	require.NoError(err)
	record(boundary, "resolved-file", "fetch", "acquired", "acquired", "resolved")
	record(boundary, "resolved-real", "fetch", "acquired", "acquired", "resolved")
	record(boundary, "active-file", "ingest", "attachment_remote_image_error", "local save failed", "error")
	record(boundary, "active-synthetic", "ingest", "remote_image_error", store.SyncRunItemFilesPendingMessage, "error")
	record(boundary, "active-real", "fetch", "fetch_error", "retry failed for hidden@example.com https://example.com/private", "error")
	// A non-acquisition outcome for the same message cannot erase file work.
	record(boundary, "active-file", "delete", "delete_error", "unrelated failure", "error")
	record(boundary, "gone", "fetch", "gmail_not_found", "not found", "skipped")
	record(boundary, "unknown-kind", "ingest", "future_kind", "unknown operation", "error")
	for i := range 40 {
		id := fmt.Sprintf("settled-%03d", i)
		record(boundary, id, "ingest", "attachment_error", store.SyncRunItemFilesPendingMessage, "error")
		record(boundary, id, "fetch", "acquired", "acquired", "resolved")
	}
	require.NoError(f.Store.CompleteSync(boundary, "100"))
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err)
	otherRun, err := f.Store.StartSync(other.ID, "full")
	require.NoError(err)
	record(otherRun, "active-synthetic", "fetch", "acquired", "acquired", "resolved")
	record(otherRun, "other-only", "fetch", "fetch_error", "other failure", "error")
	projected, err := f.Store.ListSyncRunItemsContext(ctx, boundary, "error", 3)
	require.NoError(err)
	require.Len(projected, 3, "settled synthetic markers must not consume the run-error limit")
	assert.Equal([]string{"unknown-kind", "active-file", "active-real"}, []string{
		projected[0].SourceMessageID, projected[1].SourceMessageID, projected[2].SourceMessageID,
	})

	opts := store.AcquisitionDiagnosticOptions{SourceID: f.Source.ID, Limit: 1}
	var ids []string
	for {
		report, err := f.Store.DiagnoseAcquisitionContext(ctx, opts)
		require.NoError(err)
		assert.Equal(boundary, report.IncrementalBoundaryID)
		assert.Equal(int64(3), report.Summary.ActiveObligations)
		assert.Equal(int64(42), report.Summary.ResolvedObligations)
		assert.Equal(int64(2), report.Summary.ActiveErrors)
		assert.Equal(int64(2), report.Summary.HistoricalErrors)
		assert.Equal(int64(1), report.Summary.Historical)
		assert.Equal(int64(3), report.Summary.Indeterminate)
		assert.Equal(int64(2), report.Summary.BlockedRemoteImages)
		require.Len(report.Items, 1)
		ids = append(ids, report.Items[0].SourceMessageID)
		if report.Items[0].Operation == "files" {
			assert.Contains(report.Items[0].BlockedBy, "remote_image_archiving_disabled")
		}
		if report.Items[0].SourceMessageID == "active-file" {
			assert.Equal("delete", report.Items[0].LatestPhase)
			assert.Equal("error", report.Items[0].LatestStatus)
			assert.Greater(report.Items[0].LatestOutcomeID, report.Items[0].OutcomeID)
		}
		encoded, err := json.Marshal(report)
		require.NoError(err)
		assert.NotContains(string(encoded), "hidden@example.com")
		assert.NotContains(string(encoded), "https://example.com/private")
		if !report.HasMore {
			break
		}
		require.Greater(report.NextAfterID, opts.AfterID)
		opts.AfterID = report.NextAfterID
	}
	assert.ElementsMatch([]string{"active-file", "active-synthetic", "active-real"}, ids)
	// The same identities and representative outcomes are used by replay.
	active, err := f.Store.ListUnresolvedSyncItemErrors(ctx, f.Source.ID)
	require.NoError(err)
	var replayIDs []string
	for _, item := range active {
		replayIDs = append(replayIDs, item.SourceMessageID)
	}
	assert.Contains(replayIDs, "unknown-kind", "unknown replay kinds remain visible as indeterminate")
	for _, id := range ids {
		assert.Contains(replayIDs, id)
	}
	for state, want := range map[string]int{"resolved": 42, "historical": 1, "indeterminate": 3} {
		opts.State, opts.AfterID, opts.Limit = state, 0, 100
		report, err := f.Store.DiagnoseAcquisitionContext(ctx, opts)
		require.NoError(err)
		assert.Len(report.Items, want)
		if state == "resolved" {
			assert.Contains(report.String(), "Details: resolved work\n")
			assert.Contains(report.String(), `latest="resolution recorded"`)
			assert.NotContains(report.String(), "fetch/resolved")
			for _, item := range report.Items {
				assert.Greater(item.ResolvedByID, item.OutcomeID)
				assert.Equal("resolved", item.LatestStatus)
			}
		}
	}
	opts = store.AcquisitionDiagnosticOptions{SourceID: other.ID, Limit: 10, RemoteImagesEnabled: true}
	report, err := f.Store.DiagnoseAcquisitionContext(ctx, opts)
	require.NoError(err)
	require.Len(report.Items, 1)
	assert.Equal("other-only", report.Items[0].SourceMessageID)
	assert.Zero(report.Summary.ResolvedObligations)
}

func TestAcquisitionDiagnosticReopenedOperations(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	run := f.StartSync()
	// A new attempt does not turn the previous, explicitly settled episode's
	// failures back into active errors. A skip is historical, not acquisition.
	for _, item := range []store.SyncRunItem{
		{SourceMessageID: "files", Phase: "ingest", Status: "error", ErrorKind: "attachment_error", ErrorMessage: "save failed"},
		{SourceMessageID: "files", Phase: "fetch", Status: "resolved", ErrorKind: "acquired"},
		{SourceMessageID: "files", Phase: "ingest", Status: "error", ErrorKind: "remote_image_error", ErrorMessage: store.SyncRunItemFilesPendingMessage},
		{SourceMessageID: "fetch", Phase: "fetch", Status: "error", ErrorKind: "fetch_error"},
		{SourceMessageID: "fetch", Phase: "fetch", Status: "skipped", ErrorKind: "gmail_not_found"},
		{SourceMessageID: "fetch", Phase: "fetch", Status: "error", ErrorKind: "fetch_error"},
	} {
		item.SyncRunID = run
		require.NoError(f.Store.RecordSyncRunItem(item))
	}
	opts := store.AcquisitionDiagnosticOptions{SourceID: f.Source.ID, Limit: 10, RemoteImagesEnabled: true}
	report, err := f.Store.DiagnoseAcquisitionContext(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(2), report.Summary.ActiveObligations)
	assert.Equal(int64(1), report.Summary.ResolvedObligations)
	assert.Equal(int64(1), report.Summary.ActiveErrors)
	assert.Equal(int64(2), report.Summary.HistoricalErrors)
	assert.Zero(report.Summary.BlockedRemoteImages)
	assert.Zero(report.Summary.Indeterminate)
	for _, item := range report.Items {
		assert.Empty(item.BlockedBy)
	}
	opts.State = "resolved"
	report, err = f.Store.DiagnoseAcquisitionContext(t.Context(), opts)
	require.NoError(err)
	require.Len(report.Items, 1)
	assert.Greater(report.Items[0].ResolvedByID, report.Items[0].OutcomeID)
	assert.Greater(report.Items[0].LatestOutcomeID, report.Items[0].ResolvedByID)
	assert.Equal("error", report.Items[0].LatestStatus)
}

func TestAcquisitionDiagnosticReadOnlyAndErrors(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	run := f.StartSync()
	require.NoError(f.Store.RecordSyncRunItem(store.SyncRunItem{SyncRunID: run,
		SourceMessageID: "pending", Phase: "fetch", Status: "error", ErrorKind: "fetch_error",
		ErrorMessage: "request failed"}))
	f.CreateMessage("pending")
	readonly, err := store.OpenReadOnlyContext(t.Context(), store.DBPathForTest(f.Store))
	require.NoError(err)
	t.Cleanup(func() { _ = readonly.Close() })
	opts := store.AcquisitionDiagnosticOptions{SourceID: f.Source.ID, Limit: 10}
	before, err := f.Store.ListSyncRunItems(run, "", 100)
	require.NoError(err)
	sourceBefore, err := f.Store.GetSourceByID(f.Source.ID)
	require.NoError(err)
	runBefore, err := f.Store.GetLatestSync(f.Source.ID)
	require.NoError(err)
	report, err := readonly.DiagnoseAcquisitionContext(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(1), report.Summary.ActiveObligations)
	assert.Equal(int64(1), report.Summary.ActiveErrors)
	_, err = readonly.DB().ExecContext(t.Context(), `UPDATE sync_run_items SET error_message = 'changed'`)
	require.Error(err, "the diagnostic connection must reject writes")
	after, err := f.Store.ListSyncRunItems(run, "", 100)
	require.NoError(err)
	assert.Equal(before, after)
	sourceAfter, err := f.Store.GetSourceByID(f.Source.ID)
	require.NoError(err)
	assert.Equal(sourceBefore, sourceAfter)
	runAfter, err := f.Store.GetLatestSync(f.Source.ID)
	require.NoError(err)
	assert.Equal(runBefore, runAfter)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	report, err = readonly.DiagnoseAcquisitionContext(cancelled, opts)
	require.ErrorIs(err, context.Canceled)
	assert.Nil(report)
	// Remove the ledger only in this isolated fixture to exercise a real SQL
	// failure, not a mock return value. The report must never become empty.
	_, err = f.Store.DB().ExecContext(t.Context(), `DROP TABLE sync_run_items`)
	require.NoError(err)
	report, err = readonly.DiagnoseAcquisitionContext(t.Context(), opts)
	require.Error(err)
	assert.Nil(report)
}

func TestAcquisitionDiagnosticEmptyAndValidation(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	opts := store.AcquisitionDiagnosticOptions{SourceID: f.Source.ID, Limit: 10}
	report, err := f.Store.DiagnoseAcquisitionContext(t.Context(), opts)
	require.NoError(err)
	assert.Empty(report.Items)
	assert.Contains(report.String(), "No known pending downloads or file-saving work for this source.")
	assert.Contains(report.String(), "does not prove archive completeness")
	assert.Contains(report.String(), "Messages with resolved work: 0")
	assert.NotContains(report.String(), "Messages with pending work: 0")
	assert.NotContains(report.String(), "fetch/fetch_error")
	assert.Contains(report.String(), "Details: pending downloads and file saving\n")
	assert.NotContains(report.String(), "Pages use outcome IDs")
	assert.Contains(report.String(), "Syncing can change results between pages. For consistent results, repeat while no sync is running.")
	encoded, err := json.Marshal(report)
	require.NoError(err)
	assert.Contains(string(encoded), "fetch/fetch_error", "JSON must retain the recognized replay contracts")
	assert.Contains(string(encoded), `"incremental_boundary_id":0`)
	assert.Contains(string(encoded), `"ledger_through_id":0`)
	for _, invalid := range []store.AcquisitionDiagnosticOptions{
		{Limit: 10}, {SourceID: f.Source.ID, Limit: 0}, {SourceID: f.Source.ID, Limit: 1001},
		{SourceID: f.Source.ID, Limit: 10, AfterID: -1}, {SourceID: f.Source.ID, Limit: 10, State: "complete"},
	} {
		report, err := f.Store.DiagnoseAcquisitionContext(t.Context(), invalid)
		require.Error(err)
		assert.Nil(report)
	}
	opts.SourceID = f.Source.ID + 1000
	report, err = f.Store.DiagnoseAcquisitionContext(t.Context(), opts)
	require.ErrorIs(err, store.ErrSourceNotFound)
	assert.Nil(report)
}

func TestAcquisitionDiagnosticHumanOutputPreservesJSON(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	run := f.StartSync()
	require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{
		SyncRunID: run, SourceMessageID: "pending-image", Phase: "ingest", Status: "error",
		ErrorKind: "remote_image_error", ErrorMessage: store.SyncRunItemFilesPendingMessage,
	}))
	require.NoError(f.Store.FailSync(run, "interrupted"))
	target, err := f.Store.GetOrCreateSource("gmail", "merged-target@example.com")
	require.NoError(err)
	// Retained outcomes can outlive source retirement. Seed its metadata
	// without moving the outcomes to the destination source.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO source_settings
		(source_id, history_only, merged_into_source_id) VALUES (?, TRUE, ?)`), f.Source.ID, target.ID)
	require.NoError(err)
	report, err := f.Store.DiagnoseAcquisitionContext(t.Context(), store.AcquisitionDiagnosticOptions{
		SourceID: f.Source.ID, Limit: 10,
	})
	require.NoError(err)
	require.Len(report.Items, 1)
	before, err := json.Marshal(report, json.Deterministic(true))
	require.NoError(err)
	human := report.String()
	assert.Contains(human, "Pending retries since the last completed incremental sync; resolution requires a later recorded confirmation.")
	assert.Contains(human, "File-saving reminders are not errors. One detail per message in the selected state.")
	assert.Contains(human, "Historical includes errors marked as skipped and failed downloads later skipped. Skipping does not prove a message was saved or explain why it was skipped.")
	assert.Contains(human, "Indeterminate includes unresolved failures excluded from retries and unrecognized outcomes.")
	assert.Contains(human, "Blockers concern normal sync; provider availability and permanent impossibility are not tested. Resolution does not verify saved files.")
	assert.Contains(human, "Only saved sync records for this source are examined, without changes. Counts are messages per state; a message can appear in more than one state.")
	assert.Contains(human, "Saved messages and files, attachment completeness, search index updates, the current contents of Gmail, and gaps in older sync records are not checked.")
	for _, technical := range []string{
		"acquisition replay selection", "synthetic file-saving marker", "withdrawn skipped items retaining a recognized error contract",
		"replay boundary", "acquisition contract", "normal replay path", "ledger acknowledgement",
		"Retained sync ledger", "RAW/blob", "MIME occurrence", "FTS freshness", "consistent traversal",
	} {
		assert.NotContains(human, technical)
		assert.Contains(string(before), technical)
	}
	assert.Equal([]string{
		"Retained sync ledger for this source in one read-only snapshot; counts are messages per state and may overlap.",
		"Active obligations use the acquisition replay selection since the latest completed incremental; resolved requires a later explicit resolution.",
		"Recognized contracts: fetch/fetch_error, fetch/batch_fetch_error, ingest/ingest_error, ingest/attachment_error, ingest/remote_image_error, ingest/attachment_remote_image_error.",
		"Errors exclude the shipped synthetic file-saving marker. Details contain at most one outcome per message in the selected state.",
		"Historical includes withdrawn skipped items retaining a recognized error contract and acquisition failures followed by a compatible skip. Withdrawal does not prove acquisition or identify its cause.",
		"Indeterminate includes failures hidden by the replay boundary without resolution and outcomes with no known acquisition contract.",
		"Source blockers describe the normal replay path; provider availability and permanent impossibility are not tested. Resolution is a ledger acknowledgement, not a physical file integrity check.",
		"No physical RAW/blob integrity, MIME occurrence, FTS freshness, remote inventory, or historical omissions check. The absence of known active obligations does not prove archive completeness.",
		"Concurrent sync can change later invocations. Repeat against a quiet archive for consistent traversal.",
	}, report.Coverage, "version 1 JSON coverage must remain unchanged")
	for _, blocker := range []struct{ code, label string }{
		{"source_history_only", "history-only source"},
		{"source_merged", "merged source"},
		{"remote_image_archiving_disabled", "remote image archiving disabled"},
	} {
		assert.Contains(human, blocker.label)
		assert.NotContains(human, blocker.code)
		assert.Contains(string(before), blocker.code)
	}
	after, err := json.Marshal(report, json.Deterministic(true))
	require.NoError(err)
	assert.Equal(before, after, "human rendering must preserve JSON codes and technical coverage")
}

func TestAcquisitionDiagnosticHumanDetails(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	run := f.StartSync()
	cases := []struct {
		id, phase, kind, state, label string
		synthetic                     bool
	}{
		{"download", "fetch", "fetch_error", "active", "download failed", false},
		{"batch", "fetch", "batch_fetch_error", "active", "download failed", false},
		{"message", "ingest", "ingest_error", "active", "message saving failed", false},
		{"attachment", "ingest", "attachment_error", "active", "file saving failed", false},
		{"image", "ingest", "remote_image_error", "active", "file saving failed", false},
		{"combined", "ingest", "attachment_remote_image_error", "active", "file saving failed", false},
		{"reminder", "ingest", "attachment_error", "active", "file-saving reminder", true},
		{"unknown-kind", "fetch", "future_kind", "indeterminate", "unrecognized failure (see --json)", false},
		{"unknown-phase", "future_phase", "attachment_error", "indeterminate", "unrecognized failure (see --json)", false},
	}
	for _, tc := range cases {
		message := "hidden@example.com https://example.com/private"
		if tc.synthetic {
			message = store.SyncRunItemFilesPendingMessage
		}
		require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{
			SyncRunID: run, SourceMessageID: tc.id, Phase: tc.phase, ErrorKind: tc.kind,
			Status: "error", ErrorMessage: message,
		}))
	}
	// A reminder's text does not turn a message-saving error into synthetic work.
	require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{
		SyncRunID: run, SourceMessageID: "message", Phase: "ingest", ErrorKind: "ingest_error",
		Status: "error", ErrorMessage: store.SyncRunItemFilesPendingMessage,
	}))
	for _, state := range []string{"active", "indeterminate"} {
		report, err := f.Store.DiagnoseAcquisitionContext(t.Context(), store.AcquisitionDiagnosticOptions{
			SourceID: f.Source.ID, State: state, Limit: 1,
		})
		require.NoError(err)
		assert.Contains(report.String(), fmt.Sprintf("More details: --state %s --after-id %d", state, report.NextAfterID))
		report, err = f.Store.DiagnoseAcquisitionContext(t.Context(), store.AcquisitionDiagnosticOptions{
			SourceID: f.Source.ID, State: state, Limit: 100,
		})
		require.NoError(err)
		before, err := json.Marshal(report, json.Deterministic(true))
		require.NoError(err)
		human := report.String()
		var expected int
		for _, tc := range cases {
			if tc.state != state {
				continue
			}
			expected++
			var item *store.AcquisitionDiagnosticItem
			for i := range report.Items {
				if report.Items[i].SourceMessageID == tc.id {
					item = &report.Items[i]
					break
				}
			}
			require.NotNil(item, tc.id)
			assert.Equal(tc.synthetic, item.Synthetic, tc.id)
			assert.Equal(!tc.synthetic, item.HasError, tc.id)
			assert.Contains(human, fmt.Sprintf("%q: %s (run %d, item %d, synthetic=%t, error=%t) latest=%q (run %d, item %d)",
				tc.id, tc.label, run, item.OutcomeID, tc.synthetic, !tc.synthetic, "needs attention", run, item.LatestOutcomeID))
		}
		assert.Len(report.Items, expected)
		for _, hidden := range []string{"fetch_error", "batch_fetch_error", "ingest_error", "attachment_error", "remote_image_error", "attachment_remote_image_error", "future_kind", "future_phase", "ingest/error", "fetch/error", "hidden@example.com", "https://example.com/private"} {
			assert.NotContains(human, hidden)
		}
		after, err := json.Marshal(report, json.Deterministic(true))
		require.NoError(err)
		assert.Equal(before, after)
	}
}

func TestAcquisitionDiagnosticReplayContracts(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	run := f.StartSync()
	require.NoError(f.Store.RecordSyncRunItem(store.SyncRunItem{SyncRunID: run,
		SourceMessageID: "future", Phase: "fetch", Status: "error", ErrorKind: "fetch_error"}))
	cases := []struct {
		id, phase, kind string
		candidate       bool
	}{
		{"fetch", "fetch", "fetch_error", true},
		{"batch", "fetch", "batch_fetch_error", true},
		{"ingest", "ingest", "ingest_error", true},
		{"mime", "ingest", "attachment_error", true},
		{"image", "ingest", "remote_image_error", true},
		{"combined", "ingest", "attachment_remote_image_error", true},
		{"phase", "delete", "fetch_error", false},
		{"future", "fetch", "future_kind", false},
		{"", "fetch", "fetch_error", false},
		{"(unknown)", "fetch", "fetch_error", false},
	}
	for _, tc := range cases {
		item := store.SyncRunItem{SyncRunID: run, SourceMessageID: tc.id, Phase: tc.phase,
			ErrorKind: tc.kind, Status: "error", ErrorMessage: "Message files still need saving"}
		require.NoError(f.Store.RecordSyncRunItem(item))
		assert.Equal(tc.candidate, item.IsAcquisitionReplayCandidate(), tc.id)
	}
	for state, count := range map[string]int{"active": 6, "indeterminate": 4} {
		report, err := f.Store.DiagnoseAcquisitionContext(t.Context(), store.AcquisitionDiagnosticOptions{
			SourceID: f.Source.ID, State: state, Limit: 100})
		require.NoError(err)
		assert.Len(report.Items, count)
		assert.Equal(int64(3), report.Summary.ActiveErrors, "error text cannot make an outcome synthetic")
		for _, item := range report.Items {
			assert.Equal(state == "active", (store.SyncRunItem{SourceMessageID: item.SourceMessageID,
				Phase: item.Phase, ErrorKind: item.ErrorKind}).IsAcquisitionReplayCandidate())
		}
	}
}

func TestAcquisitionDiagnosticResolutionReferences(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	old := f.StartSync()
	require.NoError(f.Store.FailSync(old, "interrupted"))
	current := f.StartSync()
	record := func(run int64, id, status, kind string) int64 {
		t.Helper()
		require.NoError(f.Store.RecordSyncRunItem(store.SyncRunItem{SyncRunID: run,
			SourceMessageID: id, Phase: "fetch", Status: status, ErrorKind: kind}))
		var outcomeID int64
		require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT MAX(id) FROM sync_run_items`).Scan(&outcomeID))
		return outcomeID
	}
	record(current, "resolved", "error", "fetch_error")
	record(current, "unsettled", "error", "fetch_error")
	record(current, "unknown", "error", "future_kind")
	// Legacy, unscoped inserts can have newer item IDs from older runs. They
	// cannot settle a newer generation's failure, even when written last.
	record(old, "unsettled", "resolved", "acquired")
	record(old, "resolved", "resolved", "acquired")
	first := record(current, "resolved", "resolved", "acquired")
	record(current, "resolved", "resolved", "acquired")
	record(old, "resolved", "resolved", "acquired")
	unknownResolution := record(current, "unknown", "resolved", "acquired")
	opts := store.AcquisitionDiagnosticOptions{SourceID: f.Source.ID, State: "resolved", Limit: 50}
	report, err := f.Store.DiagnoseAcquisitionContext(t.Context(), opts)
	require.NoError(err)
	require.Len(report.Items, 1)
	assert.Equal(first, report.Items[0].ResolvedByID, "keep the earliest compatible resolution")
	opts.State = "indeterminate"
	report, err = f.Store.DiagnoseAcquisitionContext(t.Context(), opts)
	require.NoError(err)
	require.Len(report.Items, 2)
	for _, item := range report.Items {
		if item.SourceMessageID == "unknown" {
			assert.Equal(unknownResolution, item.ResolvedByID)
		} else {
			assert.Equal("unsettled", item.SourceMessageID)
			assert.Zero(item.ResolvedByID)
		}
	}
}

func TestAcquisitionDiagnosticCancellationDuringReadPG(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL lock-wait cancellation")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	blocker, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	_, err = blocker.ExecContext(t.Context(), `LOCK TABLE sync_run_items IN ACCESS EXCLUSIVE MODE`)
	require.NoError(err)
	type result struct {
		report *store.AcquisitionDiagnostic
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := f.Store.DiagnoseAcquisitionContext(ctx, store.AcquisitionDiagnosticOptions{SourceID: f.Source.ID, Limit: 50})
		done <- result{report, err}
	}()
	waitForLockWait(t, f.Store, "sync_run_items", "diagnostic must reach the blocked ledger read")
	cancel()
	// Cancellation must finish while the ledger remains locked; releasing it
	// first would let a later context check mask an uncancellable query.
	select {
	case got := <-done:
		require.ErrorIs(got.err, context.Canceled)
		assert.Nil(got.report)
	case <-time.After(5 * time.Second):
		stacktest.Dump(t)
		require.FailNow("cancelled diagnostic did not return")
	}
	require.NoError(blocker.Rollback())
}

func TestAcquisitionDiagnosticOperationTransitions(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	run := f.StartSync()
	record := func(id, phase, kind, status, message string) {
		t.Helper()
		require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{
			SyncRunID: run, SourceMessageID: id, Phase: phase, ErrorKind: kind, Status: status, ErrorMessage: message,
		}))
	}
	record("acquisition-files", "fetch", "fetch_error", "error", "fetch failed")
	record("acquisition-files", "ingest", "attachment_error", "error", store.SyncRunItemFilesPendingMessage)
	record("files-404", "ingest", "attachment_error", "error", "save failed")
	record("files-404", "fetch", "gmail_not_found", "skipped", "not found")
	record("marker-real", "ingest", "attachment_error", "error", store.SyncRunItemFilesPendingMessage)
	record("marker-real", "ingest", "attachment_remote_image_error", "error", "save failed")
	record("unknown-recent", "fetch", "fetch_error", "error", "fetch failed")
	record("unknown-recent", "fetch", "future_kind", "error", "unknown")
	record("unknown-status", "fetch", "fetch_error", "error", "fetch failed")
	record("unknown-status", "fetch", "future_kind", "future_status", "unknown")
	record("withdrawn-real", "ingest", "attachment_error", "skipped", "save failed")
	record("withdrawn-marker", "ingest", "remote_image_error", "skipped", store.SyncRunItemFilesPendingMessage)
	// Even a later acknowledgement cannot turn this withdrawn item into a success.
	record("withdrawn-real", "fetch", "acquired", "resolved", "acquired")
	record("ordinary-skip", "fetch", "gmail_not_found", "skipped", "not found")
	for i, message := range []string{"message files still need saving", "pending materialization", "pending image archival during attachment recovery"} {
		record(fmt.Sprintf("unshipped-%d", i), "ingest", "attachment_error", "error", message)
	}
	opts := store.AcquisitionDiagnosticOptions{SourceID: f.Source.ID, Limit: 100}
	report, err := f.Store.DiagnoseAcquisitionContext(t.Context(), opts)
	require.NoError(err)
	assert.Equal(store.AcquisitionDiagnosticSummary{
		ActiveObligations: 5, ActiveErrors: 4, HistoricalErrors: 1, Historical: 2, Indeterminate: 4, BlockedRemoteImages: 1,
	}, report.Summary)
	var active []string
	for _, item := range report.Items {
		active = append(active, item.SourceMessageID)
		if item.SourceMessageID == "acquisition-files" {
			assert.Equal("files", item.Operation)
			assert.True(item.Synthetic)
			assert.False(item.HasError)
		} else {
			assert.False(item.Synthetic)
			assert.True(item.HasError)
		}
	}
	replay, err := f.Store.ListUnresolvedSyncItemErrors(t.Context(), f.Source.ID)
	require.NoError(err)
	var candidates []string
	for _, item := range replay {
		if item.IsAcquisitionReplayCandidate() {
			candidates = append(candidates, item.SourceMessageID)
		}
	}
	assert.ElementsMatch(candidates, active)
	opts.State = "historical"
	report, err = f.Store.DiagnoseAcquisitionContext(t.Context(), opts)
	require.NoError(err)
	require.Len(report.Items, 2)
	assert.Contains(report.String(), "Details: past errors and skipped work\n")
	assert.Contains(report.String(), `latest="resolution recorded"`)
	assert.Contains(report.String(), `latest="skipped"`)
	assert.NotContains(report.String(), "ingest/skipped")
	for _, item := range report.Items {
		assert.Zero(item.ResolvedByID)
	}
	opts.State = "indeterminate"
	report, err = f.Store.DiagnoseAcquisitionContext(t.Context(), opts)
	require.NoError(err)
	require.Len(report.Items, 4)
	assert.Contains(report.String(), "Details: uncertain status\n")
	assert.Contains(report.String(), `latest="unrecognized status (see --json)"`)
	assert.NotContains(report.String(), "future_status")
	for _, item := range report.Items {
		assert.Zero(item.ResolvedByID)
		if item.SourceMessageID == "acquisition-files" {
			assert.Equal("acquisition", item.Operation)
		}
	}
}

func TestAcquisitionDiagnosticGCRetirement(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("GC is SQLite-only; withdrawn statuses are covered on both backends separately")
	}
	run := f.StartSync()
	for _, tc := range []struct{ id, message string }{
		{"gc-real", "save failed"}, {"gc-marker", store.SyncRunItemFilesPendingMessage},
	} {
		f.CreateMessage(tc.id)
		require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{SyncRunID: run,
			SourceMessageID: tc.id, Phase: "ingest", Status: "error", ErrorKind: "attachment_error", ErrorMessage: tc.message}))
		require.NoError(f.Store.MarkMessageDeletedContext(t.Context(), f.Source.ID, tc.id))
	}
	plan, err := f.Store.PlanGCContext(t.Context())
	require.NoError(err)
	deleted, err := f.Store.ExecuteGCContext(t.Context(), plan)
	require.NoError(err)
	assert.Equal(int64(2), deleted)
	report, err := f.Store.DiagnoseAcquisitionContext(t.Context(), store.AcquisitionDiagnosticOptions{SourceID: f.Source.ID, State: "historical", Limit: 10})
	require.NoError(err)
	assert.Equal(store.AcquisitionDiagnosticSummary{Historical: 2, HistoricalErrors: 1}, report.Summary)
	require.Len(report.Items, 2)
	for _, item := range report.Items {
		assert.Equal("skipped", item.LatestStatus)
		assert.Zero(item.ResolvedByID)
	}
}

func TestAcquisitionDiagnosticSnapshot(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	run := f.StartSync()
	require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{SyncRunID: run,
		SourceMessageID: "snapshot", Phase: "fetch", Status: "error", ErrorKind: "fetch_error"}))
	ctx, release, err := f.Store.BeginReadSnapshotContext(t.Context())
	require.NoError(err)
	defer release()
	opts := store.AcquisitionDiagnosticOptions{SourceID: f.Source.ID, Limit: 10}
	before, err := f.Store.DiagnoseAcquisitionContext(ctx, opts)
	require.NoError(err)
	require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{SyncRunID: run,
		SourceMessageID: "snapshot", Phase: "fetch", Status: "resolved", ErrorKind: "acquired"}))
	during, err := f.Store.DiagnoseAcquisitionContext(ctx, opts)
	require.NoError(err)
	during.ObservedAt = before.ObservedAt
	assert.Equal(before, during, "coverage, counts, and details must share the bound snapshot")
	release()
	after, err := f.Store.DiagnoseAcquisitionContext(t.Context(), opts)
	require.NoError(err)
	assert.Zero(after.Summary.ActiveObligations)
	assert.Equal(int64(1), after.Summary.ResolvedObligations)
	assert.Greater(after.LedgerThroughID, before.LedgerThroughID)
}

func TestAcquisitionDiagnosticBoundaryOrder(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	var runs []int64
	for i := range 3 {
		run, err := f.Store.StartSync(f.Source.ID, "incremental")
		require.NoError(err)
		runs = append(runs, run)
		require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{SyncRunID: run,
			SourceMessageID: fmt.Sprintf("boundary-%d", i), Phase: "fetch", Status: "error", ErrorKind: "fetch_error"}))
		require.NoError(f.Store.CompleteSync(run, "100"))
	}
	stamp := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for i, run := range runs {
		delta := []time.Duration{0, 2 * time.Hour, time.Hour}[i]
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sync_runs SET completed_at = ? WHERE id = ?`), stamp.Add(delta), run)
		require.NoError(err)
	}
	opts := store.AcquisitionDiagnosticOptions{SourceID: f.Source.ID, Limit: 10}
	for _, tie := range []bool{false, true} {
		want, count := runs[1], 2
		if tie {
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sync_runs SET completed_at = ? WHERE id = ?`), stamp.Add(2*time.Hour), runs[2])
			require.NoError(err)
			want, count = runs[2], 1
		}
		report, err := f.Store.DiagnoseAcquisitionContext(t.Context(), opts)
		require.NoError(err)
		assert.Equal(want, report.IncrementalBoundaryID)
		assert.Len(report.Items, count)
		replay, err := f.Store.ListUnresolvedSyncItemErrors(t.Context(), f.Source.ID)
		require.NoError(err)
		assert.Len(replay, count)
		for i, item := range replay {
			assert.Equal(item.ID, report.Items[i].OutcomeID)
		}
	}
}

func TestAcquisitionDiagnosticImageStatsSelection(t *testing.T) {
	for _, tc := range []struct {
		kind, status string
		pending      bool
	}{
		{"attachment_error", "error", true}, {"attachment_remote_image_error", "error", true},
		{"remote_image_error", "error", false}, {"future_kind", "error", false},
		{"acquired", "resolved", false}, {"attachment_error", "skipped", false},
	} {
		t.Run(tc.kind+"/"+tc.status, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := storetest.New(t)
			messageID := f.CreateMessage("target")
			run := f.StartSync()
			record := func(run int64, id, kind, status string) {
				t.Helper()
				require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{SyncRunID: run,
					SourceMessageID: id, Phase: "ingest", Status: status, ErrorKind: kind}))
			}
			record(run, "target", "attachment_error", "error")
			record(run, "target", tc.kind, tc.status)
			record(run, "neighbor", "attachment_error", "error")
			other, err := f.Store.GetOrCreateSource("gmail", "images@example.com")
			require.NoError(err)
			otherRun, err := f.Store.StartSync(other.ID, "full")
			require.NoError(err)
			record(otherRun, "target", "attachment_error", "error")
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET has_attachments = TRUE, attachment_count = 3 WHERE id = ?`), messageID)
			require.NoError(err)
			require.NoError(f.Store.RecomputeRemoteImageAttachmentStatsContext(t.Context(), messageID))
			var count int
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT attachment_count FROM messages WHERE id = ?`), messageID).Scan(&count))
			want := 0
			if tc.pending {
				want = 3
			}
			assert.Equal(want, count, "only this source and identity's latest active MIME work may preserve expected counts")
			// A completed incremental hides older debt without rewriting it.
			require.NoError(f.Store.FailSync(run, "interrupted"))
			boundary, err := f.Store.StartSync(f.Source.ID, "incremental")
			require.NoError(err)
			require.NoError(f.Store.CompleteSync(boundary, "100"))
			require.NoError(f.Store.RecomputeRemoteImageAttachmentStatsContext(t.Context(), messageID))
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT attachment_count FROM messages WHERE id = ?`), messageID).Scan(&count))
			assert.Zero(count)
		})
	}
}

func TestAcquisitionDiagnosticImageStatsLockPG(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL message-row lock")
	}
	messageID := f.CreateMessage("image-lock")
	run := f.StartSync()
	require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{SyncRunID: run,
		SourceMessageID: "image-lock", Phase: "ingest", Status: "error", ErrorKind: "attachment_error"}))
	_, err := f.Store.DB().ExecContext(t.Context(), `UPDATE messages SET attachment_count = 3, has_attachments = TRUE WHERE id = $1`, messageID)
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	tx, err := f.Store.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var locked int64
	require.NoError(tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE id = $1 FOR UPDATE`, messageID).Scan(&locked))
	done := make(chan error, 1)
	go func() { done <- f.Store.RecomputeRemoteImageAttachmentStatsContext(ctx, messageID) }()
	waitForLockWait(t, f.Store, "SELECT source_id, source_message_id", "statistics must wait for the message lock before selecting debt")
	_, err = tx.ExecContext(ctx, `INSERT INTO sync_run_items (sync_run_id, source_message_id, phase, status, error_kind, error_message)
		VALUES ($1, 'image-lock', 'fetch', 'resolved', 'acquired', 'acquired')`, run)
	require.NoError(err)
	require.NoError(tx.Commit())
	select {
	case err := <-done:
		require.NoError(err)
	case <-ctx.Done():
		stacktest.Dump(t)
		require.FailNow("statistics did not finish", "%v", ctx.Err())
	}
	var count int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT attachment_count FROM messages WHERE id = $1`, messageID).Scan(&count))
	assert.Zero(count, "selection must observe the acknowledgement committed before it acquired the lock")
}
