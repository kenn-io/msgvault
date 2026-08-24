package store

import (
	"bytes"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
)

func TestPersonEnrichmentMergeInvalidatesActivePreparedAttemptAndAcceptsFreshResult(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentResultFixture(t)
	prepared, err := f.store.preparePersonEnrichmentCommit(t.Context(), f.commit)
	require.NoError(err)
	participantID := enrichmentInvalidationParticipant(t, f.store, f.person.ID)
	survivorID, err := f.store.EnsureParticipant(
		"merge-survivor@example.test", "Merge Survivor", "example.test")
	require.NoError(err)

	require.NoError(f.store.MergeParticipants(participantID, survivorID))
	_, err = f.store.commitPreparedPersonEnrichmentResult(t.Context(), prepared)
	require.Error(err)
	assertEnrichmentInvalidationState(t, f, f.attempt.ID)

	fresh, _ := prepareCurrentEnrichmentResult(t, f, "after-merge")
	outcome, err := f.store.commitPreparedPersonEnrichmentResult(t.Context(), fresh)
	require.NoError(err)
	assert.Equal(personenrichment.ClaimApplied, outcome.Status)
	assert.Equal([]string{"Opaque/Person:Case?part=1"},
		enrichmentInvalidationProviderIDs(t, f.store, f.person.ID, f.profile.ProviderNamespace))
}

func TestPersonEnrichmentSplitInvalidatesActivePreparedAttemptAndAcceptsFreshResult(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentResultFixture(t)
	require.NoError(f.store.MarkTerminal(t.Context(), f.attempt.Token, personenrichment.SafeFailure{
		Class: personenrichment.FailureTerminal, Message: "replace fixture attempt",
	}))
	participantID := enrichmentInvalidationParticipant(t, f.store, f.person.ID)
	otherID, err := f.store.EnsureParticipant(
		"split-other@example.test", "Split Other", "example.test")
	require.NoError(err)
	_, err = f.store.LinkParticipants(participantID, otherID)
	require.NoError(err)
	require.NoError(f.store.PutPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerManual, Generation: "manual:before-split"},
		DueAt:   f.now,
	}))
	prepared, oldAttempt := prepareCurrentEnrichmentResult(t, f, "before-split")

	_, err = f.store.UnlinkParticipants(participantID, otherID)
	require.NoError(err)
	_, err = f.store.commitPreparedPersonEnrichmentResult(t.Context(), prepared)
	require.Error(err)
	assertEnrichmentInvalidationState(t, f, oldAttempt.ID)

	fresh, _ := prepareCurrentEnrichmentResult(t, f, "after-split")
	outcome, err := f.store.commitPreparedPersonEnrichmentResult(t.Context(), fresh)
	require.NoError(err)
	assert.Equal(personenrichment.ClaimApplied, outcome.Status)
	assert.Equal([]string{"Opaque/Person:Case?part=1"},
		enrichmentInvalidationProviderIDs(t, f.store, f.person.ID, f.profile.ProviderNamespace))
}

