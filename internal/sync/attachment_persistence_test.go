package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func attachmentPersistenceEnv(t *testing.T) *TestEnv {
	t.Helper()
	st := testutil.NewTestStore(t)
	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{EmailAddress: testEmail, HistoryID: 1000}
	return &TestEnv{Store: st, Mock: mock, TmpDir: t.TempDir(), Context: t.Context()}
}

func TestAttachmentPersistenceFailureAndFullRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	e := attachmentPersistenceEnv(t)
	e.Mock.AddMessage("attachment-failure", testMIMEWithAttachment(), []string{"INBOX"})
	blocked := filepath.Join(e.TmpDir, "blocked")
	require.NoError(os.WriteFile(blocked, []byte("synthetic non-directory"), 0600))
	e.Syncer = New(e.Mock, e.Store, &Options{AttachmentsDir: blocked})
	sum, err := e.Syncer.Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Equal(int64(1), sum.Errors, "materialization failure must reach the sync result")
	source := e.CreateSource(t)
	debt, err := e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	assert.Len(debt, 1)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"attachment-failure"})
	require.NoError(err)
	id := ids["attachment-failure"]
	raw, err := e.Store.GetMessageRaw(id)
	require.NoError(err)
	assert.Equal(testMIMEWithAttachment(), raw)
	var expected int
	require.NoError(e.Store.DB().QueryRow(e.Store.Rebind(`SELECT attachment_count FROM messages WHERE id = ?`), id).Scan(&expected))
	assert.Equal(1, expected, "failed writes must not erase the expected count")
	// Repeated failures, including disabled materialization, remain retryable
	// across completed incremental runs on an otherwise idle mailbox.
	for _, path := range []string{blocked, ""} {
		e.Syncer = New(e.Mock, e.Store, &Options{AttachmentsDir: path})
		sum, err = e.Syncer.Incremental(t.Context(), source)
		require.NoError(err)
		assert.Equal(int64(1), sum.Errors)
		debt, err = e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
		require.NoError(err)
		assert.Len(debt, 1)
		source, err = e.Store.GetSourceByID(source.ID)
		require.NoError(err)
	}

	dir := filepath.Join(e.TmpDir, "attachments")
	e.Syncer = New(e.Mock, e.Store, &Options{AttachmentsDir: dir})
	sum, err = e.Syncer.Full(t.Context(), testEmail)
	require.NoError(err)
	assert.Zero(sum.Errors)
	assert.Equal([]string{"attachment-failure"}, e.Mock.GetMessageCalls, "retry must use retained MIME")
	refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(refs, 1)
	data, err := os.ReadFile(filepath.Join(dir, refs[0].StoragePath))
	require.NoError(err)
	assert.Equal([]byte("Hello World!"), data)
	debt, err = e.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(err)
	assert.Empty(debt)
}

