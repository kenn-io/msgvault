package sync

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestAttachmentReviewFTSOverflowCommitsOriginal(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	if !e.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL tsvector capacity")
	}
	// Each field fits the production caps and an individual tsvector. Their
	// distinct lexemes overflow PostgreSQL's combined vector during ingestion.
	var subject, body strings.Builder
	for i := range 45000 {
		fmt.Fprintf(&subject, "subject%05d ", i)
		fmt.Fprintf(&body, "body%05d ", i)
	}
	var vector string
	err := e.Store.DB().QueryRowContext(t.Context(), `SELECT to_tsvector('simple', $1) || to_tsvector('simple', $2)`, subject.String(), body.String()).Scan(&vector)
	var overflow *pgconn.PgError
	require.ErrorAs(err, &overflow)
	require.Equal("54000", overflow.Code)
	raw := testemail.NewMessage().Subject(subject.String()).Body(body.String()).Bytes()
	e.Mock.AddMessage("fts-overflow", raw, []string{"INBOX"})
	summary, err := New(e.Mock, e.Store, &Options{}).Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded)
	assert.Zero(summary.Errors)
	source := e.CreateSource(t)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"fts-overflow"})
	require.NoError(err)
	require.Contains(ids, "fts-overflow")
	reopenAcquisitionStore(t, e)
	saved, err := e.Store.GetMessageRaw(ids["fts-overflow"])
	require.NoError(err)
	assert.True(bytes.Equal(raw, saved), "overflow must not discard or alter RAW")
	var terminal bool
	require.NoError(e.Store.DB().QueryRowContext(t.Context(), `SELECT search_fts = ''::tsvector AND indexing_version = $1 FROM messages WHERE id = $2`,
		store.CurrentFTSIndexingVersion, ids["fts-overflow"]).Scan(&terminal))
	assert.True(terminal)
	needs := e.Store.NeedsFTSBackfill()
	assert.False(needs, "terminal overflow must not request another indexing attempt")
	results, total, err := e.Store.SearchMessages("subject00001", 0, 10)
	require.NoError(err)
	assert.Zero(total)
	assert.Empty(results)
	e.Mock.GetMessageCalls = nil
	summary, err = New(e.Mock, e.Store, &Options{}).Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.Zero(summary.Errors)
	assert.Empty(e.Mock.GetMessageCalls)
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	assert.Empty(debt, "terminal FTS overflow is not unfinished acquisition")
}

func TestAttachmentReviewOtherFTSFailureRemainsVisible(t *testing.T) {
	for _, route := range []string{"full", "incremental"} {
		t.Run(route, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			e := attachmentPersistenceEnv(t)
			if !e.Store.IsPostgreSQL() {
				t.Skip("PostgreSQL FTS transaction failure")
			}
			source := e.CreateSourceWithHistory(t, "1000")
			e.Mock.AddMessage("fts-failure", testMIME(), []string{"INBOX"})
			allow := acquisitionWriteFault(t, e, "messages", "UPDATE OF search_fts", "NEW.search_fts IS NOT NULL")
			service := New(e.Mock, e.Store, &Options{})
			var summaryErrors int64
			if route == "full" {
				summary, err := service.Full(t.Context(), testEmail)
				require.NoError(err)
				summaryErrors = summary.Errors
			} else {
				e.SetHistory(1001, historyAdded("fts-failure"))
				summary, err := service.Incremental(t.Context(), source)
				require.NoError(err)
				summaryErrors = summary.Errors
			}
			assert.Equal(int64(1), summaryErrors)
			ids, err := e.Store.MessageExistsBatch(source.ID, []string{"fts-failure"})
			require.NoError(err)
			assert.Empty(ids)
			debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
			require.NoError(err)
			require.Len(debt, 1)
			assert.Contains(debt[0].ErrorMessage, "synthetic acquisition write failure")
			allow()
		})
	}
}

