package documentindex

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestReconcilerBootstrapAndReplayConvergeOnCurrentOccurrences(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	firstMessage := f.CreateMessage("document-reconcile-first")
	firstID := createReconcileAttachment(t, f, firstMessage, "a")
	unknownMessage := f.CreateMessage("document-reconcile-unknown")
	unknownHash := strings.Repeat("b", 64)
	require.NoError(f.Store.UpsertAttachment(
		unknownMessage, "unknown.pdf", "application/pdf",
		unknownHash[:2]+"/"+unknownHash, unknownHash, 64,
	))

	reconciler, err := NewReconciler(f.Store, ReconcilerConfig{
		AttachmentPageSize: 1, ChangePageSize: 1,
	})
	require.NoError(err)
	result, err := reconciler.Reconcile(t.Context())
	require.NoError(err)
	assert.True(result.ConsumerCreated)
	assert.True(result.FullScanCompleted)
	assert.Equal(2, result.AttachmentsExamined)
	assert.Equal(1, result.EligibleOccurrences)
	assert.Equal(0, result.ChangesConsumed)
	assert.Equal([]int64{firstID}, documentOccurrenceAttachmentIDs(t, f))

	secondMessage := f.CreateMessage("document-reconcile-second")
	secondID := createReconcileAttachment(t, f, secondMessage, "c")
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE attachments SET attachment_role = ?, role_source = ? WHERE id = ?`),
		store.AttachmentRoleInline, store.AttachmentRoleSourceMIMEDisposition, firstID)
	require.NoError(err)

	result, err = reconciler.Reconcile(t.Context())
	require.NoError(err)
	assert.False(result.ConsumerCreated)
	assert.False(result.FullScanCompleted)
	assert.Zero(result.AttachmentsExamined)
	assert.Equal(2, result.ChangesConsumed)
	assert.Equal([]int64{secondID}, documentOccurrenceAttachmentIDs(t, f))
}

func TestReconcilerReenableUsesDurableJournalHighWater(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("document-reconcile-reenable")
	attachmentID := createReconcileAttachment(t, f, messageID, "9")
	reconciler, err := NewReconciler(f.Store, ReconcilerConfig{
		AttachmentPageSize: 10, ChangePageSize: 10,
	})
	require.NoError(err)
	_, err = reconciler.Reconcile(t.Context())
	require.NoError(err)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE attachments SET filename = ? WHERE id = ?`), "updated.pdf", attachmentID)
	require.NoError(err)
	_, err = reconciler.Reconcile(t.Context())
	require.NoError(err)
	var sourceSequence int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT source_sequence FROM document_occurrences WHERE attachment_id = ?`),
		attachmentID,
	).Scan(&sourceSequence))
	assert.Positive(sourceSequence)

	require.NoError(f.Store.UnregisterAttachmentChangeConsumer(
		t.Context(), DocumentAttachmentConsumerKey,
	))
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE attachments SET attachment_role = ?, role_source = ? WHERE id = ?`),
		store.AttachmentRoleInline, store.AttachmentRoleSourceMIMEDisposition, attachmentID)
	require.NoError(err)

	result, err := reconciler.Reconcile(t.Context())
	require.NoError(err)
	assert.True(result.ConsumerCreated)
	assert.True(result.FullScanCompleted)
	assert.Empty(documentOccurrenceAttachmentIDs(t, f))
	consumer, err := f.Store.GetAttachmentChangeConsumer(t.Context(), DocumentAttachmentConsumerKey)
	require.NoError(err)
	assert.Equal(sourceSequence, consumer.BaselineSequence)
}

func TestConcurrentReconciliationTreatsAlreadyAdvancedCursorAsSuccess(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	reconciler, err := NewReconciler(f.Store, ReconcilerConfig{
		AttachmentPageSize: 10, ChangePageSize: 10,
	})
	require.NoError(err)
	_, err = reconciler.Reconcile(t.Context())
	require.NoError(err)
	messageID := f.CreateMessage("document-reconcile-concurrent")
	attachmentID := createReconcileAttachment(t, f, messageID, "f")

	start := make(chan struct{})
	errorsFound := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			_, reconcileErr := reconciler.Reconcile(t.Context())
			errorsFound <- reconcileErr
		})
	}
	close(start)
	workers.Wait()
	close(errorsFound)
	for reconcileErr := range errorsFound {
		require.NoError(reconcileErr)
	}
	assert.Equal(t, []int64{attachmentID}, documentOccurrenceAttachmentIDs(t, f))
}

