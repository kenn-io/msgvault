package deletion

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// e2eFixture builds a multi-source corpus with attachments suitable for
// exercising the full staged-deletion → mock Gmail → DB pipeline.
//
// Layout:
//
//	source A (alice@example.com): msg-a1, msg-a2, msg-a3
//	source B (bob@example.com):   msg-b1, msg-b2
//
// Attachments:
//   - msg-a1 has "shared" (hash=H1)        -- also referenced by msg-b1
//   - msg-a2 has "unique-a" (hash=H2)
//   - msg-a3 has no attachment
//   - msg-b1 has "shared" (hash=H1)        -- shares with msg-a1 across sources
//   - msg-b2 has "unique-b" (hash=H3)
type e2eFixture struct {
	t       *testing.T
	store   *store.Store
	sourceA *store.Source
	sourceB *store.Source
	convA   int64
	convB   int64
	msgIDs  map[string]int64 // gmail ID → row ID
}

func newE2EFixture(t *testing.T) *e2eFixture {
	t.Helper()
	st := testutil.NewTestStore(t)

	srcA, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err, "GetOrCreateSource A")
	srcB, err := st.GetOrCreateSource("gmail", "bob@example.com")
	require.NoError(t, err, "GetOrCreateSource B")
	convA, err := st.EnsureConversation(srcA.ID, "thread-A", "Thread A")
	require.NoError(t, err, "EnsureConversation A")
	convB, err := st.EnsureConversation(srcB.ID, "thread-B", "Thread B")
	require.NoError(t, err, "EnsureConversation B")

	f := &e2eFixture{
		t:       t,
		store:   st,
		sourceA: srcA,
		sourceB: srcB,
		convA:   convA,
		convB:   convB,
		msgIDs:  make(map[string]int64),
	}

	f.addMessage("msg-a1", srcA.ID, convA)
	f.addMessage("msg-a2", srcA.ID, convA)
	f.addMessage("msg-a3", srcA.ID, convA)
	f.addMessage("msg-b1", srcB.ID, convB)
	f.addMessage("msg-b2", srcB.ID, convB)

	// Cross-source shared attachment (same content_hash).
	const sharedHash = "h1sharedhash000000000000000000000000000000000000000000000000abcd"
	const uniqueAHash = "h2uniqueA00000000000000000000000000000000000000000000000000000ef"
	const uniqueBHash = "h3uniqueB000000000000000000000000000000000000000000000000000ab12"

	f.upsertAttachment("msg-a1", "shared.pdf", "h1/"+sharedHash, sharedHash)
	f.upsertAttachment("msg-b1", "shared.pdf", "h1/"+sharedHash, sharedHash)
	f.upsertAttachment("msg-a2", "unique-a.pdf", "h2/"+uniqueAHash, uniqueAHash)
	f.upsertAttachment("msg-b2", "unique-b.pdf", "h3/"+uniqueBHash, uniqueBHash)

	return f
}

func (f *e2eFixture) addMessage(gmailID string, sourceID, convID int64) {
	f.t.Helper()
	id, err := f.store.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        sourceID,
		SourceMessageID: gmailID,
		MessageType:     "email",
		SizeEstimate:    1024,
	})
	require.NoErrorf(f.t, err, "UpsertMessage(%s)", gmailID)
	f.msgIDs[gmailID] = id
}

func (f *e2eFixture) upsertAttachment(gmailID, filename, storagePath, contentHash string) {
	f.t.Helper()
	msgID, ok := f.msgIDs[gmailID]
	require.Truef(f.t, ok, "upsertAttachment: unknown gmail ID %q", gmailID)
	err := f.store.UpsertAttachment(msgID, filename, "application/pdf",
		storagePath, contentHash, 100)
	require.NoErrorf(f.t, err, "UpsertAttachment(%s, %s)", gmailID, filename)
}

