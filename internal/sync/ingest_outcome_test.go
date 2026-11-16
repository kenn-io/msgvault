package sync

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

func TestIngestOutcomeCommittedDebtWriteFailure(t *testing.T) {
	for _, route := range []string{"full", "history", "label-add", "replay"} {
		t.Run(route, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			e := attachmentPersistenceEnv(t)
			source := e.CreateSourceWithHistory(t, "1000")
			e.Mock.AddMessage("committed", testMIMEWithAttachment(), []string{"INBOX"})
			e.SetHistory(1001, historyAdded("committed"))
			if route == "label-add" {
				e.SetHistory(1001, historyLabelAdded("committed", "INBOX"))
			}
			if route == "replay" {
				runID, startErr := e.Store.StartSync(source.ID, "full")
				require.NoError(startErr)
				require.NoError(e.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{
					SyncRunID: runID, SourceMessageID: "committed", Phase: syncItemPhaseFetch,
					Status: store.SyncRunItemStatusError, ErrorKind: syncItemKindFetchError,
				}))
				require.NoError(e.Store.FailSyncContext(t.Context(), runID, "synthetic interrupted acquisition"))
				e.SetHistory(1001)
			}
			blocked := filepath.Join(e.TmpDir, "blocked")
			require.NoError(os.WriteFile(blocked, []byte("synthetic non-directory"), 0600))
			// Allow the atomic RAW/obligation commit, then reject the real failure
			// outcome. A committed identity must not bypass this required write.
			allow := acquisitionWriteFault(t, e, "sync_run_items", "INSERT",
				"NEW.status = 'error' AND NEW.error_message <> '"+store.SyncRunItemFilesPendingMessage+"'")
			service := New(e.Mock, e.Store, &Options{SourceType: sourceTypeGmail, AttachmentsDir: blocked})
			var err error
			if route == "full" {
				_, err = service.Full(t.Context(), testEmail)
			} else {
				_, err = service.Incremental(t.Context(), source)
			}
			require.ErrorIs(err, errAcquisitionFailureRecord)
			ids, err := e.Store.MessageExistsBatchContext(t.Context(), source.ID, []string{"committed"})
			require.NoError(err)
			require.Contains(ids, "committed")
			run, err := e.Store.GetLatestSyncContext(t.Context(), source.ID, 0)
			require.NoError(err)
			assert.Equal(store.SyncStatusFailed, run.Status)
			assert.Zero(run.MessagesAdded)
			assert.Zero(run.ErrorsCount, "count only an error whose required outcome was recorded")
			source, err = e.Store.GetSourceByIDContext(t.Context(), source.ID)
			require.NoError(err)
			assert.Equal("1000", source.SyncCursor.String)
			debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
			require.NoError(err)
			require.Len(debt, 1)
			assert.Equal(store.SyncRunItemFilesPendingMessage, debt[0].ErrorMessage)
			allow()
			require.NoError(os.Remove(blocked))
			e.Mock.GetMessageCalls = nil
			summary, err := New(e.Mock, e.Store, &Options{AttachmentsDir: blocked}).Incremental(t.Context(), source)
			require.NoError(err)
			assert.Zero(summary.MessagesAdded, "retry repairs the retained row instead of acquiring it twice")
			assert.Zero(summary.Errors)
			assert.Empty(e.Mock.GetMessageCalls)
		})
	}
}

func TestIngestOutcomeLabelIdentitySurvivesRejectedDebt(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	source := e.CreateSourceWithHistory(t, "1000")
	runID, err := e.Store.StartSync(source.ID, "incremental")
	require.NoError(err)
	e.Mock.AddMessage("committed", testMIMEWithAttachment(), []string{"INBOX"})
	blocked := filepath.Join(e.TmpDir, "blocked")
	require.NoError(os.WriteFile(blocked, []byte("synthetic non-directory"), 0600))
	service := New(e.Mock, e.Store.ScopedToSync(source.ID, runID), &Options{AttachmentsDir: blocked})
	service.setAcquisitionReplay(runID, nil)
	labels, err := service.syncLabels(t.Context(), source.ID)
	require.NoError(err)
	allow := acquisitionWriteFault(t, e, "sync_run_items", "INSERT",
		"NEW.status = 'error' AND NEW.error_message <> '"+store.SyncRunItemFilesPendingMessage+"'")
	existing := make(map[string]int64)
	checkpoint := &store.Checkpoint{}
	summary := &gmail.SyncSummary{}
	updated, discoverable, err := service.handleLabelChange(t.Context(), runID, source.ID, "committed", "thread", nil, labels, true, existing, checkpoint, summary)
	require.ErrorIs(err, errAcquisitionFailureRecord)
	ids, err := e.Store.MessageExistsBatchContext(t.Context(), source.ID, []string{"committed"})
	require.NoError(err)
	require.Contains(ids, "committed")
	assert.Equal(ids["committed"], existing["committed"])
	assert.False(updated)
	assert.False(discoverable)
	assert.Zero(checkpoint.MessagesAdded)
	assert.Zero(checkpoint.ErrorsCount)
	assert.Zero(summary.BytesDownloaded)
	allow()
}