func TestOccurrenceReconciliationIgnoresStaleSourceSequence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("document-reconcile-stale")
	attachmentID := createReconcileAttachment(t, f, messageID, "7")
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 10)
	require.NoError(err)
	require.True(eligible)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE attachments SET filename = ?, attachment_role = ?, role_source = ? WHERE id = ?`),
		"stale-name.pdf", store.AttachmentRoleInline,
		store.AttachmentRoleSourceMIMEDisposition, attachmentID)
	require.NoError(err)
	_, eligible, err = f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 9)
	require.NoError(err)
	assert.False(eligible)

	var filename string
	var sequence int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT filename, source_sequence FROM document_occurrences WHERE attachment_id = ?`),
		attachmentID,
	).Scan(&filename, &sequence))
	assert.Equal("synthetic.pdf", filename)
	assert.Equal(int64(10), sequence)
}

func TestConcurrentOccurrenceReconciliationKeepsHighestSourceSequence(t *testing.T) {
	f := storetest.New(t)
	messageID := f.CreateMessage("document-reconcile-monotonic")
	attachmentID := createReconcileAttachment(t, f, messageID, "8")

	start := make(chan struct{})
	errorsFound := make(chan error, 2)
	var workers sync.WaitGroup
	for _, sequence := range []int64{50, 100} {
		workers.Go(func() {
			<-start
			_, _, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, sequence)
			errorsFound <- err
		})
	}
	close(start)
	workers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		require.NoError(t, err)
	}

	var sequence int64
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT source_sequence FROM document_occurrences WHERE attachment_id = ?`),
		attachmentID,
	).Scan(&sequence))
	assert.Equal(t, int64(100), sequence)
}

func TestSQLiteOccurrenceReconciliationReadsAfterWriterSlot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("SQLite writer-slot regression")
	}
	messageID := f.CreateMessage("document-reconcile-writer-slot")
	attachmentID := createReconcileAttachment(t, f, messageID, "1")
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 9)
	require.NoError(err)
	require.True(eligible)

	holder, err := f.Store.DB().Conn(t.Context())
	require.NoError(err)
	held := true
	t.Cleanup(func() {
		if held {
			_, _ = holder.ExecContext(context.Background(), "ROLLBACK")
		}
		_ = holder.Close()
	})
	_, err = holder.ExecContext(t.Context(), "BEGIN IMMEDIATE")
	require.NoError(err)

	type reconcileResult struct {
		eligible bool
		err      error
	}
	result := make(chan reconcileResult, 1)
	go func() {
		_, reconciledEligible, reconcileErr := f.Store.ReconcileDocumentOccurrence(
			t.Context(), attachmentID, 10,
		)
		result <- reconcileResult{eligible: reconciledEligible, err: reconcileErr}
	}()
	require.Eventually(func() bool {
		return f.Store.DB().Stats().InUse >= 2 || len(result) > 0
	}, time.Second, time.Millisecond)
	select {
	case <-result:
		require.Fail("reconciliation returned while the SQLite writer slot was held")
	default:
	}
	require.GreaterOrEqual(f.Store.DB().Stats().InUse, 2)

	select {
	case <-result:
		require.Fail("reconciliation returned while the SQLite writer slot was held")
	case <-time.After(50 * time.Millisecond):
	}
	_, err = holder.ExecContext(t.Context(), `
		UPDATE attachments SET attachment_role = ? WHERE id = ?`,
		store.AttachmentRoleInline, attachmentID)
	require.NoError(err)
	_, err = holder.ExecContext(t.Context(), `
		DELETE FROM document_occurrences
		WHERE attachment_id = ? AND source_sequence <= ?`, attachmentID, 11)
	require.NoError(err)
	_, err = holder.ExecContext(t.Context(), "COMMIT")
	require.NoError(err)
	held = false
	reconciled := <-result
	require.NoError(reconciled.err)
	assert.False(reconciled.eligible)
	assert.Empty(documentOccurrenceAttachmentIDs(t, f))
}

func TestPostgreSQLOccurrenceReconciliationSerializesEligibilityRead(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL advisory-lock regression")
	}
	messageID := f.CreateMessage("document-reconcile-advisory-lock")
	attachmentID := createReconcileAttachment(t, f, messageID, "2")
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 9)
	require.NoError(err)
	require.True(eligible)

	holder, err := f.Store.DB().Conn(t.Context())
	require.NoError(err)
	lockName := "msgvault.document_occurrence.attachment:" + strconv.FormatInt(attachmentID, 10)
	held := true
	t.Cleanup(func() {
		if held {
			_, _ = holder.ExecContext(context.Background(), f.Store.Rebind(`
				SELECT pg_advisory_unlock(hashtextextended(CAST(? AS TEXT), 0))`), lockName)
		}
		_ = holder.Close()
	})
	_, err = holder.ExecContext(t.Context(), f.Store.Rebind(`
		SELECT pg_advisory_lock(hashtextextended(CAST(? AS TEXT), 0))`), lockName)
	require.NoError(err)
	var waitingBefore int
	require.NoError(f.Store.DB().QueryRow(`
		SELECT COUNT(*) FROM pg_locks
		WHERE locktype = 'advisory' AND NOT granted`).Scan(&waitingBefore))

	type reconcileResult struct {
		eligible bool
		err      error
	}
	lower := make(chan reconcileResult, 1)
	go func() {
		_, reconciledEligible, reconcileErr := f.Store.ReconcileDocumentOccurrence(
			t.Context(), attachmentID, 10,
		)
		lower <- reconcileResult{eligible: reconciledEligible, err: reconcileErr}
	}()
	require.Eventually(func() bool {
		var waiting int
		err := f.Store.DB().QueryRow(`
			SELECT COUNT(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting)
		return err == nil && waiting >= waitingBefore+1
	}, time.Second, time.Millisecond)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE attachments SET attachment_role = ? WHERE id = ?`),
		store.AttachmentRoleInline, attachmentID)
	require.NoError(err)
	higher := make(chan reconcileResult, 1)
	go func() {
		_, reconciledEligible, reconcileErr := f.Store.ReconcileDocumentOccurrence(
			t.Context(), attachmentID, 11,
		)
		higher <- reconcileResult{eligible: reconciledEligible, err: reconcileErr}
	}()
	require.Eventually(func() bool {
		var waiting int
		err := f.Store.DB().QueryRow(`
			SELECT COUNT(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting)
		return len(higher) > 0 || err == nil && waiting >= waitingBefore+2
	}, time.Second, time.Millisecond)

	_, err = holder.ExecContext(t.Context(), f.Store.Rebind(`
		SELECT pg_advisory_unlock(hashtextextended(CAST(? AS TEXT), 0))`), lockName)
	require.NoError(err)
	held = false
	lowResult := <-lower
	highResult := <-higher
	require.NoError(lowResult.err)
	require.NoError(highResult.err)
	assert.False(lowResult.eligible)
	assert.False(highResult.eligible)
	assert.Empty(documentOccurrenceAttachmentIDs(t, f))
}