func TestAttachmentRepairPreservesUnusableEvidence(t *testing.T) {
	for _, mode := range []string{"missing-raw", "empty-raw", "undecodable-raw", "unparseable-raw", "insufficient-raw", "empty-file", "truncated-file", "corrupt-file", "metadata-conflict", "intentional-skip"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			e := attachmentPersistenceEnv(t)
			dir := filepath.Join(e.TmpDir, "attachments")
			e.Mock.AddMessage("retained", testMIMEWithAttachment(), []string{"INBOX"})
			e.Syncer = New(e.Mock, e.Store, &Options{AttachmentsDir: dir})
			_, err := e.Syncer.Full(t.Context(), testEmail)
			require.NoError(err)
			source := e.CreateSource(t)
			ids, err := e.Store.MessageExistsBatch(source.ID, []string{"retained"})
			require.NoError(err)
			id := ids["retained"]
			refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(err)
			require.Len(refs, 1)
			path := filepath.Join(dir, refs[0].StoragePath)
			switch mode {
			case "missing-raw":
				_, err = e.Store.DB().Exec(e.Store.Rebind(`DELETE FROM message_raw WHERE message_id = ?`), id)
			case "empty-raw":
				err = e.Store.UpsertMessageRaw(id, []byte{})
			case "undecodable-raw":
				_, err = e.Store.DB().Exec(e.Store.Rebind(`UPDATE message_raw SET raw_data = ? WHERE message_id = ?`), []byte("invalid zlib"), id)
			case "unparseable-raw":
				err = e.Store.UpsertMessageRaw(id, []byte("not a MIME message\x00"))
			case "insufficient-raw":
				err = e.Store.UpsertMessageRaw(id, testMIME())
			case "empty-file":
				err = os.WriteFile(path, []byte{}, 0600)
			case "truncated-file":
				err = os.WriteFile(path, []byte("Hello"), 0600)
			case "corrupt-file":
				err = os.WriteFile(path, []byte("Goodbye!!!!!"), 0600)
			case "metadata-conflict":
				_, err = e.Store.DB().Exec(e.Store.Rebind(`UPDATE attachments SET content_hash = ? WHERE message_id = ?`), strings.Repeat("a", 64), id)
			case "intentional-skip":
				_, err = e.Store.DB().Exec(e.Store.Rebind(`UPDATE attachments SET attachment_state = 'skipped' WHERE message_id = ?`), id)
			}
			require.NoError(err)
			before := attachmentSnapshot(t, e.Store, id)
			bytesBefore, err := os.ReadFile(path)
			require.NoError(err)
			service := New(nil, e.Store, &Options{AttachmentsDir: dir})
			for range 2 {
				require.Error(service.RepairMessageAttachments(t.Context(), RepairRequest{Reference: "retained", SourceID: source.ID}))
			}
			assert.Equal(before, attachmentSnapshot(t, e.Store, id))
			bytesAfter, err := os.ReadFile(path)
			require.NoError(err)
			assert.Equal(bytesBefore, bytesAfter)
		})
	}
}

func TestAttachmentRepairOccurrencesAndMissingFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	e := attachmentPersistenceEnv(t)
	raw := testemail.NewMessage().WithAttachment("one.bin", "application/octet-stream", []byte("same bytes")).
		WithAttachment("two.bin", "application/octet-stream", []byte("same bytes")).
		WithAttachment("inline.bin", "application/octet-stream", []byte("inline bytes")).
		WithAttachment("empty.bin", "application/octet-stream", nil).Bytes()
	raw = []byte(strings.Replace(string(raw), `attachment; filename="inline.bin"`, "inline; filename=\"inline.bin\"\r\nContent-ID: <synthetic-inline>", 1))
	parsed, err := mime.Parse(raw)
	require.NoError(err)
	require.Len(parsed.Attachments, 4)
	e.Mock.AddMessage("occurrences", raw, []string{"INBOX"})
	dir := filepath.Join(e.TmpDir, "attachments")
	e.Syncer = New(e.Mock, e.Store, &Options{AttachmentsDir: dir})
	_, err = e.Syncer.Full(t.Context(), testEmail)
	require.NoError(err)
	source := e.CreateSource(t)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"occurrences"})
	require.NoError(err)
	id := ids["occurrences"]
	refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(refs, 3)
	require.Equal(refs[0].ContentHash, refs[1].ContentHash)
	assert.Equal(store.AttachmentRoleInline, refs[2].Role)
	validPath := filepath.Join(dir, refs[0].StoragePath)
	valid, err := os.Stat(validPath)
	require.NoError(err)
	require.NoError(os.Remove(filepath.Join(dir, refs[2].StoragePath)))
	require.NoError(e.Store.UpsertAttachmentRecord(t.Context(), id, store.AttachmentWrite{
		SourceAttachmentID: "provider:keep", Filename: "provider.bin", StoragePath: "provider.bin", ContentHash: "provider-hash", Size: 12,
	}))
	require.NoError(e.Store.RecomputeMessageAttachmentStats(id))
	before := attachmentSnapshot(t, e.Store, id)
	service := New(nil, e.Store, &Options{AttachmentsDir: dir})
	for range 2 {
		require.NoError(service.RepairMessageAttachments(t.Context(), RepairRequest{Reference: "occurrences", SourceID: source.ID}))
	}
	assert.Equal(before, attachmentSnapshot(t, e.Store, id))
	after, err := os.Stat(validPath)
	require.NoError(err)
	assert.True(os.SameFile(valid, after))
	assert.Equal(valid.ModTime(), after.ModTime())
	restored, err := os.ReadFile(filepath.Join(dir, refs[2].StoragePath))
	require.NoError(err)
	assert.Equal([]byte("inline bytes"), restored)
}