func TestPersonEnrichmentEmploymentAdvancesGenerationAndPreservesFreshCompanyWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentResultFixture(t)
	prepared, err := f.store.preparePersonEnrichmentCommit(t.Context(), f.commit)
	require.NoError(err)
	before := enrichmentInvalidationPerson(t, f.store, f.person.ID)
	organization, err := f.store.CreateOrganizationContext(t.Context(), OrganizationInput{
		Name: "Current Company", Kind: OrganizationKindCompany,
	})
	require.NoError(err)
	title := "Engineer"
	isCurrent := true
	employment, err := f.store.AddEmploymentContext(t.Context(), EmploymentInput{
		PersonID: f.person.ID, OrganizationID: organization.ID, Title: &title,
		IsCurrent: &isCurrent, Source: ProvenanceUser,
	})
	require.NoError(err)
	afterAdd := enrichmentInvalidationPerson(t, f.store, f.person.ID)
	assert.Greater(afterAdd.Revision, before.Revision)

	newTitle := "Senior Engineer"
	_, err = f.store.UpdateEmploymentContext(t.Context(), employment.ID, employment.Revision, EmploymentInput{
		PersonID: f.person.ID, OrganizationID: organization.ID, Title: &newTitle,
		IsCurrent: &isCurrent, Source: ProvenanceUser,
	})
	require.NoError(err)
	afterUpdate := enrichmentInvalidationPerson(t, f.store, f.person.ID)
	assert.Greater(afterUpdate.Revision, afterAdd.Revision)

	_, err = f.store.commitPreparedPersonEnrichmentResult(t.Context(), prepared)
	require.Error(err)
	require.NoError(f.store.MarkTerminal(t.Context(), f.attempt.Token, personenrichment.SafeFailure{
		Class: personenrichment.FailureInvalidOutput, Message: "stale person generation",
	}))
	run, _, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: "employment-current-company", RequestedAt: f.now,
	})
	require.NoError(err)
	lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "employment-worker", ProviderName: f.profile.Name,
		Now: f.now, LeaseDuration: time.Minute,
	})
	require.NoError(err)
	require.NotNil(lease)
	assert.Equal(personenrichment.TriggerIdentity, lease.Trigger.Kind)
	assert.Equal("revision:"+formatPersonEnrichmentInt(afterUpdate.Revision), lease.Trigger.Generation)
	input, err := f.store.LoadRequestInput(t.Context(), *lease)
	require.NoError(err)
	require.Len(input.CurrentCompanies, 1)
	assert.Equal("Current Company", input.CurrentCompanies[0].Value)
}

func TestPersonEnrichmentSuccessfulAttemptPreservesFreshTriggerAlongsideRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentResultFixture(t)
	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Kind: personenrichment.TriggerManual, Generation: "manual:result-fixture", DueAt: f.now,
	}))
	idempotentRows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	require.NoError(err)
	require.Len(idempotentRows, 1)
	assert.False(idempotentRows[0].HasFreshTrigger)
	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Kind: personenrichment.TriggerExpiry, Generation: "claim:999", DueAt: f.now,
	}))
	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Kind: personenrichment.TriggerManual, Generation: "manual:result-fixture", DueAt: f.now,
	}))

	outcome, err := f.store.CommitEnrichmentClaims(t.Context(), f.commit)
	require.NoError(err)
	assert.Equal(personenrichment.ClaimApplied, outcome.Status)
	rows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Nil(rows[0].RunID)
	assert.Nil(rows[0].ActiveAttemptID)
	assert.False(rows[0].HasFreshTrigger)
	assert.Equal(
		personEnrichmentTriggerBit(personenrichment.TriggerExpiry)|
			personEnrichmentTriggerBit(personenrichment.TriggerRefresh),
		rows[0].TriggerMask,
	)
	assert.Equal("claim:999", rows[0].TriggerGeneration)
}

func TestPersonEnrichmentTerminalAttemptDoesNotResurrectConsumedTrigger(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentResultFixture(t)
	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Kind: personenrichment.TriggerExpiry, Generation: "claim:terminal-fresh", DueAt: f.now,
	}))
	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Kind: personenrichment.TriggerManual, Generation: "manual:result-fixture", DueAt: f.now,
	}))
	require.NoError(f.store.MarkTerminal(t.Context(), f.attempt.Token, personenrichment.SafeFailure{
		Class: personenrichment.FailureTerminal, Message: "synthetic terminal",
	}))
	rows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(personEnrichmentTriggerBit(personenrichment.TriggerExpiry), rows[0].TriggerMask)
	assert.Equal("claim:terminal-fresh", rows[0].TriggerGeneration)
}

