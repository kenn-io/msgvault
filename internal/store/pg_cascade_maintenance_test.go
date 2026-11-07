package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func cascadeMaintenanceFixture(t *testing.T) (*Store, int64, int64, int64) {
	t.Helper()
	skipUnlessPostgresInternal(t)
	st := newPGStoreInternal(t)
	source, err := st.GetOrCreateSource("gmail", "cascade@example.test")
	require.NoError(t, err)
	var conversationID, messageID, attachmentID int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `INSERT INTO conversations
		(source_id, source_conversation_id) VALUES ($1, 'cascade') RETURNING id`, source.ID).Scan(&conversationID))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `INSERT INTO messages
		(source_id, conversation_id, source_message_id) VALUES ($1, $2, 'cascade') RETURNING id`,
		source.ID, conversationID).Scan(&messageID))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `INSERT INTO attachments
		(message_id, content_hash, attachment_role, role_source)
		VALUES ($1, $2, 'standalone', 'importer_semantics') RETURNING id`,
		messageID, strings.Repeat("a", 64)).Scan(&attachmentID))
	return st, source.ID, messageID, attachmentID
}

// A real maintenance transaction must block DML on every previously omitted
// table, including empty tables. Reads remain available while it holds locks.
func TestIndirectCascadeMaintenanceLocks(t *testing.T) {
	skipUnlessPostgresInternal(t)
	st := newPGStoreInternal(t)
	conn, err := st.DB().Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, st.dialect.BeginExclusive(t.Context(), conn))
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(t.Context()), "ROLLBACK") }()

	for table, column := range map[string]string{
		"recording_references":               "state",
		"source_merge_archive_only_messages": "original_source_message_id",
		"source_merge_attachments":           "original_source_id",
		"source_merge_conversations":         "original_source_id",
		"source_merge_messages":              "original_source_id",
		"source_merge_preserved_attachments": "attachment_id",
		"visual_publications":                "state",
		"visual_work_claims":                 "lease_owner",
		"loop_message_evidence":              "token",
	} {
		t.Run(table, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			tx, err := st.DB().BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			_, err = tx.ExecContext(ctx, "SELECT 1 FROM "+table+" LIMIT 1")
			require.NoError(t, err, "maintenance permits readers")
			_, err = tx.ExecContext(ctx, "SET LOCAL lock_timeout = '100ms'")
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, "UPDATE "+table+" SET "+column+" = "+column+" WHERE FALSE")
			assert.True(t, isPgError(err, "55P03"), "maintenance must fence writes to %s: %v", table, err)
		})
	}
}

