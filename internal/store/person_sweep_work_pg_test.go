package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonSweepPostgreSQLReclaimedFenceRejectsPriorWorker(t *testing.T) {
	f := newPersonSweepJournalFixture(t, true, false)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL-only reclaimed-fence concurrency regression")
	}
	f.insertMessage(t, "postgres-reclaimed-fence", "email", f.aliceID,
		time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC))
	reconcilePersonSweepFixture(t, f, peoplesweep.SourceConversationText)
	first := claimPersonSweepFixture(t, f.store, "worker-a")
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_sweep_work SET lease_until = ? WHERE person_id = ?`),
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), first.PersonID)
	require.NoError(t, err)
	claimPersonSweepFixture(t, f.store, "worker-b")

	_, err = f.store.RenewPersonSweep(t.Context(), *first, time.Minute)
	require.ErrorIs(t, err, peoplesweep.ErrLeaseLost)
	err = f.store.FailPersonSweepWork(t.Context(), peoplesweep.WorkFailure{
		Lease: *first, AttemptID: "stale-attempt", Class: peoplesweep.FailureLeaseLost,
		RetryAt: time.Now().UTC(),
	})
	require.ErrorIs(t, err, peoplesweep.ErrLeaseLost)
}

func TestPersonSweepPostgreSQLUntrackingWinsPausedPublication(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL-only publication/untracking race regression")
	}
	deletePersonSweepWork(t, f.store, f.alicePersonID)
	syncID, err := f.store.StartSync(f.sourceID, "incremental")
	requirements.NoError(err)
	f.insertMessage(t, "postgres-paused-publication", "email", f.aliceID,
		time.Date(2026, 8, 23, 13, 0, 0, 0, time.UTC))

	const advisoryKey int64 = 725041
	holder, err := f.store.DB().Conn(t.Context())
	requirements.NoError(err)
	locked := true
	publishStarted := false
	untrackStarted := false
	publishFinished := make(chan struct{})
	untrackFinished := make(chan struct{})
	publishDone := make(chan error, 1)
	type untrackResult struct {
		state *store.PersonTracking
		err   error
	}
	untrackDone := make(chan untrackResult, 1)
	t.Cleanup(func() {
		if locked {
			_, _ = holder.ExecContext(context.Background(),
				`SELECT pg_advisory_unlock($1)`, advisoryKey)
		}
		if publishStarted {
			waitForPostgreSQLTestGoroutine(t, publishFinished)
		}
		if untrackStarted {
			waitForPostgreSQLTestGoroutine(t, untrackFinished)
		}
		_, _ = f.store.DB().ExecContext(context.Background(),
			`DROP TRIGGER IF EXISTS wait_for_person_sweep_publication ON person_sweep_work`)
		_, _ = f.store.DB().ExecContext(context.Background(),
			`DROP FUNCTION IF EXISTS wait_for_person_sweep_publication()`)
		_ = holder.Close()
	})
	_, err = holder.ExecContext(t.Context(), `SELECT pg_advisory_lock($1)`, advisoryKey)
	requirements.NoError(err)
	var holderPID int
	requirements.NoError(holder.QueryRowContext(t.Context(),
		`SELECT pg_backend_pid()`).Scan(&holderPID))
	_, err = f.store.DB().ExecContext(t.Context(), fmt.Sprintf(`
		CREATE FUNCTION wait_for_person_sweep_publication() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(%d);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql`, advisoryKey))
	requirements.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), `
		CREATE TRIGGER wait_for_person_sweep_publication
		BEFORE INSERT ON person_sweep_work
		FOR EACH ROW EXECUTE FUNCTION wait_for_person_sweep_publication()`)
	requirements.NoError(err)

	publishStarted = true
	go func() {
		defer close(publishFinished)
		publishDone <- f.store.CompleteSync(syncID, "published")
	}()
	publisherPID := waitForPostgreSQLBlockedPID(t.Context(), t, f.store, holderPID,
		"INSERT INTO person_sweep_work",
		"person sweep publication did not pause after selecting the tracked person")

	untrackStarted = true
	go func() {
		defer close(untrackFinished)
		state, untrackErr := f.store.SetPersonTrackingContext(
			t.Context(), f.alicePersonID, false)
		untrackDone <- untrackResult{state: state, err: untrackErr}
	}()
	waitForPostgreSQLBlockedPID(t.Context(), t, f.store, publisherPID,
		"DELETE FROM person_tracking",
		"tracking-off must serialize behind the publisher's tracking-row lock")
	var unlocked bool
	requirements.NoError(holder.QueryRowContext(t.Context(),
		`SELECT pg_advisory_unlock($1)`, advisoryKey).Scan(&unlocked))
	requirements.True(unlocked)
	locked = false
	requirements.NoError(<-publishDone)
	untracked := <-untrackDone
	requirements.NoError(untracked.err)
	requirements.NotNil(untracked.state)
	checks.False(untracked.state.Tracked)

	rows, _ := personSweepWorkState(t, f.store, f.alicePersonID)
	checks.Zero(rows, "publication must not recreate work after tracking-off commits")
	lease, err := f.store.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
		WorkerID: "post-opt-out-worker", LeaseDuration: time.Minute,
	})
	requirements.NoError(err)
	checks.Nil(lease)
}

func TestPersonSweepPostgreSQLConcurrentStartDoesNotSupersedeWriter(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL-only concurrent sync regression")
	}
	runID, err := f.store.StartSync(f.sourceID, "incremental")
	requirements.NoError(err)
	writer := f.store.ScopedToSync(f.sourceID, runID)
	partialID, err := writer.UpsertMessage(&store.Message{
		SourceID: f.sourceID, SourceMessageID: "active-run-committed-partial",
		ConversationID: f.conversationID, MessageType: "email",
		SenderID: sql.NullInt64{Int64: f.aliceID, Valid: true},
	})
	requirements.NoError(err)
	deletePersonSweepWork(t, f.store, f.alicePersonID)

	_, err = f.store.StartSync(f.sourceID, "incremental")
	requirements.ErrorIs(err, store.ErrSyncAlreadyActive)
	requirements.NoError(writer.UpsertMessageBody(partialID,
		sql.NullString{String: "active run body", Valid: true}, sql.NullString{}))
	secondID, err := writer.UpsertMessage(&store.Message{
		SourceID: f.sourceID, SourceMessageID: "active-run-continued",
		ConversationID: f.conversationID, MessageType: "email",
		SenderID: sql.NullInt64{Int64: f.aliceID, Valid: true},
	})
	requirements.NoError(err)
	secondSequence := latestPersonSweepSequence(t, f.store)
	requirements.NoError(writer.CompleteSync(runID, "completed-generation"))

	checks.True(messageExistsByID(t, f.store, partialID))
	checks.True(messageExistsByID(t, f.store, secondID))
	rows, dirtyThrough := personSweepWorkState(t, f.store, f.alicePersonID)
	checks.Equal(1, rows)
	checks.Equal(secondSequence, dirtyThrough)
	_, upper := personSweepSyncPublicationBounds(t, f.store, runID)
	requirements.True(upper.Valid)
	checks.Equal(secondSequence, upper.Int64)
}

func TestPersonSweepPostgreSQLClaimDoesNotReversePublicationOptOutLocks(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL-only claim/publication lock-order regression")
	}
	syncID, err := f.store.StartSync(f.sourceID, "incremental")
	requirements.NoError(err)
	f.insertMessage(t, "claim-publication-lock-order", "email", f.aliceID,
		time.Date(2026, 8, 23, 14, 0, 0, 0, time.UTC))
	requirements.NoError(f.store.CompleteSync(syncID, "published"))
	rows, _ := personSweepWorkState(t, f.store, f.alicePersonID)
	requirements.Equal(1, rows)

	holder, err := f.store.DB().Conn(t.Context())
	requirements.NoError(err)
	t.Cleanup(func() { _ = holder.Close() })
	tx, err := holder.BeginTx(t.Context(), nil)
	requirements.NoError(err)
	locked := true
	t.Cleanup(func() {
		if locked {
			_ = tx.Rollback()
		}
	})
	var holderPID int
	requirements.NoError(tx.QueryRowContext(t.Context(), `
		SELECT pg_backend_pid()
		FROM person_tracking
		WHERE person_id = $1
		FOR KEY SHARE`, f.alicePersonID).Scan(&holderPID))

	lease, err := f.store.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
		WorkerID: "claim-during-publication", LeaseDuration: time.Minute,
	})
	requirements.NoError(err,
		"claiming work must not wait on the publisher's tracking-row lock")
	requirements.NotNil(lease)
	requirements.Equal(f.alicePersonID, lease.PersonID)

	type untrackResult struct {
		state *store.PersonTracking
		err   error
	}
	untrackDone := make(chan untrackResult, 1)
	untrackFinished := make(chan struct{})
	go func() {
		defer close(untrackFinished)
		state, untrackErr := f.store.SetPersonTrackingContext(
			t.Context(), f.alicePersonID, false)
		untrackDone <- untrackResult{state: state, err: untrackErr}
	}()
	t.Cleanup(func() {
		if locked {
			_ = tx.Rollback()
			locked = false
		}
		waitForPostgreSQLTestGoroutine(t, untrackFinished)
	})
	waitForPostgreSQLBlockedPID(t.Context(), t, f.store, holderPID,
		"DELETE FROM person_tracking",
		"tracking-off must wait for the publisher's tracking-row lock")
	requirements.NoError(tx.Commit())
	locked = false
	untracked := <-untrackDone
	requirements.NoError(untracked.err)
	requirements.NotNil(untracked.state)
	checks.False(untracked.state.Tracked)

	rows, _ = personSweepWorkState(t, f.store, f.alicePersonID)
	checks.Zero(rows)
	lease, err = f.store.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
		WorkerID: "post-opt-out-worker", LeaseDuration: time.Minute,
	})
	requirements.NoError(err)
	checks.Nil(lease)
}

func messageExistsByID(t *testing.T, st *store.Store, messageID int64) bool {
	t.Helper()
	var exists bool
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT EXISTS (SELECT 1 FROM messages WHERE id = ?)`), messageID).Scan(&exists))
	return exists
}
