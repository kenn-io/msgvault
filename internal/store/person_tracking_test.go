package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func trackedTestPerson(t *testing.T) (*store.Store, *store.Person) {
	t.Helper()
	fixture := storetest.New(t)
	participant := fixture.EnsureParticipant(
		"tracked@example.com", "tracked", "example.com")
	person, _, err := fixture.Store.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	return fixture.Store, person
}

func TestPersonTrackingIsIdempotentPresenceState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, person := trackedTestPerson(t)
	ctx := t.Context()

	state, err := st.GetPersonTrackingContext(ctx, person.ID)
	require.NoError(err)
	assert.False(state.Tracked)
	assert.Nil(state.TrackedAt)

	tracked, err := st.SetPersonTrackingContext(ctx, person.ID, true)
	require.NoError(err)
	assert.True(tracked.Tracked)
	require.NotNil(tracked.TrackedAt)
	firstTrackedAt := *tracked.TrackedAt

	again, err := st.SetPersonTrackingContext(ctx, person.ID, true)
	require.NoError(err)
	require.NotNil(again.TrackedAt)
	assert.Equal(firstTrackedAt, *again.TrackedAt)
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(
		`DELETE FROM person_sweep_work WHERE person_id = ? RETURNING person_id`), person.ID).Scan(new(int64)))
	_, err = st.SetPersonTrackingContext(ctx, person.ID, true)
	require.NoError(err)
	var workRows int
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(
		`SELECT COUNT(*) FROM person_sweep_work WHERE person_id = ?`), person.ID).Scan(&workRows))
	assert.Zero(workRows, "idempotent tracking must not recreate consumed work")

	untracked, err := st.SetPersonTrackingContext(ctx, person.ID, false)
	require.NoError(err)
	assert.False(untracked.Tracked)
	assert.Nil(untracked.TrackedAt)

	againUntracked, err := st.SetPersonTrackingContext(ctx, person.ID, false)
	require.NoError(err)
	assert.False(againUntracked.Tracked)

	retracked, err := st.SetPersonTrackingContext(ctx, person.ID, true)
	require.NoError(err)
	require.NotNil(retracked.TrackedAt)
	assert.False(retracked.TrackedAt.Before(firstTrackedAt))

	unchanged, err := st.GetPersonContext(ctx, person.ID)
	require.NoError(err)
	assert.Equal(person.Revision, unchanged.Revision)
}

func TestPersonTrackingRejectsUnknownPersonAndCascadesOnDelete(t *testing.T) {
	require := require.New(t)
	st, person := trackedTestPerson(t)
	ctx := t.Context()

	_, err := st.GetPersonTrackingContext(ctx, person.ID+9999)
	require.ErrorIs(err, store.ErrPersonNotFound)
	_, err = st.SetPersonTrackingContext(ctx, person.ID+9999, true)
	require.ErrorIs(err, store.ErrPersonNotFound)

	_, err = st.SetPersonTrackingContext(ctx, person.ID, true)
	require.NoError(err)
	require.NoError(st.DeletePersonContext(ctx, person.ID, person.Revision))
	_, err = st.GetPersonTrackingContext(ctx, person.ID)
	require.ErrorIs(err, store.ErrPersonNotFound)

	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM person_tracking WHERE person_id = ?`),
		person.ID).Scan(&count))
	assert.Zero(t, count)
}

func TestPersonTrackingCreatesSweepWork(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	st, person := trackedTestPerson(t)

	state, err := st.SetPersonTrackingContext(t.Context(), person.ID, true)
	requirements.NoError(err)
	requirements.True(state.Tracked)

	rows, dirtyThrough := personSweepWorkState(t, st, person.ID)
	checks.Equal(1, rows)
	checks.Equal(latestPersonSweepSequence(t, st), dirtyThrough)
	var cursors int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT COUNT(*) FROM person_sweep_cursors WHERE person_id = ?`),
		person.ID).Scan(&cursors))
	checks.Zero(cursors, "tracking enrollment must leave fingerprint lanes to the worker")

	people, err := st.ListTrackedPeopleContext(t.Context(), 0, 1)
	requirements.NoError(err)
	checks.Equal([]int64{person.ID}, people)
	secondParticipant, err := st.EnsureParticipant(
		"tracked-second@example.test", "Second", "example.test")
	requirements.NoError(err)
	secondPerson, _, err := st.CreatePersonFromParticipant(secondParticipant)
	requirements.NoError(err)
	_, err = st.SetPersonTrackingContext(t.Context(), secondPerson.ID, true)
	requirements.NoError(err)
	people, err = st.ListTrackedPeopleContext(t.Context(), person.ID, 1)
	requirements.NoError(err)
	checks.Equal([]int64{secondPerson.ID}, people)
	_, err = st.ListTrackedPeopleContext(t.Context(), 0, 0)
	requirements.Error(err)
	_, err = st.ListTrackedPeopleContext(t.Context(), 0, 1_001)
	requirements.Error(err)
}

