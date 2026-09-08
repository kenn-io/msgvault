package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestPersonSweepWorkerForcedBriefWithoutArchiveHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newBriefWorkerEndToEndFixture(t, "brief-empty-archive")
	journal := newPersonSweepJournalFixture(t, false, true)
	st := journal.store
	personID := journal.bobPersonID
	profile, err := f.config.Profile()
	require.NoError(err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`INSERT INTO person_inference_consents (profile_fingerprint, granted_by)
		 VALUES (?, 'test-owner')`), profile.Fingerprint)
	require.NoError(err)
	_, err = st.SetPersonBriefEnrollmentContext(t.Context(), personID, true, "test-owner", true)
	require.NoError(err)
	f.worker.Store, f.worker.Source, f.worker.Sink = st, st, st
	f.worker.Catalog, f.worker.Brief, f.worker.Archive = st, st, st
	f.worker.Context = peoplesweep.NewContextRetriever(st)

	result, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: personID, Limit: 1, Brief: peoplesweep.BriefModeForce,
	})
	require.NoError(err)
	require.Len(result.People, 1)
	assert.Equal(1, result.PeopleSucceeded)
	assert.Zero(result.People[0].BriefVersion)
	assert.Empty(result.People[0].BriefFailureClass)
	assert.Zero(result.Usage.Requests)

	var status, failure string
	var completed bool
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT status, failure_class, completed_at IS NOT NULL
		FROM person_sweep_attempts WHERE id = ?`), result.People[0].AttemptID).
		Scan(&status, &failure, &completed))
	assert.Equal("succeeded", status)
	assert.Empty(failure)
	assert.True(completed)
	var batches, work int
	require.NoError(st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM person_sweep_batches`).Scan(&batches))
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM person_sweep_work WHERE person_id = ?`), personID).Scan(&work))
	assert.Zero(batches)
	assert.Zero(work)
}