// Fence the actual entry points on sync_runs, then race their release with
// source removal. Observing pg_locks proves they wait before holding a child
// write lock, rather than merely completing by luck in an uncoordinated race.
func TestCascadeWritersTakeMaintenanceGate(t *testing.T) {
	for _, operation := range []string{
		"recording_reconcile", "recording_finish", "loop_evidence",
		"visual_claim", "visual_prepare", "visual_commit", "visual_restore",
		"visual_reject", "visual_tombstone", "visual_purge", "visual_restart",
	} {
		t.Run(operation, func(t *testing.T) {
			st, _, messageID, attachmentID := cascadeMaintenanceFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			removable, err := st.GetOrCreateSource("gmail", "unrelated@example.test")
			require.NoError(t, err)
			generation, err := st.EnsureVisualGeneration(ctx, VisualGenerationSpec{
				Fingerprint: "cascade-generation", Model: "synthetic-model", Dimension: 1024,
			})
			require.NoError(t, err)
			fence, err := st.AttachmentChangeHighWater(ctx)
			require.NoError(t, err)
			request := VisualClaimRequest{
				GenerationID:     generation.ID,
				Owner:            VisualOwner{MessageID: messageID, BlobHash: strings.Repeat("a", 64), MediaInputKey: "original"},
				ProposedRevision: "revision", LeaseOwner: "synthetic-worker",
				Now: time.Now().UTC(), LeaseDuration: time.Hour, SourceFence: fence,
			}
			claim, acquired, err := st.ClaimVisualWork(ctx, request)
			require.NoError(t, err)
			require.True(t, acquired)
			prepared := PreparedVisualPublication{Claim: claim, RepresentativeAttachmentID: attachmentID,
				Role: AttachmentRoleStandalone, RoleSource: AttachmentRoleSourceImporterSemantics}
			token, err := st.PrepareVisualPublication(ctx, prepared)
			require.NoError(t, err)
			refs := []RecordingReferenceInput{{RouteKey: "route", RefSHA256: "hash", OccurrenceJSON: `{}`}}
			require.NoError(t, st.ReconcileRecordingReferences(ctx, "destination", messageID, true, refs))
			recordings, err := st.ClaimRecordingReferences(ctx, "destination", time.Now().Add(time.Hour), 1)
			require.NoError(t, err)
			require.Len(t, recordings, 1)
			if operation == "visual_restore" {
				require.NoError(t, st.CommitVisualPublication(ctx, claim, token))
				claim, acquired, err = st.ClaimVisualWork(ctx, request)
				require.NoError(t, err)
				require.True(t, acquired)
				prepared.Claim = claim
			}
			if operation == "visual_purge" || operation == "visual_restart" {
				require.NoError(t, st.RetireVisualGeneration(ctx, generation.ID))
				// A restart is allowed only once backend token references are gone.
				require.NoError(t, st.ClearObsoleteVisualToken(ctx, generation.ID, token))
				_, err = st.DB().ExecContext(ctx, "UPDATE visual_publications SET pending_vector_token = NULL")
				require.NoError(t, err)
			}
			write := func() error {
				switch operation {
				case "recording_reconcile":
					return st.ReconcileRecordingReferences(ctx, "destination", messageID, true, refs)
				case "recording_finish":
					ok, err := st.FinishRecordingReference(ctx, recordings[0], RecordingReferenceResult{
						State: "retained", NextActionAt: time.Now().UTC()}, true, refs)
					if err == nil && !ok {
						return sql.ErrNoRows
					}
					return err
				case "loop_evidence":
					_, _, _, err := st.LoopMessageEvidenceContext(ctx, messageID, 1)
					return err
				case "visual_claim":
					request.ProposedRevision = "revision-2"
					_, _, err := st.ClaimVisualWork(ctx, request)
					return err
				case "visual_prepare":
					_, err := st.PrepareVisualPublication(ctx, prepared)
					return err
				case "visual_commit":
					return st.CommitVisualPublication(ctx, claim, token)
				case "visual_restore":
					return st.RestoreVisualPublication(ctx, prepared, token)
				case "visual_reject":
					return st.RejectVisualPublication(ctx, claim, VisualOutcome{Kind: "terminal", Reason: "synthetic"})
				case "visual_tombstone":
					return st.TombstoneVisualOwner(ctx, generation.ID, request.Owner, fence)
				case "visual_purge":
					return st.PurgeRetiredVisualGeneration(ctx, generation.ID)
				case "visual_restart":
					_, err := st.EnsureVisualGeneration(ctx, VisualGenerationSpec{
						Fingerprint: generation.Fingerprint, Model: generation.Model, Dimension: generation.Dimension})
					return err
				default:
					return sql.ErrNoRows
				}
			}

			blocker, err := st.DB().BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = blocker.Rollback() }()
			_, err = blocker.ExecContext(ctx, "LOCK TABLE sync_runs IN EXCLUSIVE MODE")
			require.NoError(t, err)
			var blockerPID int
			require.NoError(t, blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID))
			done := make(chan error, 1)
			go func() { done <- write() }()
			writerPID := waitForPostgresBlocker(t, st.DB(), blockerPID)
			var gateWait, childLock bool
			require.NoError(t, st.DB().QueryRowContext(ctx, `SELECT
				EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND relation='sync_runs'::regclass
					AND mode='RowExclusiveLock' AND NOT granted),
				EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND granted AND mode='RowExclusiveLock'
					AND relation IN ('messages'::regclass, 'recording_references'::regclass,
					'visual_publications'::regclass, 'visual_work_claims'::regclass,
					'loop_message_evidence'::regclass))`, writerPID).Scan(&gateWait, &childLock))
			assert.True(t, gateWait, "writer must wait at the maintenance gate")
			assert.False(t, childLock, "writer must not hold a child write lock while waiting")
			removeDone := make(chan error, 1)
			go func() {
				_, _, err := st.RemoveSourceSerialized(ctx, removable.ID)
				removeDone <- err
			}()
			require.NoError(t, blocker.Commit())
			require.NoError(t, <-done)
			require.NoError(t, <-removeDone)
		})
	}
}

