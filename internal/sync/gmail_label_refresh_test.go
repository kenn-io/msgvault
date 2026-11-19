package sync

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

func TestGmailFullLabelRefresh(t *testing.T) {
	// Serial: the production Gmail client uses the fixture HTTP transport.
	e, f, ids := recoveryLabelsEnv(t)
	f.expire = false
	f.labels["normal1"] = []string{"TRASH", "STARRED"}
	otherLabels := make(map[int64]int64)
	for _, sourceType := range []string{"gmail", "imap"} {
		source, err := e.Store.GetOrCreateSourceContext(t.Context(), sourceType, "other@example.test")
		require.NoError(t, err)
		conversation, err := e.Store.EnsureConversationWithTypeContext(t.Context(), source.ID, "thread_normal1", "email_thread", "Synthetic")
		require.NoError(t, err)
		id, err := e.Store.UpsertMessageContext(t.Context(), &store.Message{
			SourceID: source.ID, ConversationID: conversation, SourceMessageID: "normal1", MessageType: "email",
		})
		require.NoError(t, err)
		labels, err := e.Store.EnsureLabelsBatch(t.Context(), source.ID, map[string]store.LabelInfo{"INBOX": {Name: "INBOX", Type: "system"}})
		require.NoError(t, err)
		_, err = e.Store.ReconcileMessageLabels(t.Context(), id, []int64{labels["INBOX"]}, true)
		require.NoError(t, err)
		otherLabels[id] = labels["INBOX"]
	}
	opts := DefaultOptions()
	opts.RefreshLabels = true
	e.Syncer = New(e.Syncer.client, e.Store, opts)
	for attempt := range 2 {
		before := f.metadataGets
		f.listTokens = nil
		summary, err := e.Syncer.Full(t.Context(), testEmail)
		require.NoError(t, err)
		assertRecoveryLabels(t, e, ids, f.labels)
		assert.Equal(t, "1000", gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
		assert.Equal(t, uint64(1000), summary.FinalHistoryID)
		assert.Empty(t, f.historyStarts, "the sweep does not consume History")
		assert.Equal(t, []string{"", "p2"}, f.listTokens, "one complete listing, including Spam and Trash")
		assert.Equal(t, 4, f.metadataGets-before)
		assert.Equal(t, 4, f.rawGets, "healthy archived RAW is never downloaded again")
		assert.Zero(t, summary.MessagesAdded)
		for id, label := range otherLabels {
			actual, err := e.Store.MessageLabelIDsContext(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, []int64{label}, actual, "overlapping provider IDs in other sources are unchanged")
		}
		if attempt == 0 {
			assert.Equal(t, int64(4), summary.MessagesUpdated)
		} else {
			assert.Zero(t, summary.MessagesUpdated, "a quiescent second sweep changes no memberships")
		}
	}
}

func TestGmailFullLabelRefreshFailuresRetryPage(t *testing.T) {
	for _, fault := range []string{"read", "identity", "unknown", "write", "checkpoint", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			e, f, ids := recoveryLabelsEnv(t)
			f.expire = false
			opts := DefaultOptions()
			opts.RefreshLabels = true
			api := e.Syncer.client
			e.Syncer = New(api, e.Store, opts)
			removeFault := func() {}
			ctx := t.Context()
			switch fault {
			case "write":
				removeFault = acquisitionWriteFault(t, e, "message_labels", "DELETE", fmt.Sprintf("OLD.message_id = %d", ids["trash"]))
			case "checkpoint":
				removeFault = acquisitionWriteFault(t, e, "sync_runs", "UPDATE", "NEW.cursor_before = 'p2'")
			default:
				f.fault = fault
			}
			if fault == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(t.Context())
				defer cancel()
				f.cancel = cancel
			}
			_, err := e.Syncer.Full(ctx, testEmail)
			require.Error(t, err)
			if fault == "unknown" {
				require.ErrorContains(t, err, "run sync-full --refresh-labels again")
			}
			run, err := e.Store.GetLatestSyncContext(t.Context(), gmailCoverageSource(t.Context(), t, e).ID, 0)
			require.NoError(t, err)
			assert.Equal(t, store.SyncStatusFailed, run.Status)
			assert.Equal(t, "1000", gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
			assertRecoveryLabels(t, e, ids, map[string][]string{"trash": {"TRASH"}})
			assertDeletedFromSource(t, e.Store, "trash", false)
			assert.Equal(t, 4, f.rawGets)
			debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), gmailCoverageSource(t.Context(), t, e).ID)
			require.NoError(t, err)
			assert.Empty(t, debt, "a metadata refresh failure is not RAW acquisition debt")
			if fault == "checkpoint" {
				assert.Empty(t, run.CursorBefore.String, "a rejected first-page checkpoint cannot publish p2")
			} else {
				assert.Equal(t, "p2", run.CursorBefore.String, "the failing metadata page remains retryable")
			}
			removeFault()
			reopenAcquisitionStore(t, e)
			e.Syncer = New(api, e.Store, opts)
			switch fault {
			case "unknown":
				// A subsequent run observes the newly created label in its
				// startup catalog. Keep the metadata observation unchanged.
				f.extraLabels = []string{"Label_unknown"}
			default:
				f.fault = ""
			}
			_, err = e.Syncer.Full(t.Context(), testEmail)
			require.NoError(t, err)
			if fault == "unknown" {
				f.labels["trash"] = []string{"INBOX", "Label_unknown"}
			}
			assertRecoveryLabels(t, e, ids, f.labels)
			assertDeletedFromSource(t, e.Store, "trash", false)
			assert.Empty(t, f.historyStarts)
			assert.Equal(t, "1000", gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
			assert.Equal(t, 4, f.rawGets)
		})
	}
}

