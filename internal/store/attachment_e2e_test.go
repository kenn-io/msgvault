package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// attachmentCorpus seeds a multi-source corpus of messages with attachments
// to exercise content-hash dedup, cross-source dedup, and ON DELETE CASCADE
// against the live store (SQLite or PostgreSQL via MSGVAULT_TEST_DB).
type attachmentCorpus struct {
	t       *testing.T
	store   *store.Store
	srcA    *store.Source
	srcB    *store.Source
	convA   int64
	convB   int64
	msgRows map[string]int64 // gmail id → message row id
}

func newAttachmentCorpus(t *testing.T) *attachmentCorpus {
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

	return &attachmentCorpus{
		t:       t,
		store:   st,
		srcA:    srcA,
		srcB:    srcB,
		convA:   convA,
		convB:   convB,
		msgRows: make(map[string]int64),
	}
}

func (c *attachmentCorpus) addMessage(gmailID string, sourceID, convID int64) int64 {
	c.t.Helper()
	id, err := c.store.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        sourceID,
		SourceMessageID: gmailID,
		MessageType:     "email",
		SizeEstimate:    100,
	})
	require.NoErrorf(c.t, err, "UpsertMessage(%s)", gmailID)
	c.msgRows[gmailID] = id
	return id
}

func (c *attachmentCorpus) addAttachment(gmailID, filename, hash string) {
	c.t.Helper()
	msgID, ok := c.msgRows[gmailID]
	require.Truef(c.t, ok, "addAttachment: unknown gmail id %q", gmailID)
	storagePath := hash[:2] + "/" + hash
	err := c.store.UpsertAttachment(msgID, filename, "application/pdf",
		storagePath, hash, 100)
	require.NoErrorf(c.t, err, "UpsertAttachment(%s, %s)", gmailID, filename)
}

func (c *attachmentCorpus) attachmentRowCount() int {
	c.t.Helper()
	var n int
	err := c.store.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&n)
	require.NoError(c.t, err, "attachmentRowCount")
	return n
}

// attachmentRowsForHash counts attachment rows carrying the given content
// hash. The hash argument is always hashShared in the current suite but
// kept explicit so each call site reads as a content-hash assertion.
func (c *attachmentCorpus) attachmentRowsForHash(hash string) int {
	c.t.Helper()
	var n int
	err := c.store.DB().QueryRow(
		c.store.Rebind(`SELECT COUNT(*) FROM attachments WHERE content_hash = ?`),
		hash,
	).Scan(&n)
	require.NoErrorf(c.t, err, "attachmentRowsForHash(%s)", hash)
	return n
}

// Deterministic SHA-256-shaped sentinels used throughout the suite.
var (
	hashShared = packTestHash("a1ab")
	hashUniqA  = packTestHash("a2cd")
	hashUniqB  = packTestHash("a3ef")
)

func TestGetMessageIncludesAttachmentWithNullableMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-nullhash", "Thread Null Hash")
	require.NoError(err, "EnsureConversation")
	msgID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "nullhash-msg",
		MessageType:     "email",
		Subject:         sql.NullString{String: "Attachment", Valid: true},
		SizeEstimate:    100,
	})
	require.NoError(err, "UpsertMessage")

	var attachmentID int64
	err = st.DB().QueryRow(
		st.Rebind(`INSERT INTO attachments (message_id, filename, mime_type, storage_path, content_hash, size, created_at)
			VALUES (?, ?, ?, ?, NULL, ?, CURRENT_TIMESTAMP)
			RETURNING id`),
		msgID, nil, nil, "legacy/path.bin", nil,
	).Scan(&attachmentID)
	require.NoError(err, "insert nullable attachment metadata")

	got, err := st.GetMessage(msgID)
	require.NoError(err, "GetMessage")
	require.Len(got.Attachments, 1, "attachments")
	assert.Equal(attachmentID, got.Attachments[0].ID, "id")
	assert.Empty(got.Attachments[0].Filename, "filename")
	assert.Empty(got.Attachments[0].MimeType, "mime_type")
	assert.Zero(got.Attachments[0].Size, "size")
	assert.Empty(got.Attachments[0].ContentHash, "content_hash")
}