func TestPersonEnrichmentClaimedPublicationFencesBeginAndPreservesExactWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentResultFixture(t)
	require.NoError(f.store.MarkTerminal(t.Context(), f.attempt.Token, personenrichment.SafeFailure{
		Class: personenrichment.FailureTerminal, Message: "replace fixture attempt",
	}))
	require.NoError(f.store.PutPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Trigger: personenrichment.Trigger{
			Kind: personenrichment.TriggerManual, Generation: "manual:claimed-before-change",
		},
		DueAt: f.now,
	}))
	run, _, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: "claimed-publication-fence", RequestedAt: f.now,
	})
	require.NoError(err)
	lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "claimed-publication-worker", ProviderName: f.profile.Name,
		Now: f.now, LeaseDuration: time.Minute,
	})
	require.NoError(err)
	require.NotNil(lease)
	input, err := f.store.LoadRequestInput(t.Context(), *lease)
	require.NoError(err)
	_, hashes, err := personenrichment.BuildRequest(input, f.profile)
	require.NoError(err)

	organization, err := f.store.CreateOrganizationContext(t.Context(), OrganizationInput{
		Name: "Claimed Fresh Company", Kind: OrganizationKindCompany,
	})
	require.NoError(err)
	isCurrent := true
	_, err = f.store.AddEmploymentContext(t.Context(), EmploymentInput{
		PersonID: f.person.ID, OrganizationID: organization.ID, Title: new("Engineer"),
		IsCurrent: &isCurrent, Source: ProvenanceUser,
	})
	require.NoError(err)
	current := enrichmentInvalidationPerson(t, f.store, f.person.ID)

	attempt, created, err := f.store.BeginAttempt(t.Context(), lease.Token, personenrichment.AttemptStart{
		RunID: run.ID, PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		PayloadHash: hashes.PayloadHash, RequestHash: hashes.RequestHash,
		PersonRevision: input.PersonRevision, Trigger: lease.Trigger,
	})
	require.ErrorIs(err, ErrStaleLease)
	assert.Nil(attempt)
	assert.False(created)
	counters, err := f.store.GetPersonEnrichmentRunCountersContext(t.Context(), run.ID)
	require.NoError(err)
	assert.Equal(PersonEnrichmentRunCounters{}, counters)
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), PersonEnrichmentAttemptFilter{
		RunID: run.ID, Limit: 10,
	})
	require.NoError(err)
	assert.Empty(attempts)
	rows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Nil(rows[0].RunID)
	assert.Nil(rows[0].LeaseOwner)
	assert.Equal(personEnrichmentTriggerBit(personenrichment.TriggerIdentity), rows[0].TriggerMask)
	assert.Equal("revision:"+formatPersonEnrichmentInt(current.Revision), rows[0].TriggerGeneration)
}

func TestPersonEnrichmentMergeSplitSerializeConcurrentBeginBeforeInvalidation(t *testing.T) {
	for _, mutation := range []string{"merge", "split"} {
		t.Run(mutation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newEnrichmentResultFixture(t)
			require.NoError(f.store.MarkTerminal(t.Context(), f.attempt.Token, personenrichment.SafeFailure{
				Class: personenrichment.FailureTerminal, Message: "replace fixture attempt",
			}))
			participantID := enrichmentInvalidationParticipant(t, f.store, f.person.ID)
			var mutate func() error
			switch mutation {
			case "merge":
				survivorID, err := f.store.EnsureParticipant(
					"race-merge-survivor@example.test", "Race Merge Survivor", "example.test")
				require.NoError(err)
				mutate = func() error { return f.store.MergeParticipants(participantID, survivorID) }
			case "split":
				otherID, err := f.store.EnsureParticipant(
					"race-split-other@example.test", "Race Split Other", "example.test")
				require.NoError(err)
				_, err = f.store.LinkParticipants(participantID, otherID)
				require.NoError(err)
				mutate = func() error {
					_, unlinkErr := f.store.UnlinkParticipants(participantID, otherID)
					return unlinkErr
				}
			}
			lease, start, runID := claimUnboundInvalidationWork(t, f, "race-"+mutation)

			invalidationLocked := make(chan struct{}, 1)
			releaseInvalidation := make(chan struct{})
			beginBeforeLock := make(chan struct{}, 1)
			beginPersonLocked := make(chan struct{}, 1)
			SetPersonEnrichmentTxBarrierForTest(f.store, func(phase string) {
				switch phase {
				case "invalidation_person_locked":
					invalidationLocked <- struct{}{}
					<-releaseInvalidation
				case "begin_before_person_lock":
					beginBeforeLock <- struct{}{}
				case "begin_person_locked":
					beginPersonLocked <- struct{}{}
				}
			})
			mutationDone := make(chan error, 1)
			go func() { mutationDone <- mutate() }()
			requireReceiveEnrichmentBarrier(t, invalidationLocked, "invalidation did not lock person")
			type beginResult struct {
				attempt *personenrichment.DurableAttempt
				created bool
				err     error
			}
			beginDone := make(chan beginResult, 1)
			go func() {
				attempt, created, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
				beginDone <- beginResult{attempt: attempt, created: created, err: err}
			}()
			requireReceiveEnrichmentBarrier(t, beginBeforeLock, "BeginAttempt did not reach person gate")
			close(releaseInvalidation)
			select {
			case <-beginPersonLocked:
				// The required person gate was acquired before BeginAttempt touched work.
			case result := <-beginDone:
				require.Fail("BeginAttempt bypassed person gate", "result: %+v", result)
			case <-time.After(5 * time.Second):
				require.Fail("BeginAttempt did not acquire person gate after invalidation")
			}
			require.NoError(<-mutationDone)
			result := <-beginDone
			require.ErrorIs(result.err, ErrStaleLease)
			assert.Nil(result.attempt)
			assert.False(result.created)
			assertNoInvalidationRaceOrphan(t, f, runID)
		})
	}
}

