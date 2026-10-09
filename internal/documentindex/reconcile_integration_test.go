package documentindex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	inlineMessage := f.CreateMessage("document-reconcile-inline-image")
	inlineHash := strings.Repeat("d", 64)
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), inlineMessage, store.AttachmentWrite{
		Filename: "logo.png", MIMEType: "image/png", MediaType: "image", Size: 64,
		StoragePath: inlineHash[:2] + "/" + inlineHash, ContentHash: inlineHash,
		Role: store.AttachmentRoleInline, RoleSource: store.AttachmentRoleSourceMIMEDisposition,
	}))

	reconciler, err := NewReconciler(f.Store, ReconcilerConfig{
		AttachmentPageSize: 1, ChangePageSize: 1,
	})
	require.NoError(err)
	result, err := reconciler.Reconcile(t.Context())
	require.NoError(err)
	assert.True(result.ConsumerCreated)
	assert.True(result.FullScanCompleted)
	assert.Equal(3, result.AttachmentsExamined)
	assert.Equal(1, result.EligibleOccurrences)
	assert.Equal(0, result.ChangesConsumed)
	assert.Equal([]int64{firstID}, documentOccurrenceAttachmentIDs(t, f))

	secondMessage := f.CreateMessage("document-reconcile-second")
	secondID := createReconcileAttachment(t, f, secondMessage, "c")
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE attachments SET attachment_role = ?, role_source = ? WHERE id = ?`),
		store.AttachmentRolePreview, store.AttachmentRoleSourceMIMEDisposition, firstID)
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
		store.AttachmentRolePreview, store.AttachmentRoleSourceMIMEDisposition, attachmentID)
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
		"stale-name.pdf", store.AttachmentRolePreview,
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
	case <-time.After(50 * time.Millisecond): //nolint:kennlint // absence check: the held SQLite writer slot keeps reconciliation waiting
	}
	_, err = holder.ExecContext(t.Context(), `
		UPDATE attachments SET attachment_role = ? WHERE id = ?`,
		store.AttachmentRolePreview, attachmentID)
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
	var holderPID int
	require.NoError(holder.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&holderPID))
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
	var deliveryFenceKey int64
	var deliveryFenceOID int64
	require.NoError(f.Store.DB().QueryRow(`
		SELECT hashtextextended('msgvault.delivery_admission:' || current_schema(), 0),
		       'delivery_admission_lock'::regclass::oid
	`).Scan(&deliveryFenceKey, &deliveryFenceOID))
	advisoryWaiterCount := func() (int, error) {
		var waiting int
		err := f.Store.DB().QueryRow(f.Store.Rebind(`
			SELECT COUNT(*) FROM pg_stat_activity activity
			WHERE activity.datname = current_database()
			  AND activity.wait_event_type = 'Lock'
			  AND ? = ANY(pg_blocking_pids(activity.pid))
			  AND activity.query LIKE '%pg_advisory_xact_lock%'
			  AND EXISTS (
				SELECT 1 FROM pg_locks fence_lock
				WHERE fence_lock.pid = activity.pid
				  AND fence_lock.locktype = 'advisory'
				  AND fence_lock.mode = 'ShareLock'
				  AND fence_lock.granted
				  AND fence_lock.classid = ((CAST(? AS BIGINT) >> 32) & 4294967295)::oid
				  AND fence_lock.objid = (CAST(? AS BIGINT) & 4294967295)::oid
				  AND fence_lock.objsubid = 1
			  )`), holderPID, deliveryFenceKey, deliveryFenceKey).Scan(&waiting)
		return waiting, err
	}
	unscopedAdvisoryWaiterCount := func(pid int) (int, error) {
		var waiting int
		err := f.Store.DB().QueryRow(f.Store.Rebind(`
			SELECT COUNT(*) FROM pg_stat_activity activity
			WHERE activity.datname = current_database()
			  AND activity.pid = ?
			  AND activity.wait_event_type = 'Lock'
			  AND ? = ANY(pg_blocking_pids(activity.pid))
			  AND activity.query LIKE '%pg_advisory_xact_lock%'`), pid, holderPID).Scan(&waiting)
		return waiting, err
	}
	var waitingBefore int
	waitingBefore, err = advisoryWaiterCount()
	require.NoError(err)

	decoySchemaBytes := make([]byte, 8)
	_, err = rand.Read(decoySchemaBytes)
	require.NoError(err)
	decoySchema := "documentindex_decoy_" + hex.EncodeToString(decoySchemaBytes)
	_, err = f.Store.DB().Exec("CREATE SCHEMA " + decoySchema)
	require.NoError(err)
	t.Cleanup(func() {
		_, _ = f.Store.DB().ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+decoySchema+" CASCADE")
	})
	_, err = f.Store.DB().Exec("CREATE TABLE " + decoySchema + `.delivery_admission_lock (
		singleton INTEGER PRIMARY KEY)`)
	require.NoError(err)
	_, err = f.Store.DB().Exec("INSERT INTO " + decoySchema + `.delivery_admission_lock (singleton) VALUES (1)`)
	require.NoError(err)
	var decoyFenceOID int64
	require.NoError(f.Store.DB().QueryRow(
		"SELECT '" + decoySchema + ".delivery_admission_lock'::regclass::oid",
	).Scan(&decoyFenceOID))
	assert.NotEqual(deliveryFenceOID, decoyFenceOID)

	decoyCtx, cancelDecoy := context.WithCancel(context.Background())
	decoyTx, err := f.Store.DB().BeginTx(decoyCtx, nil)
	require.NoError(err)
	decoyStarted := false
	decoyFinished := make(chan struct{})
	stopDecoy := func() {
		cancelDecoy()
		if decoyStarted {
			select {
			case <-decoyFinished:
			case <-time.After(5 * time.Second):
				if held {
					_, _ = holder.ExecContext(context.Background(), f.Store.Rebind(`
						SELECT pg_advisory_unlock(hashtextextended(CAST(? AS TEXT), 0))`), lockName)
					held = false
				}
				<-decoyFinished
			}
		}
		_ = decoyTx.Rollback()
	}
	t.Cleanup(stopDecoy)
	var decoyPID int
	require.NoError(decoyTx.QueryRowContext(decoyCtx, `SELECT pg_backend_pid()`).Scan(&decoyPID))
	unscopedWaitingBefore, err := unscopedAdvisoryWaiterCount(decoyPID)
	require.NoError(err)
	require.Equal(0, unscopedWaitingBefore)
	var singleton int
	require.NoError(decoyTx.QueryRowContext(decoyCtx, "SELECT singleton FROM "+decoySchema+
		`.delivery_admission_lock WHERE singleton = 1 FOR UPDATE`).Scan(&singleton))
	require.Equal(1, singleton)
	decoyStarted = true
	go func() {
		defer close(decoyFinished)
		_, _ = decoyTx.ExecContext(decoyCtx, f.Store.Rebind(`
			SELECT pg_advisory_xact_lock(hashtextextended(CAST(? AS TEXT), 0))`), lockName)
	}()
	require.Eventually(func() bool {
		var decoyWaitingOnHolder bool
		err := f.Store.DB().QueryRow(f.Store.Rebind(`
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity activity
				WHERE activity.pid = ?
				  AND activity.wait_event_type = 'Lock'
				  AND ? = ANY(pg_blocking_pids(activity.pid))
				  AND activity.query LIKE '%pg_advisory_xact_lock%'
				  AND EXISTS (
					SELECT 1 FROM pg_locks relation_lock
					WHERE relation_lock.pid = activity.pid
					  AND relation_lock.locktype = 'relation'
					  AND relation_lock.relation::bigint = ?
					  AND relation_lock.granted
				  )
			)`), decoyPID, holderPID, decoyFenceOID).Scan(&decoyWaitingOnHolder)
		return err == nil && decoyWaitingOnHolder
	}, 5*time.Second, 10*time.Millisecond,
		"the decoy writer must hold its fence-table lock while waiting on this holder")
	unscopedWaitingAfter, err := unscopedAdvisoryWaiterCount(decoyPID)
	require.NoError(err)
	require.Equal(unscopedWaitingBefore+1, unscopedWaitingAfter,
		"the unscoped observation must count the decoy schema's advisory waiter")
	fixtureWaiters, err := advisoryWaiterCount()
	require.NoError(err)
	require.Equal(waitingBefore, fixtureWaiters,
		"the fixture-scoped observation must ignore the decoy schema's advisory waiter")
	stopDecoy()

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
		waiting, err := advisoryWaiterCount()
		return err == nil && waiting >= waitingBefore+1
	}, time.Second, time.Millisecond)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE attachments SET attachment_role = ? WHERE id = ?`),
		store.AttachmentRolePreview, attachmentID)
	require.NoError(err)
	higher := make(chan reconcileResult, 1)
	go func() {
		_, reconciledEligible, reconcileErr := f.Store.ReconcileDocumentOccurrence(
			t.Context(), attachmentID, 11,
		)
		higher <- reconcileResult{eligible: reconciledEligible, err: reconcileErr}
	}()
	require.Eventually(func() bool {
		waiting, err := advisoryWaiterCount()
		if err != nil {
			return false
		}
		if waiting >= waitingBefore+2 {
			return true
		}
		var secondWriterWaitingOnDeliveryFence bool
		err = f.Store.DB().QueryRow(f.Store.Rebind(`
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity waiter
				JOIN LATERAL unnest(pg_blocking_pids(waiter.pid)) blockers(pid) ON TRUE
				JOIN pg_stat_activity blocker ON blocker.pid = blockers.pid
				WHERE waiter.datname = current_database()
				  AND waiter.wait_event_type = 'Lock'
				  AND waiter.query LIKE '%delivery_admission_lock%'
				  AND blocker.datname = current_database()
				  AND blocker.wait_event_type = 'Lock'
				  AND blocker.query LIKE '%pg_advisory_xact_lock%'
				  AND ? = ANY(pg_blocking_pids(blocker.pid))
				  AND EXISTS (
					SELECT 1 FROM pg_locks relation_lock
					WHERE relation_lock.pid = waiter.pid
					  AND relation_lock.locktype = 'relation'
					  AND relation_lock.relation::bigint = ?
					  AND relation_lock.granted
				  )
				  AND EXISTS (
					SELECT 1 FROM pg_locks relation_lock
					WHERE relation_lock.pid = blocker.pid
					  AND relation_lock.locktype = 'relation'
					  AND relation_lock.relation::bigint = ?
					  AND relation_lock.granted
				  )
			)`), holderPID, deliveryFenceOID, deliveryFenceOID).Scan(&secondWriterWaitingOnDeliveryFence)
		return err == nil && secondWriterWaitingOnDeliveryFence
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
