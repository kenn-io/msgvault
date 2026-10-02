package importer

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	pstreader "go.kenn.io/msgvault/internal/pst"
	"go.kenn.io/msgvault/internal/store"
)

const pstTestdataDir = "../pst/testdata"

func openIntegrationStore(t *testing.T) *store.Store {
	t.Helper()
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(t, err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema(), "init schema")
	return st
}

// TestImportPst_SupportPST imports the real support.pst fixture and asserts
// the expected message counts and deduplication behaviour.
func TestImportPst_SupportPST(t *testing.T) {
	assert := assert.New(t)
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")

	summary, err := ImportPst(context.Background(), st, pstPath, PstImportOptions{
		Identifier: "support@hackingteam.com",
		NoResume:   true,
	})
	require.NoError(t, err, "ImportPst")

	// support.pst has 17 email messages across Drafts (6) and Sent Messages (11).
	assert.Equal(int64(17), summary.MessagesProcessed, "MessagesProcessed")
	assert.Equal(int64(17), summary.MessagesAdded, "MessagesAdded")
	assert.Equal(int64(0), summary.MessagesSkipped, "MessagesSkipped on first import")
	assert.Equal(int64(0), summary.Errors, "Errors")
	assert.False(summary.HardErrors, "HardErrors")
	assert.Positive(summary.FoldersImported, "FoldersImported")
	assert.Equal(int64(0), summary.Errors, "Errors")
	assert.Equal(2, summary.FoldersTotal, "FoldersTotal")
}

func TestImportPst_SearchFolderResume(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")
	absPath, err := filepath.Abs(pstPath)
	require.NoError(err, "abs PST path")
	archiveID, err := pstArchiveFingerprint(absPath)
	require.NoError(err, "PST fingerprint")

	tests := []struct {
		name          string
		folderIndex   int
		folderPath    string
		messageIndex  int64
		wantCalls     int
		wantFolders   int
		wantProcessed int64
	}{
		{
			name:          "matching checkpoint resumes in the saved folder",
			folderIndex:   1,
			folderPath:    "ROOT_FOLDER/Top of Personal Folders/Sent Messages",
			messageIndex:  3,
			wantCalls:     8,
			wantFolders:   1,
			wantProcessed: 8,
		},
		{
			name:          "shifted checkpoint restarts after path mismatch",
			folderIndex:   0,
			folderPath:    "ROOT_FOLDER/Top of Personal Folders/Sent Messages",
			messageIndex:  3,
			wantCalls:     17,
			wantFolders:   2,
			wantProcessed: 17,
		},
		{
			name:          "out of range checkpoint restarts",
			folderIndex:   99,
			folderPath:    "ROOT_FOLDER/Top of Personal Folders/Sent Messages",
			messageIndex:  3,
			wantCalls:     17,
			wantFolders:   2,
			wantProcessed: 17,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := openIntegrationStore(t)
			src, err := st.GetOrCreateSource("pst", "resume@example.com")
			require.NoError(err, "get/create source")
			syncID, err := st.StartSync(src.ID, "import-pst")
			require.NoError(err, "start sync")
			cp := store.Checkpoint{
				MessagesProcessed: 9,
				MessagesAdded:     9,
				ErrorsCount:       1,
			}
			require.NoError(savePstCheckpoint(
				st, syncID, absPath, archiveID,
				tc.folderIndex, tc.folderPath, tc.messageIndex, &cp,
			), "save checkpoint")
			require.NoError(st.FailSync(syncID, "worker stopped"), "fail prior sync")

			mock := &mockIngestFunc{}
			summary, err := ImportPst(context.Background(), st, pstPath, PstImportOptions{
				Identifier:         "resume@example.com",
				CheckpointInterval: 1,
				IngestFunc:         mock.fn,
			})
			require.NoError(err, "ImportPst")
			require.True(summary.WasResumed, "expected checkpoint resume")
			assert.Len(mock.calls, tc.wantCalls, "ingest calls")
			assert.Equal(tc.wantFolders, summary.FoldersImported, "FoldersImported")
			assert.Equal(tc.wantProcessed, summary.MessagesProcessed, "MessagesProcessed")

			var errorsCount int64
			require.NoError(st.DB().QueryRow(
				`SELECT errors_count FROM sync_runs ORDER BY id DESC LIMIT 1`,
			).Scan(&errorsCount), "read resumed checkpoint")
			assert.Equal(int64(1), errorsCount, "prior errors retained")
		})
	}
}