func (f *e2eFixture) countLive(sourceID int64) int {
	f.t.Helper()
	var n int
	err := f.store.DB().QueryRow(f.store.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_id = ? AND `+
			store.LiveMessagesWhere("", true),
	), sourceID).Scan(&n)
	require.NoErrorf(f.t, err, "countLive(%d)", sourceID)
	return n
}

func (f *e2eFixture) countTotal(sourceID int64) int {
	f.t.Helper()
	var n int
	err := f.store.DB().QueryRow(
		f.store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`),
		sourceID,
	).Scan(&n)
	require.NoErrorf(f.t, err, "countTotal(%d)", sourceID)
	return n
}

func (f *e2eFixture) attachmentRowsForMessage(gmailID string) int {
	f.t.Helper()
	var n int
	err := f.store.DB().QueryRow(f.store.Rebind(
		`SELECT COUNT(*) FROM attachments a
		 JOIN messages m ON m.id = a.message_id
		 WHERE m.source_message_id = ?`,
	), gmailID).Scan(&n)
	require.NoErrorf(f.t, err, "attachmentRowsForMessage(%s)", gmailID)
	return n
}

// TestExecutor_E2E_TrashOnlyMarksTargetSource verifies that trashing messages
// in source A leaves source B's messages live, even when source B has a
// message with the same Gmail ID (no overlap in our fixture, but the
// assertion locks in source isolation behavior).
func TestExecutor_E2E_TrashOnlyMarksTargetSource(t *testing.T) {
	f := newE2EFixture(t)

	tc := newE2EContext(t, f)

	// Stage only source A's messages for trashing.
	manifest := tc.CreateManifest("trash-source-a",
		[]string{"msg-a1", "msg-a2", "msg-a3"})

	err := tc.Exec.Execute(context.Background(), manifest.ID, &ExecuteOptions{
		Method:    MethodTrash,
		BatchSize: 100,
		Resume:    true,
	})
	require.NoError(t, err, "Execute")

	// All A messages are now marked deleted_from_source_at; B is untouched.
	assert.Equal(t, 0, f.countLive(f.sourceA.ID), "source A live count")
	assert.Equal(t, 2, f.countLive(f.sourceB.ID), "source B live count (unaffected)")
	assert.Equal(t, 3, f.countTotal(f.sourceA.ID), "source A row count after trash (soft delete)")

	// Attachment rows are untouched on trash (only permanent delete cascades).
	assert.Equal(t, 1, f.attachmentRowsForMessage("msg-a1"), "msg-a1 attachment rows after trash")

	// Mock Gmail was called for each ID exactly once via TrashMessage.
	assert.Equal(t, 3, len(tc.MockAPI.TrashCalls), "TrashCalls")
	assert.Equal(t, 0, len(tc.MockAPI.DeleteCalls), "DeleteCalls (trash method)")
}

// TestExecutor_E2E_PermanentDeleteCascadesAttachments verifies that
// permanent (non-batch) deletion removes the message row entirely and
// cascades attachment rows. Cross-source shared attachments remain
// referenced via the other source.
func TestExecutor_E2E_PermanentDeleteCascadesAttachments(t *testing.T) {
	f := newE2EFixture(t)
	tc := newE2EContext(t, f)

	manifest := tc.CreateManifest("delete-a1-a2",
		[]string{"msg-a1", "msg-a2"})

	err := tc.Exec.Execute(context.Background(), manifest.ID, &ExecuteOptions{
		Method:    MethodDelete,
		BatchSize: 100,
		Resume:    true,
	})
	require.NoError(t, err, "Execute")

	// msg-a1 and msg-a2 rows are gone (permanent).
	assert.Equal(t, 1, f.countTotal(f.sourceA.ID), "source A row count after permanent delete")
	// Attachments for those messages cascade away.
	assert.Equal(t, 0, f.attachmentRowsForMessage("msg-a1"), "msg-a1 attachment rows after delete")
	assert.Equal(t, 0, f.attachmentRowsForMessage("msg-a2"), "msg-a2 attachment rows after delete")
	// Source B's row referencing the same shared content_hash survives.
	assert.Equal(t, 1, f.attachmentRowsForMessage("msg-b1"), "msg-b1 attachment rows (cross-source shared)")

	assert.Equal(t, 2, len(tc.MockAPI.DeleteCalls), "DeleteCalls")
}

