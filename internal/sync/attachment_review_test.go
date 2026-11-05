package sync

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

func TestAttachmentReviewNULContentIDRecovery(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	raw, err := os.ReadFile("../mime/testdata/nul-projections.eml")
	require.NoError(err)
	raw = bytes.ReplaceAll(raw, []byte("Content-Type: application/octet-stream"), []byte("Content-ID: <bad\x00id@example.org>\r\nContent-Type: application/octet-stream"))
	raw = bytes.Replace(raw, []byte("--outer--"), []byte("--outer\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=valid.bin\r\nContent-ID: <valid@example.org>\r\n\r\nvalid\r\n--outer--"), 1)
	parsed, err := mime.Parse(raw)
	require.NoError(err)
	require.Len(parsed.Attachments, 2)
	require.Contains(parsed.Attachments[0].ContentID, "\x00")
	dir := filepath.Join(e.TmpDir, "attachments")
	require.NoError(os.MkdirAll(dir, 0700))
	blocked := filepath.Join(dir, parsed.Attachments[0].ContentHash[:2])
	require.NotEqual(parsed.Attachments[0].ContentHash[:2], parsed.Attachments[1].ContentHash[:2])
	require.NoError(os.WriteFile(blocked, []byte("blocked prefix"), 0600))
	e.Mock.AddMessage("nul-recovery", raw, []string{"INBOX"})
	sum, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Full(t.Context(), testEmail)
	require.NoError(err)
	require.Equal(int64(1), sum.Errors)
	source := e.CreateSource(t)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"nul-recovery"})
	require.NoError(err)
	id := ids["nul-recovery"]
	before, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(before, 1)
	require.Equal("valid@example.org", before[0].ContentID)
	info, err := os.Stat(filepath.Join(dir, before[0].StoragePath))
	require.NoError(err)
	require.NoError(os.Remove(blocked))
	reopenAcquisitionStore(t, e)
	service := New(nil, e.Store, &Options{AttachmentsDir: dir})
	for range 2 {
		require.NoError(service.RepairMessageAttachments(t.Context(), RepairRequest{Reference: "nul-recovery", SourceID: source.ID}))
	}
	refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(refs, 2)
	var valid store.AttachmentRef
	for _, ref := range refs {
		if ref.ContentHash == parsed.Attachments[0].ContentHash {
			assert.Empty(ref.ContentID)
		} else {
			valid = ref
		}
	}
	assert.Equal(before[0], valid)
	after, err := os.Stat(filepath.Join(dir, valid.StoragePath))
	require.NoError(err)
	assert.True(os.SameFile(info, after))
	assert.Equal(info.ModTime(), after.ModTime())
	retained, err := e.Store.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(raw, retained)
}

func TestAttachmentReviewGCRetiresOnlyPurgedDebt(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	if e.Store.IsPostgreSQL() {
		t.Skip("archive GC is SQLite-only")
	}
	dir := filepath.Join(e.TmpDir, "blocked")
	require.NoError(os.WriteFile(dir, []byte("blocked"), 0600))
	for _, id := range []string{"purged", "retained"} {
		e.Mock.AddMessage(id, testMIMEWithAttachment(), []string{"INBOX"})
	}
	_, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Full(t.Context(), testEmail)
	require.NoError(err)
	source := e.CreateSource(t)
	other, err := e.Store.GetOrCreateSource("gmail", "other@example.org")
	require.NoError(err)
	seedRepairRow(t, e.Store, other.ID, "purged", "other-thread", "other")
	runID, err := e.Store.StartSync(other.ID, "full")
	require.NoError(err)
	require.NoError(e.Store.RecordSyncRunItem(store.SyncRunItem{SyncRunID: runID, SourceMessageID: "purged", Phase: syncItemPhaseIngest, Status: store.SyncRunItemStatusError, ErrorKind: syncItemKindAttachmentError, ErrorMessage: "other source still pending"}))
	require.NoError(e.Store.FailSync(runID, "synthetic prior failure"))
	require.NoError(e.Store.MarkMessageDeleted(source.ID, "purged"))
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	require.Len(debt, 2, "a source tombstone alone is not a terminal archive purge")
	plan, err := e.Store.PlanGCContext(t.Context())
	require.NoError(err)
	require.Equal(int64(1), plan.SourceDeleted)
	for _, fault := range []struct{ table, event, condition string }{
		{"sync_run_items", "UPDATE", "NEW.source_message_id = 'purged' AND NEW.status = 'skipped'"},
		{"messages", "DELETE", "OLD.source_message_id = 'purged'"},
	} {
		allow := acquisitionWriteFault(t, e, fault.table, fault.event, fault.condition)
		_, err = e.Store.ExecuteGCContext(t.Context(), plan)
		require.ErrorContains(err, "synthetic acquisition write failure")
		reopenAcquisitionStore(t, e)
		debt, err = e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
		require.NoError(err)
		require.Len(debt, 2, "a rolled-back purge must retain both obligations")
		ids, err := e.Store.MessageExistsBatch(source.ID, []string{"purged"})
		require.NoError(err)
		assert.Contains(ids, "purged")
		allow()
	}
	deleted, err := e.Store.ExecuteGCContext(t.Context(), plan)
	require.NoError(err)
	require.Equal(int64(1), deleted)
	reopenAcquisitionStore(t, e)
	debt, err = e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	require.Len(debt, 1)
	assert.Equal("retained", debt[0].SourceMessageID)
	otherDebt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), other.ID)
	require.NoError(err)
	require.Len(otherDebt, 1)
	assert.Equal("other source still pending", otherDebt[0].ErrorMessage)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"purged", "retained"})
	require.NoError(err)
	assert.NotContains(ids, "purged")
	assert.Contains(ids, "retained")
	require.NoError(os.Remove(dir))
	delete(e.Mock.Messages, "purged")
	e.Mock.GetMessageCalls = nil
	source, err = e.Store.GetSourceByID(source.ID)
	require.NoError(err)
	sum, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Incremental(t.Context(), source)
	require.NoError(err)
	assert.Zero(sum.Errors)
	assert.Empty(e.Mock.GetMessageCalls, "purged work must not trigger an impossible fetch")
	plan, err = e.Store.PlanGCContext(t.Context())
	require.NoError(err)
	deleted, err = e.Store.ExecuteGCContext(t.Context(), plan)
	require.NoError(err)
	assert.Zero(deleted)
}