// TestAttachment_E2E_MultiMessageDedup verifies that multiple messages within
// a single source can reference the same content_hash via UpsertAttachment
// and that the helper is idempotent (re-upserting the same (message_id,
// content_hash) pair is a no-op).
func TestAttachment_E2E_MultiMessageDedup(t *testing.T) {
	assert := assert.New(t)
	c := newAttachmentCorpus(t)

	// Three messages in source A referencing the same content hash.
	c.addMessage("msg-1", c.srcA.ID, c.convA)
	c.addMessage("msg-2", c.srcA.ID, c.convA)
	c.addMessage("msg-3", c.srcA.ID, c.convA)
	c.addAttachment("msg-1", "shared.pdf", hashShared)
	c.addAttachment("msg-2", "shared.pdf", hashShared)
	c.addAttachment("msg-3", "shared.pdf", hashShared)

	// One row per message, all referencing the same hash.
	assert.Equal(3, c.attachmentRowsForHash(hashShared), "rows for hashShared")

	// Idempotent re-upsert: existing (message_id, content_hash) is a no-op.
	c.addAttachment("msg-2", "shared.pdf", hashShared)
	assert.Equal(3, c.attachmentRowsForHash(hashShared), "rows for hashShared after re-upsert")

	// IsAttachmentPathReferenced reports the hash storage path as referenced.
	referenced, err := c.store.IsAttachmentPathReferenced(hashShared[:2] + "/" + hashShared)
	require.NoError(t, err, "IsAttachmentPathReferenced")
	assert.True(referenced, "expected referenced=true while messages still hold the hash")
}

// TestAttachment_E2E_CascadeOnMessageDelete verifies that deleting a message
// row removes its attachment row via ON DELETE CASCADE — but leaves other
// messages' attachment rows that reference the same content_hash intact.
func TestAttachment_E2E_CascadeOnMessageDelete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c := newAttachmentCorpus(t)

	// Two messages in source A referencing the shared hash plus one with a
	// unique hash.
	c.addMessage("msg-1", c.srcA.ID, c.convA)
	c.addMessage("msg-2", c.srcA.ID, c.convA)
	c.addMessage("msg-3", c.srcA.ID, c.convA)
	c.addAttachment("msg-1", "shared.pdf", hashShared)
	c.addAttachment("msg-2", "shared.pdf", hashShared)
	c.addAttachment("msg-3", "unique.pdf", hashUniqA)

	assert.Equal(3, c.attachmentRowCount(), "initial attachment count")

	// Permanently delete msg-1; its attachment row cascades.
	err := c.store.MarkMessageDeletedByGmailID(true, "msg-1")
	require.NoError(err, "MarkMessageDeletedByGmailID(permanent, msg-1)")

	assert.Equal(1, c.attachmentRowsForHash(hashShared), "rows for hashShared after delete")

	// The shared storage path is still referenced (msg-2 holds it).
	referenced, err := c.store.IsAttachmentPathReferenced(hashShared[:2] + "/" + hashShared)
	require.NoError(err, "IsAttachmentPathReferenced")
	assert.True(referenced, "shared path should remain referenced via msg-2 after msg-1 delete")

	// Now delete the last referrer of hashShared.
	err = c.store.MarkMessageDeletedByGmailID(true, "msg-2")
	require.NoError(err, "MarkMessageDeletedByGmailID(permanent, msg-2)")
	assert.Equal(0, c.attachmentRowsForHash(hashShared), "rows for hashShared after both deleted")

	referenced, err = c.store.IsAttachmentPathReferenced(hashShared[:2] + "/" + hashShared)
	require.NoError(err, "IsAttachmentPathReferenced after both deleted")
	assert.False(referenced, "shared path should be unreferenced after both messages deleted")
}