// Park an in-flight insert before its message FK check. Maintenance must wait
// on the first table (sync_runs), leaving messages unlocked so the insert can
// finish. Without the writer gate, adding child tables creates a lock cycle.
func TestRecordingInsertDrainsBeforeMaintenance(t *testing.T) {
	st, sourceID, messageID, _ := cascadeMaintenanceFixture(t)
	recordingBatchDrainsBeforeMaintenance(t, st, sourceID, messageID, []RecordingReferenceInput{
		{RouteKey: "route", RefSHA256: "hash", OccurrenceJSON: `{}`},
	})
}

// Unscoped attachment updates must drain before maintenance locks the parents
// their visual invalidation triggers still need for foreign-key checks.
func TestAttachmentInvalidationDrainsBeforeMaintenance(t *testing.T) {
	st, _, messageID, attachmentID := cascadeMaintenanceFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	removable, err := st.GetOrCreateSource("gmail", "unrelated@example.test")
	require.NoError(t, err)
	generation, err := st.EnsureVisualGeneration(ctx, VisualGenerationSpec{
		Fingerprint: "cascade-generation", Model: "synthetic-model", Dimension: 1024,
	})
	require.NoError(t, err)
	fence, err := st.AttachmentChangeHighWater(ctx)
	require.NoError(t, err)
	claim, acquired, err := st.ClaimVisualWork(ctx, VisualClaimRequest{
		GenerationID:     generation.ID,
		Owner:            VisualOwner{MessageID: messageID, BlobHash: strings.Repeat("a", 64), MediaInputKey: "original"},
		ProposedRevision: "revision", LeaseOwner: "synthetic-worker",
		Now: time.Now().UTC(), LeaseDuration: time.Hour, SourceFence: fence,
	})
	require.NoError(t, err)
	require.True(t, acquired)
	_, err = st.PrepareVisualPublication(ctx, PreparedVisualPublication{
		Claim: claim, RepresentativeAttachmentID: attachmentID,
		Role: AttachmentRoleStandalone, RoleSource: AttachmentRoleSourceImporterSemantics,
	})
	require.NoError(t, err)
	blocker, err := st.DB().Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = blocker.Close() }()
	_, err = blocker.ExecContext(ctx, "SELECT pg_advisory_lock(678902)")
	require.NoError(t, err)
	// Release the session lock even if an assertion fails.
	defer func() { _, _ = blocker.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(678902)") }()
	var blockerPID int
	require.NoError(t, blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID))
	// Run after the FK check (RI_ConstraintTrigger...) and before visual
	// invalidation (trg_visual_publication...), isolating the trigger lock order.
	_, err = st.DB().ExecContext(ctx, `CREATE FUNCTION pause_attachment_update() RETURNS trigger AS $$
		BEGIN PERFORM pg_advisory_xact_lock(678902); RETURN NEW; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER pause_attachment_update AFTER UPDATE ON attachments
		FOR EACH ROW EXECUTE FUNCTION pause_attachment_update()`)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		done <- st.UpdateAttachmentMediaMetadataContext(ctx, messageID, strings.Repeat("a", 64),
			"image", sql.NullInt64{Int64: 640, Valid: true}, sql.NullInt64{Int64: 480, Valid: true}, sql.NullInt64{})
	}()
	writerPID := waitForPostgresBlocker(t, st.DB(), blockerPID)
	removeDone := make(chan error, 1)
	go func() {
		_, _, err := st.RemoveSourceSerialized(ctx, removable.ID)
		removeDone <- err
	}()
	maintenancePID := waitForPostgresBlocker(t, st.DB(), writerPID)
	var waitingAtGate, lockedPublication bool
	require.NoError(t, st.DB().QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND relation='sync_runs'::regclass
			AND mode='ExclusiveLock' AND NOT granted),
		EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND relation='visual_publications'::regclass
			AND mode='ExclusiveLock' AND granted)`, maintenancePID).Scan(&waitingAtGate, &lockedPublication))
	assert.True(t, waitingAtGate, "maintenance must drain the attachment writer before locking parents")
	assert.False(t, lockedPublication, "attachment triggers still need to update publications")
	_, err = blocker.ExecContext(ctx, "SELECT pg_advisory_unlock(678902)")
	require.NoError(t, err)
	require.NoError(t, <-done)
	require.NoError(t, <-removeDone)
	var state string
	var pendingToken sql.NullString
	require.NoError(t, st.DB().QueryRowContext(ctx,
		"SELECT state, pending_vector_token FROM visual_publications WHERE message_id=$1", messageID).Scan(&state, &pendingToken))
	assert.Equal(t, "stale", state)
	assert.False(t, pendingToken.Valid, "attachment invalidation clears the pending publication")
}

func TestRecordingBatchDrainsBeforeMaintenanceProperty(t *testing.T) {
	skipUnlessPostgresInternal(t)
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers(1, 8))
		salt := hegel.Draw(ht, hegel.Integers(uint64(0), ^uint64(0)))
		refs := make([]RecordingReferenceInput, count)
		for i := range refs {
			refs[i] = RecordingReferenceInput{
				RouteKey:  fmt.Sprintf("route-%x-%d", salt, i),
				RefSHA256: fmt.Sprintf("hash-%x", salt), OccurrenceJSON: `{}`,
			}
		}
		st, sourceID, messageID, _ := cascadeMaintenanceFixture(ht.T)
		recordingBatchDrainsBeforeMaintenance(ht, st, sourceID, messageID, refs)
	}, hegel.WithTestCases(8), hegel.WithDatabase(""))
}

func recordingBatchDrainsBeforeMaintenance(t testing.TB, st *Store, sourceID, messageID int64, refs []RecordingReferenceInput) {
	t.Helper()
	defer func() { require.NoError(t, st.Close()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	blocker, err := st.DB().Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = blocker.Close() }()
	_, err = blocker.ExecContext(ctx, "SELECT pg_advisory_lock(678901)")
	require.NoError(t, err)
	defer func() { _, _ = blocker.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(678901)") }()
	var blockerPID int
	require.NoError(t, blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID))
	_, err = st.DB().ExecContext(ctx, `CREATE FUNCTION pause_recording_insert() RETURNS trigger AS $$
		BEGIN PERFORM pg_advisory_xact_lock(678901); RETURN NEW; END; $$ LANGUAGE plpgsql;
		CREATE TRIGGER pause_recording_insert BEFORE INSERT ON recording_references
		FOR EACH ROW EXECUTE FUNCTION pause_recording_insert()`)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- st.ReconcileRecordingReferences(ctx, "destination", messageID, true, refs) }()
	writerPID := waitForPostgresBlocker(t, st.DB(), blockerPID)
	removeDone := make(chan error, 1)
	go func() {
		_, _, err := st.RemoveSourceSerialized(ctx, sourceID)
		removeDone <- err
	}()
	maintenancePID := waitForPostgresBlocker(t, st.DB(), writerPID)
	var waitingAtGate bool
	require.NoError(t, st.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
		WHERE pid=$1 AND relation='sync_runs'::regclass AND mode='ExclusiveLock' AND NOT granted)`,
		maintenancePID).Scan(&waitingAtGate))
	assert.True(t, waitingAtGate, "maintenance must wait before locking messages")
	_, err = blocker.ExecContext(ctx, "SELECT pg_advisory_unlock(678901)")
	require.NoError(t, err)
	require.NoError(t, <-done)
	require.NoError(t, <-removeDone)
	var remaining int
	require.NoError(t, st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM recording_references").Scan(&remaining))
	assert.Zero(t, remaining, "source deletion cascades the committed reference")
}