func TestGmailFullLabelRefreshLocalReplayCheckpointFailure(t *testing.T) {
	e, f, _ := recoveryLabelsEnv(t)
	source := gmailCoverageSource(t.Context(), t, e)
	previous, err := e.Store.GetLatestSyncContext(t.Context(), source.ID, 0)
	require.NoError(t, err)
	require.NoError(t, e.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{
		SyncRunID: previous.ID, SourceMessageID: "normal1", Phase: syncItemPhaseIngest,
		Status: store.SyncRunItemStatusError, ErrorKind: syncItemKindAttachmentError, ErrorMessage: "synthetic pending files",
	}))
	opts := DefaultOptions()
	opts.RefreshLabels = true
	e.Syncer = New(e.Syncer.client, e.Store, opts)
	f.emptyListing, f.expire = true, false
	f.listTokens = nil
	removeFault := acquisitionWriteFault(t, e, "sync_runs", "UPDATE", "NEW.cursor_after = '2000' AND NEW.messages_processed = 0")
	_, err = e.Syncer.Full(t.Context(), testEmail)
	require.ErrorContains(t, err, "save local replay checkpoint")
	assert.Equal(t, "1000", gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
	assert.Empty(t, f.listTokens, "failed required checkpoint stops before an empty enumeration can complete the run")
	run, err := e.Store.GetLatestSyncContext(t.Context(), source.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, store.SyncStatusFailed, run.Status)
	removeFault()
}