// TestExecutor_E2E_BatchDeleteMarksDBAcrossSources verifies that the batch
// deletion path marks rows across both sources when a manifest spans them.
// MarkMessagesDeletedByGmailIDBatch uses an IN(...) UPDATE without a source
// filter; this test pins that behavior so future schema changes don't
// silently regress it.
func TestExecutor_E2E_BatchDeleteMarksDBAcrossSources(t *testing.T) {
	f := newE2EFixture(t)
	tc := newE2EContext(t, f)

	manifest := tc.CreateManifest("batch-cross-source",
		[]string{"msg-a1", "msg-b1"})

	err := tc.Exec.ExecuteBatch(context.Background(), manifest.ID)
	require.NoError(t, err, "ExecuteBatch")

	// Both sources see one of their messages soft-deleted.
	assert.Equal(t, 2, f.countLive(f.sourceA.ID), "source A live count (msg-a1 deleted, a2+a3 remain)")
	assert.Equal(t, 1, f.countLive(f.sourceB.ID), "source B live count (msg-b1 deleted)")

	// Batch path doesn't cascade attachments — it sets deleted_from_source_at.
	assert.Equal(t, 1, f.attachmentRowsForMessage("msg-a1"), "msg-a1 attachment rows after batch trash")

	assert.Equal(t, 1, len(tc.MockAPI.BatchDeleteCalls), "BatchDeleteCalls")
}

// TestExecutor_E2E_PermanentDeletePreservesOtherSourceAttachmentFile
// confirms that AttachmentPathsUniqueToSource, called after a permanent
// per-message delete, only reports paths whose content_hash is no longer
// referenced anywhere in the DB — exercising the orphan-detection query
// against a state mutated by the executor itself.
func TestExecutor_E2E_PermanentDeletePreservesOtherSourceAttachmentFile(t *testing.T) {
	f := newE2EFixture(t)
	tc := newE2EContext(t, f)

	// Permanently delete msg-a1 (shared hash) and msg-a2 (unique to A).
	manifest := tc.CreateManifest("perm-delete-a1-a2",
		[]string{"msg-a1", "msg-a2"})
	err := tc.Exec.Execute(context.Background(), manifest.ID, &ExecuteOptions{
		Method:    MethodDelete,
		BatchSize: 100,
		Resume:    true,
	})
	require.NoError(t, err, "Execute")

	// After deletion, source A still has msg-a3 (no attachments).
	// AttachmentPathsUniqueToSource(A) should be empty: the only remaining
	// candidate path would have been msg-a2's, but its row is gone.
	pathsA, err := f.store.AttachmentPathsUniqueToSource(f.sourceA.ID)
	require.NoError(t, err, "AttachmentPathsUniqueToSource(A)")
	assert.Empty(t, pathsA, "source A unique paths after delete")

	// Source B's unique-b path is still uniquely owned by B; shared is
	// also unique to B now that A's reference is gone.
	pathsB, err := f.store.AttachmentPathsUniqueToSource(f.sourceB.ID)
	require.NoError(t, err, "AttachmentPathsUniqueToSource(B)")
	assert.Len(t, pathsB, 2, "source B unique paths after A's permanent delete (shared became unique to B)")
}

// e2eContext bridges the e2eFixture to the executor test plumbing.
type e2eContext struct {
	*TestContext

	fix *e2eFixture
}

func newE2EContext(t *testing.T, f *e2eFixture) *e2eContext {
	t.Helper()
	tmpDir := t.TempDir()
	mgr, err := NewManager(tmpDir)
	require.NoError(t, err, "NewManager")
	progress := &trackingProgress{}
	mockAPI := gmail.NewDeletionMockAPI()
	exec := NewExecutor(mgr, f.store, mockAPI).WithProgress(progress)
	tc := &TestContext{
		Mgr:      mgr,
		Store:    f.store,
		MockAPI:  mockAPI,
		Exec:     exec,
		Progress: progress,
		Dir:      tmpDir,
		t:        t,
	}
	return &e2eContext{TestContext: tc, fix: f}
}
