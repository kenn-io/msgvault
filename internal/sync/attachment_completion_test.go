package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestAttachmentFullReplaysLocalDebtOutsideListing(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	dir := filepath.Join(e.TmpDir, "blocked")
	require.NoError(os.WriteFile(dir, []byte("blocked"), 0600))
	opts := &Options{AttachmentsDir: dir}
	raw := testMIMEWithAttachment()
	e.Mock.AddMessage("retained", raw, []string{"INBOX"})
	_, err := New(e.Mock, e.Store, opts).Full(t.Context(), testEmail)
	require.NoError(err)
	source := e.CreateSource(t)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"retained"})
	require.NoError(err)
	require.Contains(ids, "retained")
	delete(e.Mock.Messages, "retained")
	e.Mock.GetMessageCalls = nil
	sum, err := New(e.Mock, e.Store, opts).Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Equal(int64(1), sum.Errors, "one failed local repair must be reported even when Gmail no longer lists the message")
	run, err := e.Store.GetLatestSyncContext(t.Context(), source.ID, 0)
	require.NoError(err)
	assert.Equal(int64(1), run.ErrorsCount, "the stored run must count the failed local repair")
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	require.Len(debt, 1)
	require.NoError(os.Remove(dir))
	reopenAcquisitionStore(t, e)
	sum, err = New(e.Mock, e.Store, opts).Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Zero(sum.Errors)
	assert.Zero(sum.MessagesAdded, "repairing an existing archive row is not a new message")
	assert.Empty(e.Mock.GetMessageCalls, "retained RAW is sufficient even after provider deletion")
	refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), ids["retained"])
	require.NoError(err)
	require.Len(refs, 1)
	path := filepath.Join(dir, refs[0].StoragePath)
	saved, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal([]byte("Hello World!"), saved)
	before, err := os.Stat(path)
	require.NoError(err)
	_, err = New(e.Mock, e.Store, opts).Full(t.Context(), testEmail)
	require.NoError(err)
	after, err := os.Stat(path)
	require.NoError(err)
	assert.True(os.SameFile(before, after))
	assert.Equal(before.ModTime(), after.ModTime())
	retained, err := e.Store.GetMessageRaw(ids["retained"])
	require.NoError(err)
	assert.Equal(raw, retained)
	debt, err = e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	assert.Empty(debt)
}

func TestAttachmentFullLocalDebtWriteFailureStopsProgress(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	dir := filepath.Join(e.TmpDir, "blocked")
	require.NoError(os.WriteFile(dir, []byte("blocked"), 0600))
	opts := &Options{AttachmentsDir: dir}
	e.Mock.AddMessage("retained", testMIMEWithAttachment(), []string{"INBOX"})
	_, err := New(e.Mock, e.Store, opts).Full(t.Context(), testEmail)
	require.NoError(err)
	source := e.CreateSource(t)
	delete(e.Mock.Messages, "retained")
	e.Mock.Profile.HistoryID = 2000
	allow := acquisitionWriteFault(t, e, "sync_run_items", "INSERT", "NEW.status = 'error'")
	_, err = New(e.Mock, e.Store, opts).Full(t.Context(), testEmail)
	require.ErrorIs(err, errAcquisitionFailureRecord)
	reopenAcquisitionStore(t, e)
	source, err = e.Store.GetSourceByID(source.ID)
	require.NoError(err)
	assert.Equal("1000", source.SyncCursor.String)
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	assert.Len(debt, 1)
	allow()
}

func TestAttachmentCommittedMessagesAreCountedAndDiscovered(t *testing.T) {
	for _, mode := range []string{"full", "history", "label-add", "replay"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			e := attachmentPersistenceEnv(t)
			source := e.CreateSourceWithHistory(t, "1000")
			alias := "saved-alias@example.org"
			require.NoError(e.Store.AddAccountIdentity(source.ID, alias, "manual"))
			raw := testemail.NewMessage().From(alias).To("recipient@example.org").WithAttachment("saved.bin", "application/octet-stream", []byte("saved bytes")).Bytes()
			e.Mock.AddMessage("committed", raw, []string{"SENT"})
			dir := filepath.Join(e.TmpDir, "blocked")
			require.NoError(os.WriteFile(dir, []byte("blocked"), 0600))
			service := New(e.Mock, e.Store, &Options{AttachmentsDir: dir})
			var sum *gmail.SyncSummary
			var err error
			switch mode {
			case "full":
				sum, err = service.Full(t.Context(), testEmail)
			case "history":
				e.SetHistory(1001, historyAdded("committed"))
				sum, err = service.Incremental(t.Context(), source)
			case "label-add":
				e.SetHistory(1001, historyLabelAdded("committed", "SENT"))
				sum, err = service.Incremental(t.Context(), source)
			case "replay":
				runID, startErr := e.Store.StartSync(source.ID, "full")
				require.NoError(startErr)
				require.NoError(e.Store.RecordSyncRunItem(store.SyncRunItem{SyncRunID: runID, SourceMessageID: "committed", Phase: syncItemPhaseFetch, Status: store.SyncRunItemStatusError, ErrorKind: syncItemKindFetchError, ErrorMessage: "earlier fetch failed"}))
				require.NoError(e.Store.FailSync(runID, "synthetic interrupted acquisition"))
				sum, err = service.Incremental(t.Context(), source)
			}
			require.NoError(err)
			assert.Equal(int64(1), sum.MessagesAdded, "the message transaction committed despite the file error")
			assert.Zero(sum.MessagesSkipped)
			assert.Equal(int64(len(raw)), sum.BytesDownloaded)
			assert.Equal(int64(1), sum.Errors)
			ids, err := e.Store.MessageExistsBatch(source.ID, []string{"committed"})
			require.NoError(err)
			require.Contains(ids, "committed")
			retained, err := e.Store.GetMessageRaw(ids["committed"])
			require.NoError(err)
			assert.Equal(raw, retained)
			debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
			require.NoError(err)
			require.Len(debt, 1)
			assert.Equal(syncItemKindAttachmentError, debt[0].ErrorKind)
			identities, err := e.Store.ListAccountIdentities(source.ID)
			require.NoError(err)
			var signals string
			for _, identity := range identities {
				if identity.Address == alias {
					signals = identity.SourceSignal
				}
			}
			assert.Contains(signals, "sent-label", "saved messages participate in identity discovery")
			require.NoError(os.Remove(dir))
			e.Mock.GetMessageCalls = nil
			source, err = e.Store.GetSourceByID(source.ID)
			require.NoError(err)
			sum, err = New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Incremental(t.Context(), source)
			require.NoError(err)
			assert.Zero(sum.Errors)
			assert.Zero(sum.MessagesAdded)
			assert.Empty(e.Mock.GetMessageCalls)
		})
	}
}