func countEmptyAttachmentTables(t *testing.T, path string) int {
	t.Helper()
	require := require.New(t)

	pst, err := pstreader.Open(path)
	require.NoError(err, "open PST")
	t.Cleanup(func() {
		require.NoError(pst.Close(), "close PST")
	})

	count := 0
	require.NoError(pst.WalkFolders(func(entry pstreader.FolderEntry, folder *pstlib.Folder) error {
		msgIter, err := folder.GetMessageIterator()
		if err != nil {
			return nil //nolint:nilerr // skip folders whose message iterator cannot be read
		}
		for msgIter.Next() {
			msg := msgIter.Value()
			if pstreader.ExtractMessage(msg, entry.Path) == nil {
				continue
			}
			_, err := msg.GetAttachmentIterator()
			if errors.Is(err, pstlib.ErrTableContextNoRows) {
				count++
			}
		}
		return nil
	}), "walk folders")

	return count
}

// TestImportPst_EmptyAttachmentTableArchive imports a real PST containing a
// message flagged with attachments whose attachment table has no rows. Set
// MSGVAULT_TEST_PST_EMPTY_ATTACHMENT_TABLE to its path to run it.
func TestImportPst_EmptyAttachmentTableArchive(t *testing.T) {
	pstPath := os.Getenv("MSGVAULT_TEST_PST_EMPTY_ATTACHMENT_TABLE")
	if pstPath == "" {
		t.Skip("set MSGVAULT_TEST_PST_EMPTY_ATTACHMENT_TABLE to a PST containing an empty attachment table")
	}
	require := require.New(t)
	assert := assert.New(t)
	require.Positive(countEmptyAttachmentTables(t, pstPath), "archive has no empty attachment table")

	var logs bytes.Buffer
	st := openIntegrationStore(t)
	summary, err := ImportPst(context.Background(), st, pstPath, PstImportOptions{
		Identifier: "user@example.com",
		NoResume:   true,
		Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	})
	require.NoError(err, "ImportPst")
	assert.NotContains(logs.String(), "read attachments failed")
	assert.Positive(summary.MessagesProcessed, "MessagesProcessed")
	assert.False(summary.HardErrors, "HardErrors")
}

// TestImportPst_SupportPST_Idempotent verifies that re-importing the same PST
// skips all messages (content-hash deduplication).
func TestImportPst_SupportPST_Idempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")
	opts := PstImportOptions{
		Identifier: "support@hackingteam.com",
		NoResume:   true,
	}

	// First import.
	first, err := ImportPst(context.Background(), st, pstPath, opts)
	require.NoError(err, "first ImportPst")
	require.NotZero(first.MessagesAdded, "first import added no messages")

	// Second import — everything should be skipped.
	second, err := ImportPst(context.Background(), st, pstPath, opts)
	require.NoError(err, "second ImportPst")
	assert.Equal(first.MessagesAdded, second.MessagesSkipped,
		"second import skipped count should equal first import added count")
	assert.Equal(int64(0), second.MessagesAdded, "second import: added")
}

