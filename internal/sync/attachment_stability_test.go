package sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestAttachmentStabilityIMAPPartialWrite(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	raw := testemail.NewMessage().WithAttachment("blocked.txt", "text/plain", []byte("blocked attachment")).
		WithAttachment("saved.txt", "text/plain", []byte("saved attachment")).CRLF().Bytes()
	parsed, err := mime.Parse(raw)
	require.NoError(err)
	require.Len(parsed.Attachments, 2)
	dir := filepath.Join(e.TmpDir, "attachments")
	require.NoError(os.MkdirAll(dir, 0700))
	require.NotEqual(parsed.Attachments[0].ContentHash[:2], parsed.Attachments[1].ContentHash[:2])
	require.NoError(os.WriteFile(filepath.Join(dir, parsed.Attachments[0].ContentHash[:2]), []byte("blocked"), 0600))
	addr, user := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 0})
	testutil.AppendIMAPRawMessage(t, user, "INBOX", raw)
	client := newSyncTestIMAPClient(t, addr)
	summary, err := New(client, e.Store, &Options{SourceType: sourceTypeIMAP, AttachmentsDir: dir}).Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Equal(int64(1), summary.MessagesAdded, "the committed IMAP message still counts as saved")
	assert.Zero(summary.MessagesSkipped)
	assert.Zero(summary.Errors, "preserve IMAP's best-effort attachment contract")
	var id, sourceID int64
	var count int
	require.NoError(e.Store.DB().QueryRowContext(t.Context(), `SELECT id, source_id, attachment_count FROM messages`).Scan(&id, &sourceID, &count))
	assert.Equal(1, count, "IMAP statistics reflect the successfully saved subset")
	refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(refs, 1)
	assert.Equal("saved.txt", refs[0].Filename)
	assert.FileExists(filepath.Join(dir, refs[0].StoragePath))
	saved, err := e.Store.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(raw, saved)
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), sourceID)
	require.NoError(err)
	assert.Empty(debt, "this change must not introduce Gmail repair obligations for IMAP")
}

func TestAttachmentStabilityDirectorySyncRetry(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	dir := filepath.Join(e.TmpDir, "attachments")
	e.Mock.AddMessage("directory-sync", testMIMEWithAttachment(), []string{"INBOX"})
	_, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Full(t.Context(), testEmail)
	require.NoError(err)
	source := e.CreateSource(t)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"directory-sync"})
	require.NoError(err)
	id := ids["directory-sync"]
	before := attachmentSnapshot(t, e.Store, id)
	refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(refs, 1)
	path := filepath.Join(dir, refs[0].StoragePath)
	require.NoError(os.Remove(path))
	runID, err := e.Store.StartSync(source.ID, "incremental")
	require.NoError(err)
	require.NoError(e.Store.RecordSyncRunItem(store.SyncRunItem{SyncRunID: runID, SourceMessageID: "directory-sync",
		Phase: syncItemPhaseIngest, Status: store.SyncRunItemStatusError, ErrorKind: syncItemKindAttachmentError, ErrorMessage: "file missing"}))
	require.NoError(e.Store.FailSync(runID, "synthetic missing attachment"))
	resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	require.NoError(err)
	originalSyncDir := pack.SyncDir
	syncErr := errors.New("synthetic attachment directory sync failure")
	calls := 0
	pack.SyncDir = func(dir string) error {
		if filepath.Clean(dir) == resolvedDir {
			calls++
			return syncErr
		}
		return originalSyncDir(dir)
	}
	t.Cleanup(func() { pack.SyncDir = originalSyncDir })
	e.Mock.GetMessageCalls = nil
	var residue os.FileInfo
	for attempt := range 2 {
		reopenAcquisitionStore(t, e)
		summary, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Full(t.Context(), testEmail)
		require.NoError(err)
		assert.Equal(int64(1), summary.Errors, "attempt %d must not acknowledge an unsynced publication", attempt)
		reopenAcquisitionStore(t, e)
		debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
		require.NoError(err)
		require.Len(debt, 1, "unfinished directory sync must survive reopening the archive")
		assert.Contains(debt[0].ErrorMessage, syncErr.Error())
		assert.Equal(before, attachmentSnapshot(t, e.Store, id))
		info, err := os.Stat(path)
		require.NoError(err, "referenced residue must be preserved")
		if residue != nil {
			assert.True(os.SameFile(residue, info))
			assert.Equal(residue.ModTime(), info.ModTime())
		}
		residue = info
	}
	assert.GreaterOrEqual(calls, 2, "a retry must repeat the failed directory sync")
	pack.SyncDir = originalSyncDir
	summary, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Zero(summary.Errors)
	reopenAcquisitionStore(t, e)
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	assert.Empty(debt)
	assert.Empty(e.Mock.GetMessageCalls, "repair must not download the saved Gmail message")
	assert.Equal(before, attachmentSnapshot(t, e.Store, id))
	info, err := os.Stat(path)
	require.NoError(err)
	assert.True(os.SameFile(residue, info))
	assert.Equal(residue.ModTime(), info.ModTime())
}