func TestGmailFullLabelRefreshResumeModes(t *testing.T) {
	for _, scenario := range []struct {
		name               string
		first              bool
		next               bool
		noResume           bool
		incrementalBetween bool
		wantResumed        bool
	}{
		{name: "acquisition then refresh", next: true},
		{name: "refresh then acquisition", first: true},
		{name: "refresh resumes refresh", first: true, next: true, wantResumed: true},
		{name: "noresume restarts refresh", first: true, next: true, noResume: true},
		{name: "incremental between refresh attempts", first: true, next: true, wantResumed: true, incrementalBetween: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			e, f, ids := recoveryLabelsEnv(t)
			api := e.Syncer.client
			f.expire, f.stopAtToken = false, "p2"
			opts := DefaultOptions()
			opts.RefreshLabels = scenario.first
			e.Syncer = New(api, e.Store, opts)
			_, err := e.Syncer.Full(t.Context(), testEmail)
			require.ErrorContains(t, err, "synthetic interrupted listing")
			wantCursor := "1000"
			if scenario.incrementalBetween {
				f.history = 2200
				e.Syncer = New(api, e.Store, nil)
				_, err = e.Syncer.Incremental(t.Context(), gmailCoverageSource(t.Context(), t, e))
				require.NoError(t, err)
				wantCursor = "2200"
			}
			reopenAcquisitionStore(t, e)
			opts = DefaultOptions()
			opts.RefreshLabels, opts.NoResume = scenario.next, scenario.noResume
			e.Syncer = New(api, e.Store, opts)
			f.stopAtToken, f.listTokens = "", nil
			before := f.metadataGets
			summary, err := e.Syncer.Full(t.Context(), testEmail)
			require.NoError(t, err)
			assert.Equal(t, scenario.wantResumed, summary.WasResumed)
			require.NotEmpty(t, f.listTokens)
			if scenario.wantResumed {
				assert.Equal(t, "p2", f.listTokens[0])
				assert.Equal(t, 2, f.metadataGets-before)
			} else {
				assert.Empty(t, f.listTokens[0])
			}
			if scenario.next {
				assertRecoveryLabels(t, e, ids, f.labels)
			} else {
				assertRecoveryLabels(t, e, ids, map[string][]string{"trash": {"TRASH"}})
				assert.Equal(t, before, f.metadataGets, "ordinary acquisition keeps its archived-RAW shortcut")
			}
			assert.Equal(t, wantCursor, gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
			assert.Equal(t, 4, f.rawGets)
		})
	}
}

func TestGmailFullLabelRefreshSkipsUnarchivedMail(t *testing.T) {
	e, f, ids := recoveryLabelsEnv(t)
	f.expire = false
	source := gmailCoverageSource(t.Context(), t, e)
	_, err := e.Store.DB().ExecContext(t.Context(), e.Store.Rebind("DELETE FROM message_raw WHERE message_id = ?"), ids["normal2"])
	require.NoError(t, err)
	// "excluded" was never archived, as after an earlier date-filtered sync.
	// "retry" failed acquisition earlier and remains acquisition debt.
	previous, err := e.Store.GetLatestSyncContext(t.Context(), source.ID, 0)
	require.NoError(t, err)
	require.NoError(t, e.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{
		SyncRunID: previous.ID, SourceMessageID: "retry", Phase: syncItemPhaseFetch,
		Status: store.SyncRunItemStatusError, ErrorKind: syncItemKindFetchError, ErrorMessage: "synthetic fetch failure",
	}))
	f.extraMessageIDs = []string{"excluded", "retry"}
	f.labels["excluded"] = []string{"INBOX"}
	f.labels["retry"] = []string{"INBOX"}
	opts := DefaultOptions()
	opts.RefreshLabels = true
	e.Syncer = New(e.Syncer.client, e.Store, opts)
	_, err = e.Syncer.Full(t.Context(), testEmail)
	require.NoError(t, err)
	archived, err := e.Store.MessageExistsBatchContext(t.Context(), source.ID, []string{"excluded", "retry", "normal2"})
	require.NoError(t, err)
	assert.NotContains(t, archived, "excluded", "label refresh does not download mail the archive never held")
	require.Contains(t, archived, "retry", "earlier acquisition failures are still retried")
	assert.Equal(t, ids["normal2"], archived["normal2"], "reacquisition keeps the existing archive identity")
	ids["retry"] = archived["retry"]
	want := maps.Clone(f.labels)
	delete(want, "excluded")
	assertRecoveryLabels(t, e, ids, want)
	assert.Equal(t, 3, f.metadataGets, "healthy archived messages use metadata")
	assert.Equal(t, 6, f.rawGets, "only the missing-RAW and retried messages download content")
	assert.Equal(t, "1000", gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
}