func TestAttachmentReviewExpectedMIMECountWithRemoteImages(t *testing.T) {
	for _, kind := range []string{"mime", "mime-and-images", "no-mime-debt"} {
		t.Run(kind, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			e := attachmentPersistenceEnv(t)
			source := e.CreateSourceWithHistory(t, "1000")
			raw := revisionHTMLMIME()
			parsed, err := mime.Parse(raw)
			require.NoError(err)
			dir := filepath.Join(e.TmpDir, "attachments")
			opts := &Options{}
			pending := kind != "no-mime-debt"
			blocked := filepath.Join(dir, parsed.Attachments[0].ContentHash[:2])
			if pending {
				require.NoError(os.MkdirAll(dir, 0700))
				require.NoError(os.WriteFile(blocked, []byte("blocked MIME prefix"), 0600))
				opts.AttachmentsDir = dir
			}
			fetcher, requests := revisionImageServer(t)
			if kind == "mime-and-images" {
				opts.RemoteImages = fetcher
			}
			e.Mock.AddMessage("backfill-debt", raw, []string{"INBOX"})
			summary, err := New(e.Mock, e.Store, opts).Full(t.Context(), testEmail)
			require.NoError(err)
			if pending {
				require.Equal(int64(1), summary.Errors)
			} else {
				require.Zero(summary.Errors)
			}
			ids, err := e.Store.MessageExistsBatch(source.ID, []string{"backfill-debt"})
			require.NoError(err)
			id := ids["backfill-debt"]
			wantCount := 1
			if pending {
				wantCount = 2
			}
			for range 2 {
				result, err := fetcher.Backfill(t.Context(), e.Store, dir, source.ID, 0, slog.Default())
				require.NoError(err)
				assert.Zero(result.Errors)
				assert.Equal(1, result.Downloaded+result.Reused)
				var count int
				require.NoError(e.Store.DB().QueryRowContext(t.Context(), e.Store.Rebind(`SELECT attachment_count FROM messages WHERE id = ?`), id).Scan(&count))
				assert.Equal(wantCount, count, "preserve only MIME occurrences backed by active debt")
				refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
				require.NoError(err)
				assert.Empty(refs)
			}
			images, err := e.Store.MessageRemoteImages(id)
			require.NoError(err)
			require.Len(images, 1)
			debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
			require.NoError(err)
			if pending {
				require.Len(debt, 1, "backfill must not settle unfinished MIME work")
				e.Mock.GetMessageCalls = nil
				replay := &Options{AttachmentsDir: dir, RemoteImages: fetcher}
				summary, err = New(e.Mock, e.Store, replay).Incremental(t.Context(), source)
				require.NoError(err)
				assert.Equal(int64(1), summary.Errors)
				var count int
				require.NoError(e.Store.DB().QueryRowContext(t.Context(), e.Store.Rebind(`SELECT attachment_count FROM messages WHERE id = ?`), id).Scan(&count))
				assert.Equal(2, count, "a failed repair must retain the expected count after backfill")
				require.NoError(os.Remove(blocked))
				reopenAcquisitionStore(t, e)
				summary, err = New(e.Mock, e.Store, replay).Incremental(t.Context(), source)
				require.NoError(err)
				assert.Zero(summary.Errors)
				assert.Empty(e.Mock.GetMessageCalls)
				require.NoError(e.Store.DB().QueryRowContext(t.Context(), e.Store.Rebind(`SELECT attachment_count FROM messages WHERE id = ?`), id).Scan(&count))
				assert.Equal(2, count)
				refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
				require.NoError(err)
				assert.Len(refs, 1)
				debt, err = e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
				require.NoError(err)
			}
			assert.Empty(debt)
			after, err := e.Store.MessageRemoteImages(id)
			require.NoError(err)
			assert.Equal(images, after)
			assert.Equal(int64(1), requests.Load())
			saved, err := e.Store.GetMessageRaw(id)
			require.NoError(err)
			assert.Equal(raw, saved)
		})
	}
}

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