func TestAttachmentPersistenceOfflineReplay(t *testing.T) {
	for _, materialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "valid"}[materialized], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			e := attachmentPersistenceEnv(t)
			dir := filepath.Join(e.TmpDir, "attachments")
			opts := &Options{}
			if materialized {
				opts.AttachmentsDir = dir
			}
			e.Syncer = New(e.Mock, e.Store, opts)
			e.Mock.AddMessage("offline-attachment", testMIMEWithAttachment(), []string{"INBOX"})
			_, err := e.Syncer.Full(t.Context(), testEmail)
			require.NoError(err)
			source := e.CreateSource(t)
			ids, err := e.Store.MessageExistsBatch(source.ID, []string{"offline-attachment"})
			require.NoError(err)
			id := ids["offline-attachment"]
			before := attachmentSnapshot(t, e.Store, id)
			var info os.FileInfo
			if materialized {
				refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
				require.NoError(err)
				require.Len(refs, 1)
				info, err = os.Stat(filepath.Join(dir, refs[0].StoragePath))
				require.NoError(err)
			}
			// No provider client exists on this production replay path.
			service := New(nil, e.Store, &Options{AttachmentsDir: dir})
			for range 2 {
				runID, err := e.Store.StartSync(source.ID, "incremental")
				require.NoError(err)
				service.setAcquisitionReplay(runID, []string{"offline-attachment"})
				require.NoError(service.recordAcquisitionFailure(runID, "offline-attachment", syncItemPhaseIngest, syncItemKindIngestError, errAttachmentMaterialization))
				_, err = service.replayAcquisitionFailures(context.Background(), runID, source.ID,
					[]string{"offline-attachment"}, nil, &store.Checkpoint{}, &gmail.SyncSummary{})
				require.NoError(err)
				require.NoError(e.Store.CompleteSync(runID, ""))
			}
			refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
			require.NoError(err)
			require.Len(refs, 1)
			data, err := os.ReadFile(filepath.Join(dir, refs[0].StoragePath))
			require.NoError(err)
			assert.Equal([]byte("Hello World!"), data)
			if materialized {
				after, err := os.Stat(filepath.Join(dir, refs[0].StoragePath))
				require.NoError(err)
				assert.True(os.SameFile(info, after))
				assert.Equal(info.ModTime(), after.ModTime())
				assert.Equal(before, attachmentSnapshot(t, e.Store, id))
			}
		})
	}
}

func attachmentSnapshot(t *testing.T, st *store.Store, id int64) []string {
	t.Helper()
	require := require.New(t)
	var snapshot []string
	for _, table := range []string{"messages", "message_bodies", "message_raw", "message_recipients", "message_labels", "attachments"} {
		key := "message_id"
		if table == "messages" {
			key = "id"
		}
		rows, err := st.DB().Query(st.Rebind("SELECT * FROM "+table+" WHERE "+key+" = ?"), id)
		require.NoError(err)
		defer func() { require.NoError(rows.Close()) }()
		columns, err := rows.Columns()
		require.NoError(err)
		for rows.Next() {
			values := make([]any, len(columns))
			args := make([]any, len(columns))
			for i := range values {
				args[i] = &values[i]
			}
			require.NoError(rows.Scan(args...))
			snapshot = append(snapshot, table+fmt.Sprint(values))
		}
		require.NoError(rows.Err())
		require.NoError(rows.Close())
	}
	return snapshot
}