func TestGmailFullLabelRefreshSkipsMessageGoneMidScan(t *testing.T) {
	e, f, ids := recoveryLabelsEnv(t)
	f.expire, f.fault = false, "gone"
	opts := DefaultOptions()
	opts.RefreshLabels = true
	e.Syncer = New(e.Syncer.client, e.Store, opts)
	summary, err := e.Syncer.Full(t.Context(), testEmail)
	require.NoError(t, err, "History replay from the preserved cursor handles the deletion")
	assert.Zero(t, summary.Errors)
	// The vanished message keeps its archived labels and is not tombstoned;
	// only complete recovery infers deletion from absence.
	f.labels["trash"] = []string{"TRASH"}
	assertRecoveryLabels(t, e, ids, f.labels)
	assertDeletedFromSource(t, e.Store, "trash", false)
	assert.Equal(t, "1000", gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
	assert.Equal(t, 4, f.rawGets)
}

func TestGmailFullLabelRefreshResumesPendingRecovery(t *testing.T) {
	for _, phase := range []string{"enumerate", "catchup"} {
		for _, noResume := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/noresume=%t", phase, noResume), func(t *testing.T) {
				e, f, ids := recoveryLabelsEnv(t)
				f.expire = false
				api := e.Syncer.client
				if phase == "enumerate" {
					f.stopAtToken = "p2"
				} else {
					f.stopHistory = true
				}
				_, err := e.Syncer.RecoverExpiredHistory(t.Context(), gmailCoverageSource(t.Context(), t, e))
				require.Error(t, err)
				reopenAcquisitionStore(t, e)
				opts := DefaultOptions()
				opts.RefreshLabels, opts.NoResume = true, noResume
				e.Syncer = New(api, e.Store, opts)
				f.stopAtToken, f.stopHistory, f.listTokens, f.historyStarts = "", false, nil, nil
				f.fault = "unknown" // Preserve the existing recovery catalog policy.
				_, err = e.Syncer.Full(t.Context(), testEmail)
				require.NoError(t, err)
				assertRecoveryLabels(t, e, ids, f.labels)
				assert.Equal(t, "2001", gmailCoverageSource(t.Context(), t, e).SyncCursor.String, "pending recovery still catches up before publishing")
				if noResume {
					require.NotEmpty(t, f.listTokens)
					assert.Empty(t, f.listTokens[0], "noresume restarts complete recovery from the current profile boundary")
				} else {
					assert.NotEmpty(t, f.historyStarts)
				}
				if phase == "catchup" && !noResume {
					assert.Empty(t, f.listTokens, "a pinned catch-up phase does not repeat completed enumeration")
				}
				assert.Equal(t, 4, f.rawGets)
			})
		}
	}
}

func TestGmailFullLabelRefreshPendingRecoveryAcquiresCatchupMessage(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(strconv.FormatBool(refresh), func(t *testing.T) {
			e, f, _ := recoveryLabelsEnv(t)
			api := e.Syncer.client
			f.expire, f.stopHistory = false, true
			_, err := e.Syncer.RecoverExpiredHistory(t.Context(), gmailCoverageSource(t.Context(), t, e))
			require.ErrorContains(t, err, "synthetic interrupted History")
			reopenAcquisitionStore(t, e)
			opts := DefaultOptions()
			opts.RefreshLabels = refresh
			e.Syncer = New(api, e.Store, opts)
			f.stopHistory, f.listTokens = false, nil
			f.event = "arrival"
			// Catch-up acquisition retains recovery's existing catalog-race
			// policy even though its caller requested a strict ordinary sweep.
			f.labels["arrival"] = []string{"INBOX", "Label_unknown"}
			_, err = e.Syncer.Full(t.Context(), testEmail)
			require.NoError(t, err)
			source := gmailCoverageSource(t.Context(), t, e)
			ids, err := e.Store.MessageExistsBatchContext(t.Context(), source.ID, []string{"arrival"})
			require.NoError(t, err)
			require.Len(t, ids, 1)
			labels, err := e.Store.MessageLabelIDsContext(t.Context(), ids["arrival"])
			require.NoError(t, err)
			inbox, err := e.Store.LabelIDContext(t.Context(), source.ID, "INBOX")
			require.NoError(t, err)
			assert.Equal(t, []int64{inbox}, labels, "flagged and unflagged pending recovery retain the same catalog policy")
			assert.Empty(t, f.listTokens, "resume the pinned catch-up phase")
			assert.Equal(t, "2001", source.SyncCursor.String)
			assert.Equal(t, 5, f.rawGets, "only catch-up's new message needs RAW")
		})
	}
}