// TestAttachment_E2E_CrossSourceDedupPromotion verifies that
// AttachmentPathsUniqueToSource handles the cross-source case correctly:
// a hash shared with another source is NOT reported as unique. After the
// other source is removed, the same hash becomes unique.
func TestAttachment_E2E_CrossSourceDedupPromotion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c := newAttachmentCorpus(t)

	// Layout:
	//   source A: msg-a1 (shared hash), msg-a2 (unique-A hash)
	//   source B: msg-b1 (shared hash), msg-b2 (unique-B hash)
	c.addMessage("msg-a1", c.srcA.ID, c.convA)
	c.addMessage("msg-a2", c.srcA.ID, c.convA)
	c.addMessage("msg-b1", c.srcB.ID, c.convB)
	c.addMessage("msg-b2", c.srcB.ID, c.convB)
	c.addAttachment("msg-a1", "shared.pdf", hashShared)
	c.addAttachment("msg-a2", "unique-a.pdf", hashUniqA)
	c.addAttachment("msg-b1", "shared.pdf", hashShared)
	c.addAttachment("msg-b2", "unique-b.pdf", hashUniqB)
	const packID = "01hzy3v7q8r9s0t1a2v3w4x5p1"
	rec := store.PackRecord{
		PackID:      packID,
		EntryCount:  3,
		StoredBytes: 300,
		CreatedAt:   time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(c.store.RecordPackedBlobs(rec, []store.PackIndexEntry{
		{BlobHash: hashShared, PackID: packID, StoredLen: 100, RawLen: 100},
		{BlobHash: hashUniqA, PackID: packID, Offset: 100, StoredLen: 100, RawLen: 100},
		{BlobHash: hashUniqB, PackID: packID, Offset: 200, StoredLen: 100, RawLen: 100},
	}))

	// Before removing B: A's unique-set is just hashUniqA.
	pathsA, err := c.store.AttachmentPathsUniqueToSource(c.srcA.ID)
	require.NoError(err, "AttachmentPathsUniqueToSource(A)")
	wantA := hashUniqA[:2] + "/" + hashUniqA
	if assert.Len(pathsA, 1, "pathsA before B removal") {
		assert.Equal(wantA, pathsA[0], "pathsA[0] before B removal")
	}

	// Symmetric: B has only unique-B as a unique path.
	pathsB, err := c.store.AttachmentPathsUniqueToSource(c.srcB.ID)
	require.NoError(err, "AttachmentPathsUniqueToSource(B)")
	wantB := hashUniqB[:2] + "/" + hashUniqB
	if assert.Len(pathsB, 1, "pathsB before A removal") {
		assert.Equal(wantB, pathsB[0], "pathsB[0] before A removal")
	}

	// Remove source B transactionally. Its unique mapping is deleted while
	// the cross-source shared mapping remains live for A.
	_, packedB, err := c.store.RemoveSourceSerialized(context.Background(), c.srcB.ID)
	require.NoError(err, "RemoveSourceSerialized(B)")
	assert.Equal(int64(1), packedB, "only B's unique packed mapping is removed")
	entry, err := c.store.GetAttachmentPackEntry(hashShared)
	require.NoError(err)
	assert.NotNil(entry, "shared packed mapping remains live for A")
	entry, err = c.store.GetAttachmentPackEntry(hashUniqB)
	require.NoError(err)
	assert.Nil(entry, "B's unique packed mapping is logically deleted")

	pathsA, err = c.store.AttachmentPathsUniqueToSource(c.srcA.ID)
	require.NoError(err, "AttachmentPathsUniqueToSource(A) after B removal")
	got := testutil.MakeSet(pathsA...)
	for _, want := range []string{hashShared[:2] + "/" + hashShared, wantA} {
		assert.Truef(got[want], "paths missing %q after B removal; got %v", want, pathsA)
	}
	assert.Len(pathsA, 2, "pathsA len after B removal; got %v", pathsA)
	_, packedA, err := c.store.RemoveSourceSerialized(context.Background(), c.srcA.ID)
	require.NoError(err, "RemoveSourceSerialized(A)")
	assert.Equal(int64(2), packedA,
		"shared packed blob becomes unique and is deleted with A's original unique blob")
}

// TestAttachment_E2E_RemoveSourceCascadesAttachmentRows verifies that
// removing a source cascades all of its attachment rows but leaves rows
// in other sources alone — even when they share content_hash.
func TestAttachment_E2E_RemoveSourceCascadesAttachmentRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c := newAttachmentCorpus(t)

	c.addMessage("msg-a1", c.srcA.ID, c.convA)
	c.addMessage("msg-b1", c.srcB.ID, c.convB)
	c.addAttachment("msg-a1", "shared.pdf", hashShared)
	c.addAttachment("msg-b1", "shared.pdf", hashShared)

	assert.Equal(2, c.attachmentRowCount(), "initial attachment count")
	assert.Equal(2, c.attachmentRowsForHash(hashShared), "initial rows for shared hash")

	err := c.store.RemoveSource(c.srcA.ID)
	require.NoError(err, "RemoveSource(A)")

	assert.Equal(1, c.attachmentRowCount(), "attachment count after A removed")
	assert.Equal(1, c.attachmentRowsForHash(hashShared), "rows for shared hash after A removed (B keeps reference)")

	// IsAttachmentPathReferenced still reports the shared path as referenced
	// (B's row).
	referenced, err := c.store.IsAttachmentPathReferenced(hashShared[:2] + "/" + hashShared)
	require.NoError(err, "IsAttachmentPathReferenced")
	assert.True(referenced, "shared path should remain referenced via source B")
}