func TestAttachmentRepairProviderCollisionRollsBack(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	e := attachmentPersistenceEnv(t)
	raw := testemail.NewMessage().WithAttachment("one.bin", "application/octet-stream", []byte("one")).
		WithAttachment("two.bin", "application/octet-stream", []byte("two")).Bytes()
	e.Mock.AddMessage("collision", raw, []string{"INBOX"})
	_, err := New(e.Mock, e.Store, &Options{}).Full(t.Context(), testEmail)
	require.NoError(err)
	source := e.CreateSource(t)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"collision"})
	require.NoError(err)
	id := ids["collision"]
	parsed, err := mime.Parse(raw)
	require.NoError(err)
	require.Len(parsed.Attachments, 2)
	part := parsed.Attachments[1]
	require.NoError(e.Store.UpsertAttachmentRecord(t.Context(), id, store.AttachmentWrite{
		SourceAttachmentID: "provider:protected", SourcePartKey: part.PartKey, Filename: "provider.bin",
		ContentHash: part.ContentHash, StoragePath: "provider.bin", Size: int64(len(part.Content)),
	}))
	before := attachmentSnapshot(t, e.Store, id)
	dir := filepath.Join(e.TmpDir, "attachments")
	service := New(nil, e.Store, &Options{AttachmentsDir: dir})
	require.ErrorContains(service.RepairMessageAttachments(t.Context(), RepairRequest{Reference: "collision", SourceID: source.ID}), `conflicting metadata for attachment "two.bin"`)
	assert.Equal(before, attachmentSnapshot(t, e.Store, id))
	assert.NoFileExists(filepath.Join(dir, parsed.Attachments[0].ContentHash[:2], parsed.Attachments[0].ContentHash))
}

func TestAttachmentRepairLegacyMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	e := attachmentPersistenceEnv(t)
	dir := filepath.Join(e.TmpDir, "attachments")
	e.Mock.AddMessage("legacy", testMIMEWithAttachment(), []string{"INBOX"})
	_, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Full(t.Context(), testEmail)
	require.NoError(err)
	source := e.CreateSource(t)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"legacy"})
	require.NoError(err)
	id := ids["legacy"]
	_, err = e.Store.DB().Exec(e.Store.Rebind(`UPDATE attachments SET source_part_key = NULL, filename = 'retained-name.bin' WHERE message_id = ?`), id)
	require.NoError(err)
	var rowID int64
	require.NoError(e.Store.DB().QueryRow(e.Store.Rebind(`SELECT id FROM attachments WHERE message_id = ?`), id).Scan(&rowID))
	require.NoError(New(nil, e.Store, &Options{AttachmentsDir: dir}).RepairMessageAttachments(t.Context(), RepairRequest{Reference: "legacy", SourceID: source.ID}))
	var gotID int64
	var filename, key string
	require.NoError(e.Store.DB().QueryRow(e.Store.Rebind(`SELECT id, filename, source_part_key FROM attachments WHERE message_id = ?`), id).Scan(&gotID, &filename, &key))
	assert.Equal(rowID, gotID)
	assert.Equal("retained-name.bin", filename)
	assert.NotEmpty(key)
}

func TestAttachmentRepairPreservesPackedBlob(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	e := attachmentPersistenceEnv(t)
	dir := filepath.Join(e.TmpDir, "attachments")
	e.Mock.AddMessage("packed", testMIMEWithAttachment(), []string{"INBOX"})
	_, err := New(e.Mock, e.Store, &Options{AttachmentsDir: dir}).Full(t.Context(), testEmail)
	require.NoError(err)
	source := e.CreateSource(t)
	ids, err := e.Store.MessageExistsBatch(source.ID, []string{"packed"})
	require.NoError(err)
	id := ids["packed"]
	refs, err := e.Store.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(refs, 1)
	layout, err := packstore.NewLayout(dir, packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
	require.NoError(err)
	maintainer, err := packstore.NewMaintainer(store.NewMaintenancePackCatalog(e.Store, dir), layout, packstore.MaintainerOptions{})
	require.NoError(err)
	defer func() { require.NoError(maintainer.Close()) }()
	stats, err := maintainer.Pack(t.Context(), packstore.PackOptions{})
	require.NoError(err)
	require.Equal(1, stats.BlobsPacked)
	before := attachmentSnapshot(t, e.Store, id)
	path := filepath.Join(dir, refs[0].StoragePath)
	require.NoFileExists(path, "a packed blob legitimately has no loose file")
	require.NoError(New(nil, e.Store, &Options{AttachmentsDir: dir}).RepairMessageAttachments(t.Context(), RepairRequest{Reference: "packed", SourceID: source.ID}))
	assert.NoFileExists(path, "a valid pack must not be needlessly unpacked")
	assert.Equal(before, attachmentSnapshot(t, e.Store, id))
}