func claimUnboundInvalidationWork(
	t *testing.T, f *enrichmentResultFixture, suffix string,
) (*personenrichment.WorkLease, personenrichment.AttemptStart, int64) {
	t.Helper()
	require.NoError(t, f.store.PutPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Trigger: personenrichment.Trigger{
			Kind: personenrichment.TriggerManual, Generation: "manual:" + suffix,
		},
		DueAt: f.now,
	}))
	run, _, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: suffix, RequestedAt: f.now,
	})
	require.NoError(t, err)
	lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "unbound-" + suffix, ProviderName: f.profile.Name,
		Now: f.now, LeaseDuration: time.Minute,
	})
	require.NoError(t, err)
	require.NotNil(t, lease)
	input, err := f.store.LoadRequestInput(t.Context(), *lease)
	require.NoError(t, err)
	_, hashes, err := personenrichment.BuildRequest(input, f.profile)
	require.NoError(t, err)
	return lease, personenrichment.AttemptStart{
		RunID: run.ID, PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		PayloadHash: hashes.PayloadHash, RequestHash: hashes.RequestHash,
		PersonRevision: input.PersonRevision, Trigger: lease.Trigger,
	}, run.ID
}

func requireReceiveEnrichmentBarrier(t *testing.T, barrier <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-barrier:
	case <-time.After(5 * time.Second):
		require.Fail(t, message)
	}
}

func assertNoInvalidationRaceOrphan(
	t *testing.T, f *enrichmentResultFixture, runID int64,
) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), PersonEnrichmentAttemptFilter{
		RunID: runID, Limit: 10,
	})
	require.NoError(err)
	assert.Empty(attempts)
	counters, err := f.store.GetPersonEnrichmentRunCountersContext(t.Context(), runID)
	require.NoError(err)
	assert.Equal(PersonEnrichmentRunCounters{}, counters)
	rows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Nil(rows[0].RunID)
	assert.Nil(rows[0].ActiveAttemptID)
	assert.Equal(personEnrichmentTriggerBit(personenrichment.TriggerIdentity), rows[0].TriggerMask)
	person := enrichmentInvalidationPerson(t, f.store, f.person.ID)
	assert.Equal("revision:"+formatPersonEnrichmentInt(person.Revision), rows[0].TriggerGeneration)
}

