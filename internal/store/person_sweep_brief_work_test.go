package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

func personSweepBriefClaim(personID int64) peoplesweep.ClaimRequest {
	return peoplesweep.ClaimRequest{WorkerID: "brief-worker", LeaseDuration: time.Minute,
		AvailableAt: time.Now().UTC(), PersonID: personID}
}

func TestHasPersonSweepChangesAfterReportsNewActivity(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	sentAt := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	f.insertMessage(t, "brief-activity-1", "chat", f.aliceID, sentAt)

	high, err := f.store.LatestPersonSweepChangeSequence(t.Context())
	requirements.NoError(err)
	requirements.Positive(high, "inserting a scoped message must journal a change")

	fresh, err := f.store.HasPersonSweepChangesAfter(t.Context(), f.alicePersonID, 0)
	requirements.NoError(err)
	checks.True(fresh)

	fresh, err = f.store.HasPersonSweepChangesAfter(t.Context(), f.alicePersonID, high)
	requirements.NoError(err)
	checks.False(fresh, "a brief generated through the high water sees no new activity")

	f.insertMessage(t, "brief-activity-2", "chat", f.aliceID, sentAt.Add(time.Hour))
	fresh, err = f.store.HasPersonSweepChangesAfter(t.Context(), f.alicePersonID, high)
	requirements.NoError(err)
	checks.True(fresh, "a later message is new activity past the brief's boundary")

	_, err = f.store.HasPersonSweepChangesAfter(t.Context(), 0, 0)
	requirements.Error(err)
}

func TestEnsurePersonSweepWorkPublishesClaimableWorkForATrackedPerson(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	f.insertMessage(t, "brief-work-1", "chat", f.aliceID,
		time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`DELETE FROM person_sweep_work WHERE person_id = ?`), f.alicePersonID)
	requirements.NoError(err)

	published, err := f.store.EnsurePersonSweepWork(t.Context(), f.alicePersonID, true)
	requirements.NoError(err)
	checks.True(published)

	lease, err := f.store.ClaimPersonSweep(t.Context(), personSweepBriefClaim(f.alicePersonID))
	requirements.NoError(err)
	requirements.NotNil(lease, "the published work must be claimable now")
	checks.Equal(f.alicePersonID, lease.PersonID)

	var dirtyThrough int64
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT dirty_through_sequence FROM person_sweep_work WHERE person_id = ?`),
		f.alicePersonID).Scan(&dirtyThrough))
	checks.Positive(dirtyThrough, "the published row carries the person's journal high water")

	published, err = f.store.EnsurePersonSweepWork(t.Context(), f.bobPersonID, true)
	requirements.NoError(err)
	checks.False(published, "an untracked person never gets sweep work")

	_, err = f.store.EnsurePersonSweepWork(t.Context(), 0, true)
	requirements.Error(err)
}

func TestAutomaticBriefWorkPreservesRetryBackoff(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	lease, err := f.store.ClaimPersonSweep(t.Context(), personSweepBriefClaim(f.alicePersonID))
	require.NoError(err)
	require.NotNil(lease)
	require.NoError(f.store.FailPersonSweepWork(t.Context(), peoplesweep.WorkFailure{
		Lease: *lease, Class: peoplesweep.FailureProviderHTTP, RetryAt: time.Now().Add(time.Hour),
	}))
	_, err = f.store.EnsurePersonSweepWork(t.Context(), f.alicePersonID, false)
	require.NoError(err)
	deferred, err := f.store.ClaimPersonSweep(t.Context(), personSweepBriefClaim(f.alicePersonID))
	require.NoError(err)
	assert.Nil(deferred, "automatic brief work preserves the provider retry time")
	_, err = f.store.EnsurePersonSweepWork(t.Context(), f.alicePersonID, true)
	require.NoError(err)
	forced, err := f.store.ClaimPersonSweep(t.Context(), personSweepBriefClaim(f.alicePersonID))
	require.NoError(err)
	assert.NotNil(forced, "an explicit generation request still runs immediately")
}

// The brief's pre-call window is evaluated against the cadence the contact
// projection derives at read time from last_contact_at and the person's contact
// frequency attribute; there is no stored cadence column.
func TestPersonSweepCadenceDueAtReadsDerivedContactCadence(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f, personID := newActivityQueryFixture(t)
	last := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)

	_, hasDue, err := f.Store.PersonSweepCadenceDueAt(t.Context(), personID, last)
	requirements.NoError(err)
	checks.False(hasDue, "a person with no projected activity has no cadence")

	seedProjectedActivity(t, f, personID, []time.Time{last})
	days := int64(2)
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: store.AttributeSlugContactFrequency,
		Value:  store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days},
		Source: store.ProvenanceUser,
	})
	requirements.NoError(err)

	dueAt, hasDue, err := f.Store.PersonSweepCadenceDueAt(
		t.Context(), personID, last.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.True(hasDue)
	checks.True(last.AddDate(0, 0, 2).Equal(dueAt),
		"the due date is the last contact plus the contact frequency")

	_, _, err = f.Store.PersonSweepCadenceDueAt(t.Context(), 0, last)
	requirements.Error(err)
}

// TestPersonSweepHistoryReportsBriefFailureClass pins the operator-visible half
// of a brief failure: an attempt that succeeded with no brief still says why.
func TestPersonSweepHistoryReportsBriefFailureClass(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newPersonSweepBudgetFixture(t, "brief-history")
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_sweep_attempts SET status = 'succeeded', brief_failure_class = ?
		WHERE id = ?`), string(peoplesweep.FailureBudget), f.attemptID)
	requirements.NoError(err)

	attempts, err := f.store.ListPersonSweepAttempts(t.Context(),
		peoplesweep.AttemptFilter{PersonID: f.personID, Limit: 10})
	requirements.NoError(err)
	requirements.Len(attempts, 1)
	checks.Equal(peoplesweep.AttemptSucceeded, attempts[0].Status)
	checks.Empty(attempts[0].FailureClass)
	checks.Equal(peoplesweep.FailureBudget, attempts[0].BriefFailureClass)
}