func TestReconcilerRemovesCascadedOccurrenceFromJournalReplay(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("document-reconcile-delete")
	attachmentID := createReconcileAttachment(t, f, messageID, "d")
	reconciler, err := NewReconciler(f.Store, ReconcilerConfig{
		AttachmentPageSize: 10, ChangePageSize: 10,
	})
	require.NoError(err)
	_, err = reconciler.Reconcile(t.Context())
	require.NoError(err)
	assert.Equal(t, []int64{attachmentID}, documentOccurrenceAttachmentIDs(t, f))

	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM messages WHERE id = ?`), messageID)
	require.NoError(err)
	result, err := reconciler.Reconcile(t.Context())
	require.NoError(err)
	assert.Equal(t, 1, result.ChangesConsumed)
	assert.Empty(t, documentOccurrenceAttachmentIDs(t, f))
}

func TestReconcilerPeriodicBackstopRemovesOccurrenceAfterMissedEvent(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("document-reconcile-missed")
	createReconcileAttachment(t, f, messageID, "e")
	reconciler, err := NewReconciler(f.Store, ReconcilerConfig{
		AttachmentPageSize: 10, ChangePageSize: 10,
	})
	require.NoError(err)
	_, err = reconciler.Reconcile(t.Context())
	require.NoError(err)
	require.Len(documentOccurrenceAttachmentIDs(t, f), 1)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`DELETE FROM attachment_change_log`)
	require.NoError(err, "simulate a legacy or externally lost journal event")
	result, err := reconciler.FullReconcile(t.Context())
	require.NoError(err)
	assert.True(t, result.FullScanCompleted)
	assert.Equal(t, 1, result.AttachmentsExamined)
	assert.Empty(t, documentOccurrenceAttachmentIDs(t, f))
}

func createReconcileAttachment(
	t *testing.T,
	f *storetest.Fixture,
	messageID int64,
	hashCharacter string,
) int64 {
	t.Helper()
	hash := strings.Repeat(hashCharacter, 64)
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: 64,
		StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: "part:1",
	}))
	var id int64
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT id FROM attachments WHERE message_id = ? AND source_part_key = ?`), messageID, "part:1").Scan(&id))
	return id
}

func documentOccurrenceAttachmentIDs(t *testing.T, f *storetest.Fixture) []int64 {
	t.Helper()
	require := require.New(t)
	rows, err := f.Store.DB().Query(`SELECT attachment_id FROM document_occurrences ORDER BY attachment_id`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(rows.Err())
	return ids
}