func prepareCurrentEnrichmentResult(
	t *testing.T, f *enrichmentResultFixture, suffix string,
) (*preparedEnrichmentCommit, *personenrichment.DurableAttempt) {
	t.Helper()
	person := enrichmentInvalidationPerson(t, f.store, f.person.ID)
	run, _, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: "invalidation-" + suffix, RequestedAt: f.now,
	})
	require.NoError(t, err)
	lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "invalidation-worker-" + suffix, ProviderName: f.profile.Name,
		Now: f.now, LeaseDuration: time.Minute,
	})
	require.NoError(t, err)
	require.NotNil(t, lease)
	input, err := f.store.LoadRequestInput(t.Context(), *lease)
	require.NoError(t, err)
	_, hashes, err := personenrichment.BuildRequest(input, f.profile)
	require.NoError(t, err)
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, personenrichment.AttemptStart{
		RunID: run.ID, PersonID: person.ID, ProfileFingerprint: f.profile.Fingerprint,
		PayloadHash: hashes.PayloadHash, RequestHash: hashes.RequestHash,
		PersonRevision: person.Revision, Trigger: lease.Trigger,
	})
	require.NoError(t, err)
	result := f.result
	result.RequestID = "request-" + suffix
	result.JobID = "job-" + suffix
	programFingerprint, err := personenrichment.ProgramFingerprint(personenrichment.ProgramDescriptor{
		HostMappingVersion: personenrichment.HostClaimMappingVersion,
		AdapterVersion:     result.AdapterVersion, WireSchemaVersion: result.SchemaVersion,
	})
	require.NoError(t, err)
	require.NoError(t, f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
		State: personenrichment.AttemptPending, RequestID: result.RequestID, JobID: result.JobID,
		StartedAt: f.now, AdapterVersion: result.AdapterVersion, SchemaVersion: result.SchemaVersion,
		ProgramFingerprint: programFingerprint,
	}))
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x6a}, 32))
	require.NoError(t, err)
	commit, err := personenrichment.NewClaimCommit(personenrichment.ClaimCommitInput{
		AttemptID: attempt.ID, RunID: run.ID, PersonID: person.ID,
		LeaseFence: attempt.Token.Fence, ProfileFingerprint: f.profile.Fingerprint,
		ProviderNamespace: f.profile.ProviderNamespace, RequestHash: hashes.RequestHash,
		IdentityAssessment: f.commit.IdentityAssessment,
	}, result, hasher)
	require.NoError(t, err)
	prepared, err := f.store.preparePersonEnrichmentCommit(t.Context(), commit)
	require.NoError(t, err)
	return prepared, attempt
}

func assertEnrichmentInvalidationState(
	t *testing.T, f *enrichmentResultFixture, attemptID int64,
) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	attempt, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), attemptID)
	require.NoError(err)
	assert.Equal("terminal", attempt.State)
	rows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Nil(rows[0].RunID)
	assert.Nil(rows[0].ActiveAttemptID)
	assert.Equal(personEnrichmentTriggerBit(personenrichment.TriggerIdentity), rows[0].TriggerMask)
	person := enrichmentInvalidationPerson(t, f.store, f.person.ID)
	assert.Equal("revision:"+formatPersonEnrichmentInt(person.Revision), rows[0].TriggerGeneration)
}

func enrichmentInvalidationParticipant(t *testing.T, st *Store, personID int64) int64 {
	t.Helper()
	var participantID int64
	require.NoError(t, st.db.QueryRowContext(t.Context(), `
		SELECT participant_id FROM person_participants WHERE person_id = ? ORDER BY participant_id LIMIT 1`,
		personID).Scan(&participantID))
	return participantID
}

func enrichmentInvalidationPerson(t *testing.T, st *Store, personID int64) *Person {
	t.Helper()
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(t, err)
	return person
}

func enrichmentInvalidationProviderIDs(
	t *testing.T, st *Store, personID int64, namespace string,
) []string {
	t.Helper()
	ids, err := st.LoadProviderPersonIDs(t.Context(), personID, namespace)
	require.NoError(t, err)
	return ids
}

func personEnrichmentTriggerBit(kind personenrichment.TriggerKind) int64 {
	mask, err := personEnrichmentTriggerMask(kind)
	if err != nil {
		panic(err)
	}
	return mask
}

func formatPersonEnrichmentInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