func TestAttachmentStabilityCaseEquivalentLegacyRows(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	e := attachmentPersistenceEnv(t)
	content := []byte("identical legacy attachment bytes")
	raw := testemail.NewMessage().WithAttachment("first.txt", "text/plain", content).
		WithAttachment("second.txt", "text/plain", content).Bytes()
	e.Mock.AddMessage("legacy-casing", raw, []string{"INBOX"})
	_, err := New(e.Mock, e.Store, &Options{}).Full(t.Context(), testEmail)
	require.NoError(err)
	source := e.CreateSource(t)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"legacy-casing"})
	require.NoError(err)
	id := ids["legacy-casing"]
	parsed, err := mime.Parse(raw)
	require.NoError(err)
	require.Len(parsed.Attachments, 2)
	dir := filepath.Join(e.TmpDir, "attachments")
	receipt, err := export.StoreAttachmentFileDurable(t.Context(), dir, &parsed.Attachments[0])
	require.NoError(err)
	hash := parsed.Attachments[0].ContentHash
	legacyIDs := make([]int64, 3)
	for i, legacyHash := range []string{hash, strings.ToUpper(hash), strings.ToUpper(hash[:32]) + hash[32:]} {
		require.NoError(e.Store.UpsertAttachmentRecord(t.Context(), id, store.AttachmentWrite{
			Filename: fmt.Sprintf("legacy-%d.txt", i), MIMEType: "application/octet-stream", StoragePath: receipt.StoragePath,
			ContentHash: legacyHash, Size: int64(len(content)), ContentID: fmt.Sprintf("legacy-%d@example.com", i),
			Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceMIMEDisposition,
		}))
		require.NoError(e.Store.DB().QueryRowContext(t.Context(), e.Store.Rebind(`SELECT id FROM attachments WHERE message_id = ? AND filename = ?`),
			id, fmt.Sprintf("legacy-%d.txt", i)).Scan(&legacyIDs[i]))
	}
	require.NoError(e.Store.RecomputeMessageAttachmentStats(id))
	before, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(before, 3)
	path := filepath.Join(dir, receipt.StoragePath)
	info, err := os.Stat(path)
	require.NoError(err)
	service := New(nil, e.Store, &Options{AttachmentsDir: dir})
	for range 2 {
		require.NoError(service.RepairMessageAttachments(t.Context(), RepairRequest{Reference: "legacy-casing", SourceID: source.ID}))
		after, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
		require.NoError(err)
		require.Len(after, 3)
		for i := range after {
			var rowID int64
			require.NoError(e.Store.DB().QueryRowContext(t.Context(), e.Store.Rebind(`SELECT id FROM attachments WHERE message_id = ? AND filename = ?`),
				id, after[i].Filename).Scan(&rowID))
			assert.Equal(legacyIDs[i], rowID)
			if i < 2 {
				assert.Equal(parsed.Attachments[i].PartKey, after[i].SourcePartKey)
			} else {
				assert.Empty(after[i].SourcePartKey, "the surplus historical row is untouched")
			}
			after[i].SourcePartKey = ""
		}
		assert.Equal(before, after, "all existing metadata, including hash casing, must be preserved")
	}
	after, err := os.Stat(path)
	require.NoError(err)
	assert.True(os.SameFile(info, after))
	assert.Equal(info.ModTime(), after.ModTime())
}