type incompleteLabelRefreshAPI struct {
	gmail.API
	messageLabelBatchReader
}

func (incompleteLabelRefreshAPI) LabelsSnapshotComplete() bool { return false }

func TestGmailFullLabelRefreshRequiresCompleteMetadata(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(strconv.FormatBool(incomplete), func(t *testing.T) {
			e, f, _ := recoveryLabelsEnv(t)
			api := e.Syncer.client
			var wrapped gmail.API = struct{ gmail.API }{api}
			if incomplete {
				reader, ok := api.(messageLabelBatchReader)
				require.True(t, ok)
				wrapped = incompleteLabelRefreshAPI{api, reader}
			}
			opts := DefaultOptions()
			opts.RefreshLabels = true
			e.Syncer = New(wrapped, e.Store, opts)
			f.listTokens = nil
			_, err := e.Syncer.Full(t.Context(), testEmail)
			require.ErrorContains(t, err, "complete Gmail label metadata")
			assert.Empty(t, f.listTokens)
			assert.Zero(t, f.metadataGets)
			assert.Equal(t, 4, f.rawGets)
		})
	}
}

func TestGmailFullLabelRefreshRejectsPartialOrOtherSource(t *testing.T) {
	for _, scenario := range []string{"query", "limit", "imap", "incremental"} {
		t.Run(scenario, func(t *testing.T) {
			e, f, _ := recoveryLabelsEnv(t)
			opts := DefaultOptions()
			opts.RefreshLabels = true
			source := gmailCoverageSource(t.Context(), t, e)
			switch scenario {
			case "query":
				opts.Query = "in:inbox"
			case "limit":
				opts.Limit = 1
			case "imap":
				source.SourceType = "imap"
			}
			e.Syncer = New(e.Syncer.client, e.Store, opts)
			before, err := e.Store.GetLatestSyncContext(t.Context(), source.ID, 0)
			require.NoError(t, err)
			f.listTokens = nil
			if scenario == "incremental" {
				_, err = e.Syncer.IncrementalWithHistoryRecovery(t.Context(), source, nil)
			} else {
				_, err = e.Syncer.FullWithFinalizer(t.Context(), source, nil)
			}
			require.Error(t, err)
			after, err := e.Store.GetLatestSyncContext(t.Context(), source.ID, 0)
			require.NoError(t, err)
			assert.Equal(t, before.ID, after.ID, "invalid options acquire no new sync execution")
			assert.Empty(t, f.listTokens)
			assert.Zero(t, f.metadataGets)
			assert.Equal(t, 4, f.rawGets)
		})
	}
}

func TestGmailFullLabelRefreshFingerprintCompatibility(t *testing.T) {
	t.Parallel()
	// Saved acquire-only request identifiers from the pre-refresh contract.
	for _, fixture := range []struct {
		sourceType  string
		fingerprint string
	}{
		{"gmail", "full:v3-history-boundary:864b224bbaf1812b29969fde37ac8c7bb08b34ded81e0bc47e35023e9d480d18"},
		{"", "full:v3-history-boundary:49af1e10cf1567681f4ebfe29694f3390d5281093d06768e7edafb99a586da38"},
		{"imap", "full:v1:7dd8be04f24a48298e2f2bb65f25b115ea714582eeea7ddfd578031b3e4cb20b"},
	} {
		opts := DefaultOptions()
		opts.SourceType = fixture.sourceType
		syncer := &Syncer{opts: opts}
		assert.Equal(t, fixture.fingerprint, syncer.fullSyncRequestFingerprint())
		if fixture.sourceType != "imap" {
			opts.RefreshLabels = true
			assert.NotEqual(t, fixture.fingerprint, syncer.fullSyncRequestFingerprint())
		}
	}
}