func TestAttachmentReviewExpectedMIMECountWithRemoteImages(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	source := e.CreateSourceWithHistory(t, "1000")
	raw := revisionHTMLMIME()
	parsed, err := mime.Parse(raw)
	require.NoError(err)
	dir := filepath.Join(e.TmpDir, "attachments")
	require.NoError(os.MkdirAll(dir, 0700))
	blocked := filepath.Join(dir, parsed.Attachments[0].ContentHash[:2])
	require.NoError(os.WriteFile(blocked, []byte("blocked MIME prefix"), 0600))
	fetcher, requests := revisionImageServer(t)
	opts := &Options{AttachmentsDir: dir, RemoteImages: fetcher}
	e.Mock.AddMessage("partial-stats", raw, []string{"INBOX"})
	sum, err := New(e.Mock, e.Store, opts).Full(t.Context(), testEmail)
	require.NoError(err)
	require.Equal(int64(1), sum.Errors)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"partial-stats"})
	require.NoError(err)
	id := ids["partial-stats"]
	for attempt := range 3 {
		var count int
		require.NoError(e.Store.DB().QueryRow(e.Store.Rebind("SELECT attachment_count FROM messages WHERE id = ?"), id).Scan(&count))
		assert.Equal(2, count, "one expected MIME occurrence and one stored remote image, attempt %d", attempt)
		refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
		require.NoError(err)
		assert.Empty(refs, "the expected count must not fabricate a stored MIME row")
		images, err := e.Store.MessageRemoteImages(id)
		require.NoError(err)
		require.Len(images, 1)
		debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
		require.NoError(err)
		require.Len(debt, 1)
		e.Mock.GetMessageCalls = nil
		sum, err = New(e.Mock, e.Store, opts).Incremental(t.Context(), source)
		require.NoError(err)
		assert.Equal(int64(1), sum.Errors)
		assert.Empty(e.Mock.GetMessageCalls)
	}
	require.NoError(os.Remove(blocked))
	sum, err = New(e.Mock, e.Store, opts).Incremental(t.Context(), source)
	require.NoError(err)
	assert.Zero(sum.Errors)
	var count int
	require.NoError(e.Store.DB().QueryRow(e.Store.Rebind("SELECT attachment_count FROM messages WHERE id = ?"), id).Scan(&count))
	assert.Equal(2, count)
	refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	assert.Len(refs, 1)
	assert.Equal(int64(1), requests.Load(), "valid images must be reused on every retry")
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	assert.Empty(debt)
}

type attachmentReviewLabelClient struct {
	*gmail.Client

	short bool
}

func (c *attachmentReviewLabelClient) GetMessageLabelsBatch(ctx context.Context, ids []string) ([]gmail.MessageLabelsBatchResult, error) {
	labels, err := c.Client.GetMessageLabelsBatch(ctx, ids)
	for _, id := range ids {
		if c.short && id == "trash" {
			return nil, err
		}
	}
	return labels, err
}