func TestAttachmentReviewRecoveryRefreshesLabelsDespiteDebt(t *testing.T) {
	for _, kind := range []string{syncItemKindAttachmentError, syncItemKindRemoteImageError} {
		for _, fault := range []string{"", "write", "cancel"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				e, f, ids := recoveryLabelsEnv(t)
				api := e.Syncer.client
				source := gmailCoverageSource(t.Context(), t, e)
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
					summary, err := e.Syncer.IncrementalWithHistoryRecovery(ctx, gmailCoverageSource(t.Context(), t, e), nil)
					cancel()
					switch fault {
					case "":
						require.NoError(err)
						assert.Positive(summary.Errors)
					case "cancel":
						require.ErrorIs(err, context.Canceled)
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
						assert.Equal("1000", gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
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
				_, err = e.Syncer.IncrementalWithHistoryRecovery(t.Context(), gmailCoverageSource(t.Context(), t, e), nil)
				require.NoError(err)
				labelID, err := e.Store.LabelIDContext(t.Context(), source.ID, "INBOX")
				require.NoError(err)
				labels, err := e.Store.MessageLabelIDsContext(t.Context(), ids["trash"])
				require.NoError(err)
				assert.Equal([]int64{labelID}, labels)
				assert.Equal("2001", gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
				assert.Equal(4, f.rawGets, "recovery must not reacquire retained messages")
			})
		}
	}
}

func TestAttachmentReviewLegacyRecoveryCheckpointRestartsLabels(t *testing.T) {
	for _, phase := range []string{recoveryEnumerate, recoveryCatchup} {
		t.Run(phase, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			e, f, ids := recoveryLabelsEnv(t)
			api := e.Syncer.client
			source := gmailCoverageSource(t.Context(), t, e)
			runID, err := e.Store.StartSync(source.ID, "full")
			require.NoError(err)
			_, err = e.Store.DB().ExecContext(t.Context(), e.Store.Rebind("UPDATE sync_runs SET request_fingerprint = ? WHERE id = ?"),
				store.GmailHistoryRecoveryRequestFingerprint+":spam-trash:v1:labels:v1", runID)
			require.NoError(err)
			state := &syncState{recoveryPhase: phase, handoffCursor: "1500", checkpoint: &store.Checkpoint{}}
			if phase == recoveryEnumerate {
				state.checkpoint.PageToken = "p2"
			}
			require.NoError(e.Store.ScopedToSync(source.ID, runID).UpdateSyncCheckpoint(runID, state.durableCheckpoint()))
			require.NoError(e.Store.RecordSyncRunItem(store.SyncRunItem{SyncRunID: runID, SourceMessageID: "normal1", Phase: syncItemPhaseIngest,
				Status: store.SyncRunItemStatusError, ErrorKind: syncItemKindAttachmentError, ErrorMessage: "older recovery skipped labels with these unfinished files"}))
			require.NoError(e.Store.FailSync(runID, "synthetic interruption after skipped labels"))
			reopenAcquisitionStore(t, e)
			dir := filepath.Join(t.TempDir(), "blocked")
			require.NoError(os.WriteFile(dir, []byte("blocked"), 0600))
			f.expire = false
			summary, err := New(api, e.Store, &Options{AttachmentsDir: dir}).Full(t.Context(), testEmail)
			require.NoError(err)
			assert.Positive(summary.Errors)
			reopenAcquisitionStore(t, e)
			labelID, err := e.Store.LabelIDContext(t.Context(), source.ID, "TRASH")
			require.NoError(err)
			labels, err := e.Store.MessageLabelIDsContext(t.Context(), ids["normal1"])
			require.NoError(err)
			assert.Equal([]int64{labelID}, labels, "the skipped first page needs authoritative labels too")
			assert.Equal([]string{"1500"}, f.historyStarts, "retain the old safe History boundary")
			assert.Equal("2001", gmailCoverageSource(t.Context(), t, e).SyncCursor.String)
			assert.Equal(4, f.rawGets)
			debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
			require.NoError(err)
			assert.Len(debt, 1)
		})
	}
}