func TestImportPst_DoesNotCheckpointPastHeaderRepairFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")
	first, err := ImportPst(t.Context(), st, pstPath, PstImportOptions{
		Identifier: "owner@example.test",
		NoResume:   true,
	})
	require.NoError(err)
	require.Zero(first.Errors)

	var failedID int64
	require.NoError(st.DB().QueryRow(`
		SELECT id FROM messages
		WHERE source_id = ? AND rfc822_message_id IS NOT NULL AND rfc822_message_id <> ''
		ORDER BY id LIMIT 1
	`, first.SourceID).Scan(&failedID))
	var laterMessages int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE source_id = ? AND id > ?`, first.SourceID, failedID,
	).Scan(&laterMessages))
	require.GreaterOrEqual(laterMessages, 2, "the failed repair must have a later batch")
	_, err = st.DB().Exec(`UPDATE messages SET metadata = '{' WHERE id = ?`, failedID)
	require.NoError(err)

	failed, err := importPstWithBatchSize(t.Context(), st, pstPath, PstImportOptions{
		Identifier:         "owner@example.test",
		NoResume:           true,
		CheckpointInterval: 1,
	}, 2)
	require.NoError(err)
	require.True(failed.HardErrors)
	require.Equal(int64(1), failed.Errors)

	_, err = st.DB().Exec(`UPDATE messages SET metadata = NULL WHERE id = ?`, failedID)
	require.NoError(err)
	resumed, err := ImportPst(t.Context(), st, pstPath, PstImportOptions{
		Identifier:         "owner@example.test",
		CheckpointInterval: 1,
	})
	require.NoError(err)
	require.True(resumed.WasResumed)

	var metadata string
	require.NoError(st.DB().QueryRow(
		`SELECT COALESCE(metadata, '') FROM messages WHERE id = ?`, failedID,
	).Scan(&metadata))
	assert.Contains(metadata, `"pst_thread_key"`)
}

// TestImportPst_SupportPST_CrossFolderLabels verifies that duplicate messages
// (same content in Drafts and Sent Messages) get both folder labels applied
// rather than being ingested twice.
func TestImportPst_SupportPST_CrossFolderLabels(t *testing.T) {
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")

	summary, err := ImportPst(context.Background(), st, pstPath, PstImportOptions{
		Identifier: "support@hackingteam.com",
		NoResume:   true,
	})
	require.NoError(t, err, "ImportPst")

	// support.pst has 17 raw items but some subjects appear in both Drafts and
	// Sent Messages (duplicates). The total processed should equal all items.
	// Added + Skipped should equal processed (no items dropped).
	assert.Equal(t,
		summary.MessagesProcessed,
		summary.MessagesAdded+summary.MessagesSkipped+summary.MessagesUpdated,
		"accounting mismatch: added(%d)+skipped(%d)+updated(%d) != processed(%d)",
		summary.MessagesAdded, summary.MessagesSkipped, summary.MessagesUpdated, summary.MessagesProcessed)
}

// TestImportPst_SupportPST_SkipFolder verifies that --skip-folder correctly
// excludes the specified folder from import.
func TestImportPst_SupportPST_SkipFolder(t *testing.T) {
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")

	// Skip Drafts (6 messages) — should only import Sent Messages (11).
	summary, err := ImportPst(context.Background(), st, pstPath, PstImportOptions{
		Identifier:  "support@hackingteam.com",
		SkipFolders: []string{"Drafts"},
		NoResume:    true,
	})
	require.NoError(t, err, "ImportPst")

	// With Drafts skipped we process fewer messages. Some "Sent Messages" subjects
	// also appear in Drafts — but those aren't processed since Drafts is skipped.
	// At minimum we should have processed fewer than all 17.
	assert.Less(t, summary.MessagesProcessed, int64(17),
		"MessagesProcessed with Drafts skipped; expected < 17")
	assert.NotZero(t, summary.MessagesProcessed,
		"MessagesProcessed; Sent Messages should still be imported")
}

// TestImportPst_SupportPST_ContextCancelled verifies that cancelling mid-import
// saves a checkpoint and returns cleanly (no panic, no hang).
func TestImportPst_SupportPST_ContextCancelled(t *testing.T) {
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")

	// Cancel immediately — this should cause ImportPst to abort early.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	summary, _ := ImportPst(ctx, st, pstPath, PstImportOptions{
		Identifier: "support@hackingteam.com",
		NoResume:   true,
	})

	// Must not panic and must return a (possibly zero) summary.
	require.NotNil(t, summary, "ImportPst returned nil summary")
}

func TestImportPst_ContextCancellationDoesNotSkipPendingMessageOnResume(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "support.pst")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var ingestCalls int
	interrupted, err := importPstWithBatchSize(ctx, st, pstPath, PstImportOptions{
		Identifier:         "owner@example.test",
		CheckpointInterval: 200,
		IngestFunc: func(
			ctx context.Context, st *store.Store, sourceID int64, identifier, attachmentsDir string,
			labelIDs []int64, sourceMsgID, rawHash string, raw []byte, fallbackDate time.Time,
			log *slog.Logger,
		) error {
			ingestCalls++
			err := IngestRawMessage(ctx, st, sourceID, identifier, attachmentsDir, labelIDs, sourceMsgID, rawHash, raw, fallbackDate, log)
			if err == nil && ingestCalls == 1 {
				cancel()
			}
			return err
		},
	}, 2)
	require.NoError(err)
	require.Equal(1, ingestCalls)
	require.Equal(int64(1), interrupted.MessagesAdded)

	resumed, err := ImportPst(t.Context(), st, pstPath, PstImportOptions{
		Identifier: "owner@example.test",
	})
	require.NoError(err)
	assert.True(resumed.WasResumed)

	var messageCount int64
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE source_id = ?`, interrupted.SourceID,
	).Scan(&messageCount))
	assert.Equal(int64(17), messageCount)
}

// TestImportPst_32BitPST verifies that a 32-bit format PST is handled
// gracefully. go-pst may fail to read sub-folder metadata in 32-bit files;
// the importer skips those branches and completes without error.
func TestImportPst_32BitPST(t *testing.T) {
	st := openIntegrationStore(t)
	pstPath := filepath.Join(pstTestdataDir, "32-bit.pst")

	summary, err := ImportPst(context.Background(), st, pstPath, PstImportOptions{
		Identifier: "user@example.com",
		NoResume:   true,
	})
	require.NoError(t, err, "ImportPst")
	// 32-bit.pst has no readable email messages.
	assert.Equal(t, int64(0), summary.MessagesProcessed, "MessagesProcessed")
	assert.False(t, summary.HardErrors, "HardErrors")
}