func TestAttachmentReviewRecoveryRefreshesLabelsDespiteDebt(t *testing.T) {
	for _, kind := range []string{syncItemKindAttachmentError, syncItemKindRemoteImageError} {
		for _, fault := range []string{"", "write", "cancel", "short"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				e, f, ids := recoveryLabelsEnv(t)
				api := e.Syncer.client
				client, ok := api.(*gmail.Client)
				require.True(ok)
				short := &attachmentReviewLabelClient{Client: client, short: fault == "short"}
				api = short
				source := gmailCoverageSource(t, e)
				if kind == syncItemKindRemoteImageError {
					raw := revisionHTMLMIME()
					parsed, err := mime.Parse(raw)
					require.NoError(err)
					require.NoError(e.Store.UpsertMessageRaw(ids["trash"], raw))
					msg, err := e.Store.GetMessage(ids["trash"])
					require.NoError(err)
					msg.BodyHTML = parsed.BodyHTML
					// The replay reads the already archived HTML; image downloads are now disabled.
					require.NoError(e.Store.UpsertMessageBody(ids["trash"], sql.NullString{String: msg.BodyText, Valid: msg.BodyText != ""}, sql.NullString{String: msg.BodyHTML, Valid: true}))
				}
				runID, err := e.Store.StartSync(source.ID, "full")
				require.NoError(err)
				require.NoError(e.Store.RecordSyncRunItem(store.SyncRunItem{SyncRunID: runID, SourceMessageID: "trash", Phase: syncItemPhaseIngest, Status: store.SyncRunItemStatusError, ErrorKind: kind, ErrorMessage: "synthetic pending files"}))
				require.NoError(e.Store.FailSync(runID, "synthetic earlier interruption"))
				dir := filepath.Join(t.TempDir(), "blocked")
				require.NoError(os.WriteFile(dir, []byte("blocked"), 0600))
				opts := &Options{AttachmentsDir: dir}
				e.Syncer = New(api, e.Store, opts)
				removeFault := func() {}
				if fault == "write" {
					removeFault = acquisitionWriteFault(t, e, "message_labels", "INSERT", fmt.Sprintf("NEW.message_id = %d", ids["trash"]))
				} else {
					f.fault = fault
				}
				for range 2 {
					ctx, cancel := context.WithCancel(t.Context())
					f.cancel = cancel
					summary, err := e.Syncer.IncrementalWithHistoryRecovery(ctx, gmailCoverageSource(t, e), nil)
					cancel()
					switch fault {
					case "":
						require.NoError(err)
						assert.Positive(summary.Errors)
					case "cancel":
						require.ErrorIs(err, context.Canceled)
					case "short":
						require.ErrorContains(err, "label metadata missing from batch response")
					default:
						require.ErrorContains(err, "synthetic acquisition write failure")
					}
					reopenAcquisitionStore(t, e)
					e.Syncer = New(api, e.Store, opts)
					labels, err := e.Store.MessageLabelIDsContext(t.Context(), ids["trash"])
					require.NoError(err)
					want := "INBOX"
					if fault != "" {
						want = "TRASH"
					}
					labelID, err := e.Store.LabelIDContext(t.Context(), source.ID, want)
					require.NoError(err)
					assert.Equal([]int64{labelID}, labels)
					if fault != "" {
						assert.Equal("1000", gmailCoverageSource(t, e).SyncCursor.String)
						prior, err := e.Store.GetLatestCheckpointedSync(source.ID)
						require.NoError(err)
						cp, err := decodeRecoveryCheckpoint(prior.CursorBefore.String)
						require.NoError(err)
						assert.Equal(recoveryEnumerate, cp.Phase)
						assert.Equal("p2", cp.PageToken)
						assert.Equal("2000", prior.CursorAfter.String)
					}
					debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
					require.NoError(err)
					assert.Len(debt, 1)
				}
				removeFault()
				f.fault = ""
				short.short = false
				_, err = e.Syncer.IncrementalWithHistoryRecovery(t.Context(), gmailCoverageSource(t, e), nil)
				require.NoError(err)
				labelID, err := e.Store.LabelIDContext(t.Context(), source.ID, "INBOX")
				require.NoError(err)
				labels, err := e.Store.MessageLabelIDsContext(t.Context(), ids["trash"])
				require.NoError(err)
				assert.Equal([]int64{labelID}, labels)
				assert.Equal("2001", gmailCoverageSource(t, e).SyncCursor.String)
				assert.Equal(4, f.rawGets, "recovery must not reacquire retained messages")
			})
		}
	}
}