func TestPersonTrackingReenrollmentForcesFreshBackstop(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st, person := trackedTestPerson(t)
	_, err := st.SetPersonTrackingContext(t.Context(), person.ID, true)
	must.NoError(err)
	key := personSweepConversationCursorKey(person.ID)
	_, err = st.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{key})
	must.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_sweep_cursors SET
		reconcile_upper_key = 'upper', reconcile_after_key = 'upper',
		reconciliation_complete = TRUE, last_backstop_at = ?
		WHERE person_id = ?`), time.Now().UTC(), person.ID)
	must.NoError(err)
	_, err = st.SetPersonTrackingContext(t.Context(), person.ID, false)
	must.NoError(err)
	_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
	must.NoError(err)

	cursor := loadPersonSweepCursor(t, st, key)
	checks.Empty(cursor.ReconcileUpperKey)
	checks.Empty(cursor.ReconcileAfterKey)
	checks.False(cursor.ReconciliationComplete)
	checks.Nil(cursor.LastBackstopAt)
	rows, _ := personSweepWorkState(t, st, person.ID)
	checks.Equal(1, rows)
}

func TestPersonUntrackingStopsSweep(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	messageID := f.insertMessage(t, "untracking-history", "email", f.aliceID,
		time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))
	historyBefore := personSweepChangesAfter(t, f.store, f.alicePersonID, 0)
	requirements.NotEmpty(historyBefore)

	key := personSweepConversationCursorKey(f.alicePersonID)
	key.ProgramFingerprint = "untracking-program"
	key.CatalogFingerprint = "untracking-catalog"
	_, err := f.store.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{key})
	requirements.NoError(err)

	state, err := f.store.SetPersonTrackingContext(t.Context(), f.alicePersonID, false)
	requirements.NoError(err)
	checks.False(state.Tracked)
	rows, _ := personSweepWorkState(t, f.store, f.alicePersonID)
	checks.Zero(rows)

	lease, err := f.store.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
		WorkerID: "untracking-worker", LeaseDuration: time.Minute,
	})
	requirements.NoError(err)
	checks.Nil(lease)
	checks.Equal(historyBefore,
		personSweepChangesAfter(t, f.store, f.alicePersonID, 0),
		"untracking must not destroy durable archive mutation history")
	var cursors int
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_sweep_cursors WHERE person_id = ?`),
		f.alicePersonID).Scan(&cursors))
	checks.Equal(1, cursors, "untracking must preserve completed-attempt cursor history")

	var retainedMessage int64
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT message_id FROM person_sweep_changes
		WHERE person_id = ? ORDER BY sequence DESC LIMIT 1`),
		f.alicePersonID).Scan(&retainedMessage))
	checks.Equal(messageID, retainedMessage)
}

func personSweepWorkState(t *testing.T, st *store.Store, personID int64) (int, int64) {
	t.Helper()
	var (
		rows  int
		dirty sql.NullInt64
	)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT COUNT(*), MAX(dirty_through_sequence)
		FROM person_sweep_work WHERE person_id = ?`), personID).Scan(&rows, &dirty))
	return rows, dirty.Int64
}

func deletePersonSweepWork(t *testing.T, st *store.Store, personIDs ...int64) {
	t.Helper()
	for _, personID := range personIDs {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(
			`DELETE FROM person_sweep_work WHERE person_id = ?`), personID)
		require.NoError(t, err)
	}
}