func TestIngestOutcomeSettlementFailureIsAcquired(t *testing.T) {
	for _, route := range []string{"full", "history", "label-add", "replay"} {
		t.Run(route, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			e := attachmentPersistenceEnv(t)
			source := e.CreateSourceWithHistory(t, "1000")
			raw := testMIMEWithAttachment()
			e.Mock.AddMessage("committed", raw, []string{"INBOX"})
			e.SetHistory(1001, historyAdded("committed"))
			if route == "label-add" {
				e.SetHistory(1001, historyLabelAdded("committed", "INBOX"))
			}
			if route == "replay" {
				runID, err := e.Store.StartSync(source.ID, "full")
				require.NoError(err)
				require.NoError(e.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{
					SyncRunID: runID, SourceMessageID: "committed", Phase: syncItemPhaseFetch,
					Status: store.SyncRunItemStatusError, ErrorKind: syncItemKindFetchError,
				}))
				require.NoError(e.Store.FailSyncContext(t.Context(), runID, "synthetic interrupted acquisition"))
				e.SetHistory(1001)
			}
			allow := acquisitionWriteFault(t, e, "sync_run_items", "INSERT", "NEW.status = 'resolved'")
			service := New(e.Mock, e.Store, &Options{AttachmentsDir: filepath.Join(e.TmpDir, "attachments")})
			var summary *gmail.SyncSummary
			var err error
			if route == "full" {
				summary, err = service.Full(t.Context(), testEmail)
			} else {
				summary, err = service.Incremental(t.Context(), source)
			}
			require.NoError(err)
			assert.Equal(int64(1), summary.MessagesAdded)
			assert.Equal(int64(len(raw)), summary.BytesDownloaded)
			assert.Equal(int64(1), summary.Errors, "failure to settle is still a post-commit error")
			ids, err := e.Store.MessageExistsBatchContext(t.Context(), source.ID, []string{"committed"})
			require.NoError(err)
			require.Contains(ids, "committed")
			refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), ids["committed"])
			require.NoError(err)
			require.Len(refs, 1, "file publication succeeded before settlement failed")
			debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
			require.NoError(err)
			assert.Len(debt, 1)
			allow()
		})
	}
}

func TestReplayPostCommitStopPrecedesAcquisition(t *testing.T) {
	for _, stop := range []string{"cancellation", "supersession"} {
		t.Run(stop, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			e := attachmentPersistenceEnv(t)
			source := e.CreateSourceWithHistory(t, "1000")
			runID, err := e.Store.StartSync(source.ID, "incremental")
			require.NoError(err)
			e.Mock.AddMessage("committed", revisionHTMLMIME(), []string{"INBOX"})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			scoped := e.Store.ScopedToSync(source.ID, runID)
			images, _ := revisionImageServer(t)
			dial := images.DialContext
			images.DialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
				// The real image request happens after the RAW/obligation commit.
				if stop == "cancellation" {
					cancel()
					return nil, context.Canceled
				}
				if err := e.Store.FailSyncContext(t.Context(), runID, "synthetic supersession after commit"); err != nil {
					return nil, err
				}
				// Let the real download succeed. Its occurrence write then fails
				// through the scoped Store, preserving ErrSyncRunSuperseded.
				return dial(dialCtx, network, address)
			}
			service := New(e.Mock, scoped, &Options{AttachmentsDir: filepath.Join(e.TmpDir, "attachments"), RemoteImages: images})
			service.setAcquisitionReplay(runID, []string{"committed"})
			labels, err := service.syncLabels(t.Context(), source.ID)
			require.NoError(err)
			checkpoint := &store.Checkpoint{}
			summary := &gmail.SyncSummary{}
			acquired, err := service.replayAcquisitionFailures(ctx, runID, source.ID, []string{"committed"}, labels, checkpoint, summary)
			if stop == "cancellation" {
				require.ErrorIs(ctx.Err(), context.Canceled)
			} else {
				require.ErrorIs(err, store.ErrSyncRunSuperseded)
			}
			var committed *committedMessageError
			require.ErrorAs(err, &committed)
			assert.Positive(committed.messageID)
			assert.Empty(acquired)
			assert.Zero(checkpoint.MessagesAdded)
			assert.Zero(checkpoint.ErrorsCount, "stop before recording and counting the post-commit error")
			assert.Zero(summary.BytesDownloaded)
			retained, err := e.Store.GetMessageRawContext(t.Context(), committed.messageID)
			require.NoError(err)
			assert.Equal(revisionHTMLMIME(), retained)
			items, err := e.Store.ListSyncRunItemsContext(t.Context(), runID, "", 10)
			require.NoError(err)
			require.Len(items, 1, "only the atomic pending marker precedes the stop")
			assert.Equal(store.SyncRunItemFilesPendingMessage, items[0].ErrorMessage)
		})
	}
}