// TestAttachment_E2E_OrphanCleanupLifecycle simulates the full orphan-cleanup
// pipeline in remove_account.go for a multi-source corpus: collect candidate
// paths, run the source removal, then verify per-file reference checks against
// the post-removal DB state.
func TestAttachment_E2E_OrphanCleanupLifecycle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c := newAttachmentCorpus(t)

	// Source A has one unique attachment + one shared with B.
	// Source B has its own unique + the shared one.
	c.addMessage("msg-a1", c.srcA.ID, c.convA)
	c.addMessage("msg-a2", c.srcA.ID, c.convA)
	c.addMessage("msg-b1", c.srcB.ID, c.convB)
	c.addMessage("msg-b2", c.srcB.ID, c.convB)
	c.addAttachment("msg-a1", "shared.pdf", hashShared)
	c.addAttachment("msg-a2", "unique-a.pdf", hashUniqA)
	c.addAttachment("msg-b1", "shared.pdf", hashShared)
	c.addAttachment("msg-b2", "unique-b.pdf", hashUniqB)

	// Pipeline step 1: collect candidate paths for source A *before* the
	// cascade — matching remove_account.go's ordering.
	candidates, err := c.store.AttachmentPathsUniqueToSource(c.srcA.ID)
	require.NoError(err, "AttachmentPathsUniqueToSource(A)")
	wantUniqAPath := hashUniqA[:2] + "/" + hashUniqA
	if assert.Len(candidates, 1, "candidates for A") {
		assert.Equal(wantUniqAPath, candidates[0], "candidates[0] for A")
	}

	// Pipeline step 2: cascade-delete source A.
	hadActive, packedRemoved, err := c.store.RemoveSourceSerialized(context.Background(), c.srcA.ID)
	require.NoError(err, "RemoveSourceSerialized(A)")
	assert.False(hadActive, "hadActiveSync want false (no sync running in fixture)")
	assert.Zero(packedRemoved, "fixture has no packed mappings")

	// Pipeline step 3: per-candidate reference recheck. The candidate path
	// for A is now unreferenced (msg-a2 row is gone); the shared path is
	// still referenced by source B.
	referenced, err := c.store.IsAttachmentPathReferenced(wantUniqAPath)
	require.NoError(err, "IsAttachmentPathReferenced(uniqA)")
	assert.False(referenced, "uniqA path should be unreferenced after source A removed")

	sharedPath := hashShared[:2] + "/" + hashShared
	referenced, err = c.store.IsAttachmentPathReferenced(sharedPath)
	require.NoError(err, "IsAttachmentPathReferenced(shared)")
	assert.True(referenced, "shared path should remain referenced after source A removed")
}

// TestAttachment_E2E_NullAndEmptyHashesIgnored verifies that attachments with
// NULL content_hash or empty storage_path are excluded from
// AttachmentPathsUniqueToSource (mirroring the existing focused test but in
// a multi-message context).
func TestAttachment_E2E_NullAndEmptyHashesIgnored(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c := newAttachmentCorpus(t)

	c.addMessage("msg-a1", c.srcA.ID, c.convA)
	c.addMessage("msg-a2", c.srcA.ID, c.convA)
	c.addMessage("msg-a3", c.srcA.ID, c.convA)

	// Normal attachment with a unique content hash.
	c.addAttachment("msg-a1", "good.pdf", hashUniqA)

	// Attachment with NULL content_hash — must NOT appear in unique set.
	_, err := c.store.DB().Exec(c.store.Rebind(fmt.Sprintf(
		`INSERT INTO attachments (message_id, filename, mime_type, storage_path, content_hash, size, created_at)
		 VALUES (?, 'null-hash.pdf', 'application/pdf', 'nn/nullpath', NULL, 0, %s)`,
		"CURRENT_TIMESTAMP",
	)), c.msgRows["msg-a2"])
	require.NoError(err, "insert null-hash attachment")

	// Attachment with empty storage_path — also excluded.
	err = c.store.UpsertAttachment(c.msgRows["msg-a3"], "empty.pdf",
		"application/pdf", "", "emptypathhash", 0)
	require.NoError(err, "UpsertAttachment(empty)")

	paths, err := c.store.AttachmentPathsUniqueToSource(c.srcA.ID)
	require.NoError(err, "AttachmentPathsUniqueToSource")
	want := hashUniqA[:2] + "/" + hashUniqA
	if assert.Len(paths, 1, "paths want 1 only") {
		assert.Equal(want, paths[0], "paths[0]")
	}
}
