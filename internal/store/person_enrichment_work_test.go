package store_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type enrichmentWorkFixture struct {
	store   *store.Store
	person  *store.Person
	profile personenrichment.ProviderProfile
	now     time.Time
}

func newEnrichmentWorkFixture(t *testing.T) enrichmentWorkFixture {
	t.Helper()
	fixture := storetest.New(t)
	participantID := fixture.EnsureParticipant("work-person@example.com", "Work Person", "example.com")
	person, _, err := fixture.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(t, err)
	profile := enrichmentTestProfile(t)
	_, err = fixture.Store.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(t, err)
	_, _, err = fixture.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(t, err)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	store.SetPersonEnrichmentClockForTest(fixture.Store, func() time.Time { return now })
	_, err = fixture.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(t, err)
	_, err = fixture.Store.DB().ExecContext(t.Context(), fixture.Store.Rebind(
		`DELETE FROM person_enrichment_work WHERE person_id = ?`), person.ID)
	require.NoError(t, err)
	return enrichmentWorkFixture{store: fixture.Store, person: person, profile: profile, now: now}
}

func (f *enrichmentWorkFixture) setNow(t time.Time) {
	f.now = t
	store.SetPersonEnrichmentClockForTest(f.store, func() time.Time { return f.now })
}

func (f *enrichmentWorkFixture) startRun(t *testing.T, key string) personenrichment.DurableRun {
	t.Helper()
	run, created, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "scheduled", RequestedBy: key, RequestedAt: f.now,
	})
	require.NoError(t, err)
	require.True(t, created)
	return *run
}

func (f *enrichmentWorkFixture) enqueue(t *testing.T) {
	t.Helper()
	require.NoError(t, f.store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "revision:1"},
		DueAt:   f.now,
	}))
}

func (f *enrichmentWorkFixture) claim(t *testing.T, runID int64, owner string) *personenrichment.WorkLease {
	t.Helper()
	lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: runID, Owner: owner, ProviderName: f.profile.Name,
		Now: f.now, LeaseDuration: 5 * time.Minute,
	})
	require.NoError(t, err)
	require.NotNil(t, lease)
	return lease
}

func (f *enrichmentWorkFixture) work(t *testing.T) []store.PersonEnrichmentWork {
	t.Helper()
	rows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	require.NoError(t, err)
	return rows
}

func testAttemptStart(f *enrichmentWorkFixture, runID int64, hashByte string) personenrichment.AttemptStart {
	return personenrichment.AttemptStart{
		RunID: runID, PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		PayloadHash: strings.Repeat(hashByte, 64), RequestHash: strings.Repeat(hashByte, 64),
		PersonRevision:    f.person.Revision,
		Trigger:           personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "revision:1"},
		GuaranteedMaxCost: personenrichment.Cost{},
	}
}

func TestPersonEnrichmentBeginAttemptRejectsConcurrentConfiguredKeyWinner(t *testing.T) {
	for _, winner := range []string{"suppression", "deletion"} {
		t.Run(winner, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newEnrichmentWorkFixture(t)
			run := f.startRun(t, "key-state-race-"+winner)
			f.enqueue(t)
			lease := f.claim(t, run.ID, "key-state-worker")
			oldHasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x6a}, 32))
			require.NoError(err)
			newHasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x4e}, 32))
			require.NoError(err)
			oldDigest := oldHasher.Digest(f.profile.ProviderNamespace,
				personenrichment.SuppressionEmail, personenrichment.EmailNormalizationV1,
				"work-person@example.com")
			newDigest := newHasher.Digest(f.profile.ProviderNamespace,
				personenrichment.SuppressionEmail, personenrichment.EmailNormalizationV1,
				"configured-winner@example.test")
			newKeyID, err := newHasher.KeyID()
			require.NoError(err)

			var deletePerson *store.Person
			if winner == "deletion" {
				participantID, createErr := f.store.EnsureParticipant(
					"delete-key-winner@example.test", "Delete Key Winner", "example.test")
				require.NoError(createErr)
				deletePerson, _, createErr = f.store.CreatePersonFromParticipantContext(
					t.Context(), participantID)
				require.NoError(createErr)
			}

			beginReached := make(chan struct{})
			releaseBegin := make(chan struct{})
			store.SetPersonEnrichmentTxBarrierForTest(f.store, func(phase string) {
				if phase == "begin_before_authority_lock" {
					close(beginReached)
					<-releaseBegin
				}
			})
			type beginResult struct {
				attempt *personenrichment.DurableAttempt
				created bool
				err     error
			}
			result := make(chan beginResult, 1)
			go func() {
				start := testAttemptStart(&f, run.ID, "d")
				start.CheckedIdentifiers = []personenrichment.SuppressionDigest{oldDigest}
				attempt, created, beginErr := f.store.BeginAttempt(t.Context(), lease.Token, start)
				result <- beginResult{attempt: attempt, created: created, err: beginErr}
			}()
			requireChannelSignal(t, beginReached, "begin attempt did not reach its person-first gate")
			input := store.PersonEnrichmentSuppressionInput{
				ProviderNamespace: newDigest.ProviderNamespace, IdentifierClass: newDigest.IdentifierClass,
				NormalizationVersion: newDigest.NormalizationVersion, KeyID: newDigest.KeyID,
				Digest: newDigest.Digest, Reason: store.PersonEnrichmentSuppressionDeletion,
				Actor: "privacy-test",
			}
			if winner == "suppression" {
				require.NoError(f.store.InsertPersonEnrichmentSuppressionsForConfiguredKeyContext(
					t.Context(), newKeyID, []store.PersonEnrichmentSuppressionInput{input}))
			} else {
				require.NoError(f.store.DeletePersonWithEnrichmentSuppressionsContext(
					t.Context(), store.DeletePersonEnrichmentInput{
						PersonID: deletePerson.ID, ExpectedRevision: deletePerson.Revision,
						ConfiguredKeyID: newKeyID, Actor: "privacy-test",
						Reason:             store.PersonEnrichmentSuppressionDeletion,
						CurrentIdentifiers: []store.PersonEnrichmentSuppressionInput{input},
					}))
			}
			close(releaseBegin)
			got := <-result
			require.ErrorIs(got.err, personenrichment.ErrSuppressionKeyMismatch)
			assert.Nil(got.attempt)
			assert.False(got.created)
			var attempts, identifiers int64
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
				`SELECT COUNT(*) FROM person_enrichment_attempts WHERE run_id = ?`), run.ID).Scan(&attempts))
			require.NoError(f.store.DB().QueryRowContext(t.Context(),
				`SELECT COUNT(*) FROM person_enrichment_attempt_identifiers`).Scan(&identifiers))
			assert.Zero(attempts)
			assert.Zero(identifiers)
			if winner == "deletion" {
				_, getErr := f.store.GetPersonContext(t.Context(), deletePerson.ID)
				assert.ErrorIs(getErr, store.ErrPersonNotFound)
			}
		})
	}
}

func TestPersonEnrichmentDispatchAuthorizationIsFencedByConsentRevocation(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "dispatch-consent")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "dispatch-consent-worker")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token,
		testAttemptStart(&f, run.ID, "d"))
	requirements.NoError(err)

	requirements.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
	var authorized bool
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT dispatch_authorized_at IS NOT NULL FROM person_enrichment_attempts WHERE id = ?`),
		attempt.ID).Scan(&authorized))
	checks.True(authorized)

	changed, err := f.store.RevokePersonEnrichmentConsent(
		t.Context(), f.profile.Fingerprint, "dispatch-test")
	requirements.ErrorIs(err, store.ErrPersonEnrichmentDispatchInProgress)
	checks.False(changed)
	requirements.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT dispatch_authorized_at IS NOT NULL FROM person_enrichment_attempts WHERE id = ?`),
		attempt.ID).Scan(&authorized))
	checks.True(authorized)
	requirements.NoError(f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
		State: personenrichment.AttemptPending, RequestID: "opaque-request", JobID: "opaque-job",
		StartedAt: f.now, AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
		ProgramFingerprint: strings.Repeat("b", 64),
	}))
	changed, err = f.store.RevokePersonEnrichmentConsent(
		t.Context(), f.profile.Fingerprint, "dispatch-test")
	requirements.NoError(err)
	checks.True(changed)
	requirements.ErrorIs(
		f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token),
		personenrichment.ErrConsentRequired)
}

func TestPersonEnrichmentDispatchAuthorizationRejectsStaleIdentityRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "dispatch-stale-identity")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "dispatch-stale-identity-worker")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token,
		testAttemptStart(&f, run.ID, "e"))
	require.NoError(err)

	name := "Changed Work Person"
	_, err = f.store.AddPersonNameContext(t.Context(), f.person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: &name, OriginalValue: name,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	require.ErrorIs(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token), store.ErrStaleLease)
	stored, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
	require.NoError(err)
	assert.Equal("terminal", stored.State)
	work := f.work(t)
	require.Len(work, 1)
	assert.Nil(work[0].RunID)
	assert.Nil(work[0].ActiveAttemptID)
	assert.False(work[0].HasFreshTrigger)
}

func TestPersonEnrichmentProfileCleanupRejectsAuthorizedDispatch(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "profile-cleanup-dispatch")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "profile-cleanup-worker")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token,
		testAttemptStart(&f, run.ID, "f"))
	requirements.NoError(err)
	requirements.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))

	requirements.ErrorIs(
		f.store.CancelPersonEnrichmentWorkOutsideProfilesContext(t.Context(), nil),
		store.ErrPersonEnrichmentDispatchInProgress,
	)
	stored, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
	requirements.NoError(err)
	checks.Equal("starting", stored.State)
	work := f.work(t)
	requirements.Len(work, 1)
	requirements.NotNil(work[0].ActiveAttemptID)
	checks.Equal(attempt.ID, *work[0].ActiveAttemptID)
}

func TestPersonEnrichmentProfileIdentityMutationInvalidatesProviderBindingAndAttempt(t *testing.T) {
	for _, mutation := range []string{"add_name", "supersede_name", "add_contact", "supersede_contact"} {
		t.Run(mutation, func(t *testing.T) {
			requirements := require.New(t)
			checks := assert.New(t)
			f := newEnrichmentWorkFixture(t)
			var mutate func() error
			switch mutation {
			case "add_name":
				mutate = func() error {
					name := "Changed Work Person"
					_, err := f.store.AddPersonNameContext(t.Context(), f.person.ID, store.PersonNameInput{
						NameKind: store.PersonNameFormatted, Formatted: &name, OriginalValue: name,
						Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
					})
					return err
				}
			case "supersede_name":
				name := "Historical Work Person"
				created, err := f.store.AddPersonNameContext(t.Context(), f.person.ID, store.PersonNameInput{
					NameKind: store.PersonNameFormatted, Formatted: &name, OriginalValue: name,
					Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
				requirements.NoError(err)
				mutate = func() error {
					return f.store.SupersedePersonNameContext(
						t.Context(), f.person.ID, created.Envelope.ID, nil)
				}
			case "add_contact":
				mutate = func() error {
					_, err := f.store.AddPersonContactPointContext(t.Context(), f.person.ID,
						store.PersonContactPointInput{
							AddressKind: store.ContactAddressEmail, OriginalValue: "changed-work@example.test",
							Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
						})
					return err
				}
			case "supersede_contact":
				created, err := f.store.AddPersonContactPointContext(t.Context(), f.person.ID,
					store.PersonContactPointInput{
						AddressKind: store.ContactAddressEmail, OriginalValue: "historical-work@example.test",
						Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
					})
				requirements.NoError(err)
				mutate = func() error {
					return f.store.SupersedePersonContactPointContext(
						t.Context(), f.person.ID, created.Envelope.ID, nil)
				}
			}
			requirements.NoError(clearEnrichmentWork(t, f.store, f.person.ID))
			var err error
			f.person, err = f.store.GetPersonContext(t.Context(), f.person.ID)
			requirements.NoError(err)
			insertProviderIdentity(t, f.store, f.person.ID,
				f.profile.ProviderNamespace, "opaque-before-"+mutation)
			run := f.startRun(t, "identity-mutation-"+mutation)
			f.enqueue(t)
			lease := f.claim(t, run.ID, "identity-mutation-worker")
			attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token,
				testAttemptStart(&f, run.ID, "6"))
			requirements.NoError(err)

			requirements.NoError(mutate())
			checks.Zero(providerIdentityCount(t, f.store, f.person.ID))
			stored, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
			requirements.NoError(err)
			checks.Equal("terminal", stored.State)
			work := f.work(t)
			requirements.Len(work, 1)
			checks.Nil(work[0].RunID)
			checks.Nil(work[0].ActiveAttemptID)
			checks.Equal(int64(2), work[0].TriggerMask)
			requirements.ErrorIs(
				f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token), store.ErrStaleLease)
		})
	}
}

func TestPersonEnrichmentUntrackingFencesLeasedAttemptBeforeDispatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	run := f.startRun(t, "dispatch-untracking")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "dispatch-untracking-worker")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token,
		testAttemptStart(&f, run.ID, "7"))
	require.NoError(err)

	_, err = f.store.SetPersonTrackingContext(t.Context(), f.person.ID, false)
	require.NoError(err)
	stored, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
	require.NoError(err)
	assert.Equal("terminal", stored.State)
	assert.Empty(f.work(t))
	require.ErrorIs(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token), store.ErrStaleLease)
}

func TestPersonEnrichmentRetrackingCreatesFreshAttempt(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, false)
	requirements.NoError(err)
	_, err = f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	requirements.NoError(err)
	run := f.startRun(t, "retracking")
	lease := f.claim(t, run.ID, "worker-a")
	payloadHash := strings.Repeat("7", 64)
	requestHash, err := personenrichment.RequestHash(f.person.ID, payloadHash, lease.Trigger)
	requirements.NoError(err)
	first, created, err := f.store.BeginAttempt(t.Context(), lease.Token, personenrichment.AttemptStart{
		RunID: run.ID, PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		PayloadHash: payloadHash, RequestHash: requestHash,
		PersonRevision: f.person.Revision, Trigger: lease.Trigger,
	})
	requirements.NoError(err)
	checks.True(created)

	_, err = f.store.SetPersonTrackingContext(t.Context(), f.person.ID, false)
	requirements.NoError(err)
	_, err = f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	requirements.NoError(err)
	retrackedLease := f.claim(t, run.ID, "worker-b")
	checks.NotEqual(lease.Trigger.Generation, retrackedLease.Trigger.Generation)
	retrackedHash, err := personenrichment.RequestHash(
		f.person.ID, payloadHash, retrackedLease.Trigger)
	requirements.NoError(err)
	checks.NotEqual(requestHash, retrackedHash)
	second, created, err := f.store.BeginAttempt(t.Context(), retrackedLease.Token,
		personenrichment.AttemptStart{
			RunID: run.ID, PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
			PayloadHash: payloadHash, RequestHash: retrackedHash,
			PersonRevision: f.person.Revision, Trigger: retrackedLease.Trigger,
		})
	requirements.NoError(err)
	checks.True(created)
	checks.NotEqual(first.ID, second.ID)
}

func TestPersonEnrichmentSuppressionFencesLeasedAttemptBeforeDispatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	run := f.startRun(t, "dispatch-suppression")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "dispatch-suppression-worker")
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x74}, 32))
	require.NoError(err)
	digest := hasher.Digest(f.profile.ProviderNamespace,
		personenrichment.SuppressionEmail, personenrichment.EmailNormalizationV1,
		"work-person@example.com")
	start := testAttemptStart(&f, run.ID, "8")
	start.CheckedIdentifiers = []personenrichment.SuppressionDigest{digest}
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
	require.NoError(err)

	require.NoError(f.store.InsertPersonEnrichmentSuppressionsContext(t.Context(),
		[]store.PersonEnrichmentSuppressionInput{{
			ProviderNamespace: digest.ProviderNamespace, IdentifierClass: digest.IdentifierClass,
			NormalizationVersion: digest.NormalizationVersion, KeyID: digest.KeyID,
			Digest: digest.Digest, Reason: store.PersonEnrichmentSuppressionOptOut,
			Actor: "privacy-test",
		}}))
	stored, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
	require.NoError(err)
	assert.Equal("terminal", stored.State)
	assert.Empty(f.work(t))
	require.ErrorIs(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token), store.ErrStaleLease)
}

func TestPersonEnrichmentSuppressionSerializesAttemptCreation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL exercises concurrent transactions")
	}
	run := f.startRun(t, "concurrent-suppression")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "concurrent-suppression-worker")
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x76}, 32))
	require.NoError(err)
	digest := hasher.Digest(f.profile.ProviderNamespace,
		personenrichment.SuppressionEmail, personenrichment.EmailNormalizationV1,
		"work-person@example.com")
	start := testAttemptStart(&f, run.ID, "a")
	start.CheckedIdentifiers = []personenrichment.SuppressionDigest{digest}

	beginRechecked := make(chan struct{})
	releaseBegin := make(chan struct{})
	suppressionSnapshotted := make(chan struct{})
	store.SetPersonEnrichmentTxBarrierForTest(f.store, func(phase string) {
		switch phase {
		case "begin_suppressions_rechecked":
			close(beginRechecked)
			<-releaseBegin
		case "suppression_affected_people_snapshotted":
			close(suppressionSnapshotted)
		}
	})
	type beginResult struct {
		attempt *personenrichment.DurableAttempt
		created bool
		err     error
	}
	beginDone := make(chan beginResult, 1)
	go func() {
		attempt, created, beginErr := f.store.BeginAttempt(t.Context(), lease.Token, start)
		beginDone <- beginResult{attempt: attempt, created: created, err: beginErr}
	}()
	requireChannelSignal(t, beginRechecked, "begin did not recheck suppressions")
	suppressionDone := make(chan error, 1)
	go func() {
		suppressionDone <- f.store.InsertPersonEnrichmentSuppressionsContext(t.Context(),
			[]store.PersonEnrichmentSuppressionInput{{
				ProviderNamespace: digest.ProviderNamespace, IdentifierClass: digest.IdentifierClass,
				NormalizationVersion: digest.NormalizationVersion, KeyID: digest.KeyID,
				Digest: digest.Digest, Reason: store.PersonEnrichmentSuppressionOptOut,
				Actor: "privacy-test",
			}})
	}()
	select {
	case <-suppressionSnapshotted:
		require.Fail("suppression acquired authority before attempt creation released it")
	case <-time.After(250 * time.Millisecond):
	}
	close(releaseBegin)
	result := <-beginDone
	require.NoError(result.err)
	require.True(result.created)
	require.NotNil(result.attempt)
	requireChannelSignal(t, suppressionSnapshotted,
		"suppression did not snapshot the created attempt")
	require.NoError(<-suppressionDone)
	stored, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), result.attempt.ID)
	require.NoError(err)
	assert.Equal("terminal", stored.State)
	assert.Empty(f.work(t))
}

func TestPersonEnrichmentBeginAttemptRechecksCheckedIdentifierSuppression(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	run := f.startRun(t, "begin-suppression")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "begin-suppression-worker")
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x75}, 32))
	require.NoError(err)
	digest := hasher.Digest(f.profile.ProviderNamespace,
		personenrichment.SuppressionEmail, personenrichment.EmailNormalizationV1,
		"work-person@example.com")
	require.NoError(f.store.InsertPersonEnrichmentSuppressionsContext(t.Context(),
		[]store.PersonEnrichmentSuppressionInput{{
			ProviderNamespace: digest.ProviderNamespace, IdentifierClass: digest.IdentifierClass,
			NormalizationVersion: digest.NormalizationVersion, KeyID: digest.KeyID,
			Digest: digest.Digest, Reason: store.PersonEnrichmentSuppressionOptOut,
			Actor: "privacy-test",
		}}))
	start := testAttemptStart(&f, run.ID, "9")
	start.CheckedIdentifiers = []personenrichment.SuppressionDigest{digest}

	attempt, created, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
	require.ErrorIs(err, personenrichment.ErrSuppressed)
	assert.Nil(attempt)
	assert.False(created)
}

func TestPersonEnrichmentLeaseRejectsStaleWorkerAfterReclaim(t *testing.T) {
	require := require.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "2026-08-22T12:00:00Z")
	f.enqueue(t)
	first := f.claim(t, run.ID, "worker-a")

	f.setNow(first.LeaseUntil.Add(time.Nanosecond))
	second := f.claim(t, run.ID, "worker-b")
	assert.Greater(t, second.Token.Fence, first.Token.Fence)
	err := f.store.ReleaseWork(t.Context(), first.Token, personenrichment.WorkRelease{Outcome: "complete"})
	require.ErrorIs(err, store.ErrStaleLease)
}

func TestPersonEnrichmentClaimReturnsBoundActiveAttempt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "bound-attempt")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, created, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "a"))
	require.NoError(err)
	assert.True(created)
	assert.True(attempt.StartedAt.IsZero())
	target := durableAttemptTarget(t)
	providerStartedAt := f.now.Add(90 * time.Second)
	require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
	require.NoError(f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
		State: personenrichment.AttemptPending, RequestID: "opaque-request", JobID: "opaque-job-42",
		StartedAt:      providerStartedAt,
		AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
		GeneratedSchema: true, GeneratedSchemaHash: strings.Repeat("c", 64),
		ProgramFingerprint: strings.Repeat("b", 64), Targets: []personfacts.TargetDescriptor{target},
	}))

	// A later terminal row must never be inferred as the resumable attempt.
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_enrichment_attempts
			(run_id, person_id, profile_fingerprint, trigger_kind, trigger_generation,
			 person_revision, payload_hash, request_hash, state, lease_fence,
			 hard_cost_cap_enforced, reserved_cost_usd_micros, completed_at)
		VALUES (?, ?, ?, 'tracked', 'diagnostic', ?, ?, ?, 'terminal', 0, FALSE, 0, ?)`),
		run.ID, f.person.ID, f.profile.Fingerprint, f.person.Revision,
		strings.Repeat("c", 64), strings.Repeat("d", 64), f.now)
	require.NoError(err)

	f.setNow(lease.LeaseUntil.Add(time.Nanosecond))
	reclaimed := f.claim(t, run.ID, "worker-b")
	require.NotNil(reclaimed.ActiveAttempt)
	assert.Equal(attempt.ID, reclaimed.ActiveAttempt.ID)
	assert.Equal("opaque-job-42", reclaimed.ActiveAttempt.JobID)
	assert.Equal(providerStartedAt, reclaimed.ActiveAttempt.StartedAt)
	assert.Equal([]personfacts.TargetDescriptor{target}, reclaimed.ActiveAttempt.Targets)
	assert.Equal(attempt.ID, reclaimed.Token.AttemptID)
}

func TestSixtyfourReclaimUsesProviderStartTimeForMaximumJobAge(t *testing.T) {
	require := require.New(t)
	checks := assert.New(t)
	target := durableAttemptTarget(t)
	var calls int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/start" {
			_, err := w.Write([]byte(`{"task_id":"opaque-expired-job","status":"RUNNING"}`))
			assert.NoError(t, err)
			return
		}
		http.Error(w, "expired jobs must fail before poll egress", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	config := personenrichment.ProviderConfig{
		Name: "sixtyfour-expired-restart", Kind: personenrichment.ProviderSixtyfour, Enabled: true,
		Endpoint: server.URL + "/start", PollEndpoint: server.URL + "/poll", APIKeyEnv: "TEST_API_KEY",
		Tier: "medium", AllowedIdentifiers: []personenrichment.IdentifierClass{
			personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
		},
		TargetKeys: []string{target.Key}, RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		RefreshInterval: 24 * time.Hour, RequestTimeout: time.Second, PollInterval: time.Second,
		MaxJobAge: 15 * time.Minute, MaxRetries: 2, MaxRequestsPerRun: 10, MaxRequestsPerDay: 100,
	}

	f := newEnrichmentWorkFixture(t)
	reservationTime := time.Now().UTC().Truncate(time.Millisecond)
	f.setNow(reservationTime)
	run := f.startRun(t, "sixtyfour-expired-restart")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	durable, _, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "7"))
	require.NoError(err)
	provider, err := personenrichment.NewSixtyfourProvider(config, "test-credential", server.Client())
	require.NoError(err)
	started, err := provider.Start(t.Context(), personenrichment.Request{
		RequestHash: strings.Repeat("7", 64), Identity: personenrichment.Identity{
			Name: "Expired Person", CurrentCompany: "Example Labs",
		},
		Targets: []personfacts.TargetDescriptor{target},
	})
	require.NoError(err)
	providerStartedAt := reservationTime.Add(-config.MaxJobAge - time.Minute)
	started.StartedAt = providerStartedAt
	require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), durable.Token))
	require.NoError(f.store.RecordProviderStarted(t.Context(), durable.Token, started))
	require.NoError(f.store.SchedulePoll(t.Context(), durable.Token, personenrichment.Result{
		State: personenrichment.ResultPending, JobID: started.JobID, PollAfter: started.PollAfter,
		AdapterVersion: started.AdapterVersion, SchemaVersion: started.SchemaVersion,
		GeneratedSchema: started.GeneratedSchema, GeneratedSchemaHash: started.GeneratedSchemaHash,
	}))

	f.setNow(reservationTime.Add(2 * time.Second))
	reclaimed := f.claim(t, run.ID, "worker-b")
	require.NotNil(reclaimed.ActiveAttempt)
	active := reclaimed.ActiveAttempt
	checks.Equal(providerStartedAt, active.StartedAt)
	_, err = provider.Poll(t.Context(), personenrichment.Attempt{
		State: personenrichment.AttemptPending, JobID: active.JobID, StartedAt: active.StartedAt,
		AdapterVersion: active.AdapterVersion, SchemaVersion: active.SchemaVersion,
		GeneratedSchema: active.GeneratedSchema, GeneratedSchemaHash: active.GeneratedSchemaHash,
		ProgramFingerprint: active.ProgramFingerprint, Targets: active.Targets,
	})
	require.Error(err)
	checks.Equal(int64(1), calls)
}

func TestSixtyfourAttemptPollsAfterDurableReleaseAndReclaim(t *testing.T) {
	require := require.New(t)
	checks := assert.New(t)
	target := durableAttemptTarget(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/start":
			_, err := w.Write([]byte(`{"task_id":"opaque-job-42","status":"RUNNING"}`))
			assert.NoError(t, err)
		case r.Method == http.MethodGet && r.URL.Path == "/poll/opaque-job-42":
			_, err := w.Write([]byte(`{"task_id":"opaque-job-42","status":"completed",` +
				`"result":{"structured_data":{"attribute:summary":"Restarted public profile",` +
				`"name":"Restart Person","company":"Example Labs"},` +
				`"confidence_score":9,"findings":[]},"charge_amount":12}`))
			assert.NoError(t, err)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	config := personenrichment.ProviderConfig{
		Name: "sixtyfour-restart", Kind: personenrichment.ProviderSixtyfour, Enabled: true,
		Endpoint: server.URL + "/start", PollEndpoint: server.URL + "/poll", APIKeyEnv: "TEST_API_KEY",
		Tier: "medium", AllowedIdentifiers: []personenrichment.IdentifierClass{
			personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
		},
		TargetKeys: []string{target.Key}, RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		RefreshInterval: 24 * time.Hour, RequestTimeout: time.Second, PollInterval: time.Second,
		MaxJobAge: 15 * time.Minute, MaxRetries: 2, MaxRequestsPerRun: 10, MaxRequestsPerDay: 100,
	}

	f := newEnrichmentWorkFixture(t)
	f.setNow(time.Now().UTC().Add(-time.Second).Truncate(time.Millisecond))
	run := f.startRun(t, "sixtyfour-restart")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	durable, _, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "e"))
	require.NoError(err)
	firstProvider, err := personenrichment.NewSixtyfourProvider(config, "test-credential", server.Client())
	require.NoError(err)
	started, err := firstProvider.Start(t.Context(), personenrichment.Request{
		RequestHash: strings.Repeat("e", 64), Identity: personenrichment.Identity{
			Name: "Restart Person", CurrentCompany: "Example Labs",
		},
		Targets: []personfacts.TargetDescriptor{target},
	})
	require.NoError(err)
	require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), durable.Token))
	require.NoError(f.store.RecordProviderStarted(t.Context(), durable.Token, started))
	require.NoError(f.store.SchedulePoll(t.Context(), durable.Token, personenrichment.Result{
		State: personenrichment.ResultPending, JobID: started.JobID, PollAfter: started.PollAfter,
		AdapterVersion: started.AdapterVersion, SchemaVersion: started.SchemaVersion,
		GeneratedSchema: started.GeneratedSchema, GeneratedSchemaHash: started.GeneratedSchemaHash,
	}))

	f.setNow(f.now.Add(2 * time.Second))
	reclaimed := f.claim(t, run.ID, "worker-b")
	require.NotNil(reclaimed.ActiveAttempt)
	active := reclaimed.ActiveAttempt
	checks.Equal([]personfacts.TargetDescriptor{target}, active.Targets)

	restartedProvider, err := personenrichment.NewSixtyfourProvider(config, "test-credential", server.Client())
	require.NoError(err)
	result, err := restartedProvider.Poll(t.Context(), personenrichment.Attempt{
		State: personenrichment.AttemptPending, JobID: active.JobID, StartedAt: active.StartedAt,
		AdapterVersion: active.AdapterVersion, SchemaVersion: active.SchemaVersion,
		GeneratedSchema: active.GeneratedSchema, GeneratedSchemaHash: active.GeneratedSchemaHash,
		ProgramFingerprint: active.ProgramFingerprint, Targets: active.Targets,
	})
	require.NoError(err)
	require.Len(result.Claims, 1)
	checks.Equal(target, result.Claims[0].Target)
	checks.JSONEq(`"Restarted public profile"`, string(result.Claims[0].SubmittedValue))
}

func TestPersonEnrichmentClaimRejectsCorruptDurableAttemptTargets(t *testing.T) {
	require := require.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "corrupt-durable-targets")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "f"))
	require.NoError(err)
	target := durableAttemptTarget(t)
	require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
	require.NoError(f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
		State: personenrichment.AttemptPending, JobID: "opaque-job-42",
		StartedAt:      f.now,
		AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
		GeneratedSchema: true, GeneratedSchemaHash: strings.Repeat("c", 64),
		ProgramFingerprint: strings.Repeat("b", 64), Targets: []personfacts.TargetDescriptor{target},
	}))
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE person_enrichment_attempts SET targets_json = ? WHERE id = ?`), `[]`, attempt.ID)
	require.NoError(err)

	f.setNow(lease.LeaseUntil.Add(time.Nanosecond))
	_, err = f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "worker-b", ProviderName: f.profile.Name,
		Now: f.now, LeaseDuration: 5 * time.Minute,
	})
	assert.ErrorContains(t, err, "durable attempt targets")
}

func durableAttemptTarget(t *testing.T) personfacts.TargetDescriptor {
	t.Helper()
	target := personfacts.TargetDescriptor{
		Kind: personfacts.TargetAttribute, Key: "attribute:summary",
		UniversalID: "attribute:summary", Slug: "summary",
		Description: "Synthetic public profile summary", ValueType: personfacts.ValueText,
		Cardinality: personfacts.CardinalitySingle,
	}
	revision, err := personfacts.DescriptorRevision(target)
	require.NoError(t, err)
	target.Revision = revision
	return target
}

func TestPersonEnrichmentOneActiveAttemptInvariant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "one-active")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	first, created, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "a"))
	require.NoError(err)
	assert.True(created)

	again, created, err := f.store.BeginAttempt(t.Context(), first.Token, testAttemptStart(&f, run.ID, "a"))
	require.NoError(err)
	assert.False(created)
	assert.Equal(first.ID, again.ID)

	conflicting := testAttemptStart(&f, run.ID, "b")
	_, created, err = f.store.BeginAttempt(t.Context(), first.Token, conflicting)
	require.ErrorIs(err, store.ErrActiveAttemptConflict)
	assert.False(created)

	var count int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_enrichment_attempts
		WHERE person_id = ? AND profile_fingerprint = ?`),
		f.person.ID, f.profile.Fingerprint).Scan(&count))
	assert.Equal(1, count)
}

func TestPersonEnrichmentBeginAttemptReplacesActiveAttemptAfterFreshTrigger(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "fresh-active")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	start := testAttemptStart(&f, run.ID, "a")
	first, created, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
	requirements.NoError(err)
	checks.True(created)

	requirements.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Kind: personenrichment.TriggerManual, Generation: "manual:fresh-active", DueAt: f.now,
	}))
	replayed, created, err := f.store.BeginAttempt(t.Context(), first.Token, start)
	requirements.ErrorIs(err, store.ErrStaleLease)
	checks.Nil(replayed)
	checks.False(created)

	stored, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), first.ID)
	requirements.NoError(err)
	checks.Equal("terminal", stored.State)
	work := f.work(t)
	requirements.Len(work, 1)
	checks.Nil(work[0].RunID)
	checks.Nil(work[0].ActiveAttemptID)
	checks.False(work[0].HasFreshTrigger)
	checks.Equal("manual:fresh-active", work[0].TriggerGeneration)

	replacementLease := f.claim(t, run.ID, "worker-b")
	input, err := f.store.LoadRequestInput(t.Context(), *replacementLease)
	requirements.NoError(err)
	replacementStart := testAttemptStart(&f, run.ID, "b")
	replacementStart.PersonRevision = input.PersonRevision
	replacementStart.Trigger = replacementLease.Trigger
	replacement, created, err := f.store.BeginAttempt(t.Context(), replacementLease.Token,
		replacementStart)
	requirements.NoError(err)
	checks.True(created)
	checks.NotEqual(first.ID, replacement.ID)
}

func TestPersonEnrichmentFreshTriggerSurvivesActiveAttemptRetryRelease(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "fresh-after-retry")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token,
		testAttemptStart(&f, run.ID, "a"))
	require.NoError(err)
	require.NoError(f.store.ScheduleRetry(t.Context(), attempt.Token, personenrichment.RetryUpdate{
		Failure: personenrichment.SafeFailure{
			Class: personenrichment.FailureTransient, Message: "retry later",
		},
		NextActionAt: f.now.Add(time.Minute),
	}))

	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Kind: personenrichment.TriggerManual, Generation: "manual:fresh-after-retry", DueAt: f.now,
	}))
	work := f.work(t)
	require.Len(work, 1)
	assert.True(work[0].HasFreshTrigger)

	reclaimed := f.claim(t, run.ID, "worker-b")
	require.NotNil(reclaimed.ActiveAttempt)
	require.NoError(f.store.MarkTerminal(t.Context(), reclaimed.Token, personenrichment.SafeFailure{
		Class: personenrichment.FailureInvalidOutput, Message: "durable request binding changed",
	}))
	work = f.work(t)
	require.Len(work, 1)
	assert.Nil(work[0].RunID)
	assert.Nil(work[0].ActiveAttemptID)
	assert.False(work[0].HasFreshTrigger)
	assert.Equal("manual:fresh-after-retry", work[0].TriggerGeneration)
}

func TestPersonEnrichmentStaleAttemptRetainsReplacementWork(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "stale-revision")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token,
		testAttemptStart(&f, run.ID, "a"))
	requirements.NoError(err)

	_, err = f.store.AddPersonAddressContext(t.Context(), f.person.ID, store.PersonAddressInput{
		AddressKind: store.PersonAddressPostal,
		Locality:    new("Exampleville"),
		Envelope:    store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	current, err := f.store.GetPersonContext(t.Context(), f.person.ID)
	requirements.NoError(err)
	requirements.Greater(current.Revision, attempt.PersonRevision)

	requirements.NoError(f.store.MarkTerminal(t.Context(), attempt.Token, personenrichment.SafeFailure{
		Class: personenrichment.FailureInvalidOutput, Message: "durable request binding changed",
	}))
	work := f.work(t)
	requirements.Len(work, 1)
	checks.Nil(work[0].RunID)
	checks.Nil(work[0].ActiveAttemptID)
	checks.False(work[0].HasFreshTrigger)
	checks.Equal("revision:"+strconv.FormatInt(current.Revision, 10), work[0].TriggerGeneration)
}

func TestPersonEnrichmentActiveRetryReleaseIncrementsAttemptCount(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "active-retry-count")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token,
		testAttemptStart(&f, run.ID, "a"))
	requirements.NoError(err)
	checks.Equal(int64(1), attempt.AttemptCount)

	next := f.now.Add(time.Minute)
	requirements.NoError(f.store.ReleaseWork(t.Context(), attempt.Token, personenrichment.WorkRelease{
		Outcome: "retry",
		Failure: &personenrichment.SafeFailure{
			Class: personenrichment.FailureTransient, Message: "retry later",
		},
		NextActionAt: &next,
	}))
	stored, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
	requirements.NoError(err)
	checks.Equal("retry_wait", stored.State)
	checks.Equal(int64(2), stored.AttemptCount)
}

func TestPersonEnrichmentAttemptRejectsGeneratedSchemaHashWithoutGeneratedSchema(t *testing.T) {
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "generated-schema-shape")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "a"))
	require.NoError(t, err)

	require.NoError(t, f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
	err = f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
		State: personenrichment.AttemptPending, JobID: "opaque-job",
		StartedAt:      f.now,
		AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
		GeneratedSchema: false, GeneratedSchemaHash: "not-empty",
		ProgramFingerprint: strings.Repeat("b", 64),
	})
	require.Error(t, err)
}

func TestPersonEnrichmentAttemptRejectsNonAtomicSynchronousStart(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "generated-synchronous")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "9"))
	require.NoError(err)
	result := personenrichment.Result{
		State: personenrichment.ResultComplete, AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
		ProviderVersion: "provider-v1", GeneratedSchema: true,
		GeneratedSchemaHash: strings.Repeat("c", 64),
	}
	require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
	err = f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
		State: personenrichment.AttemptComplete, RequestID: "opaque-request", Result: &result,
		AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1", GeneratedSchema: true,
		GeneratedSchemaHash: strings.Repeat("c", 64), ProgramFingerprint: strings.Repeat("b", 64),
	})
	require.Error(err)

	stored, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
	require.NoError(err)
	assert.Equal("starting", stored.State)
	assert.Nil(stored.ProviderRequestID)
}

func TestPersonDisplayNameRejectsAuthorizedEnrichmentDispatch(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "display-name-dispatch")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token,
		testAttemptStart(&f, run.ID, "d"))
	requirements.NoError(err)
	requirements.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))

	displayName := "Changed During Dispatch"
	updated, err := f.store.UpdatePersonDisplayNameContext(
		t.Context(), f.person.ID, f.person.Revision, &displayName)
	requirements.ErrorIs(err, store.ErrPersonEnrichmentDispatchInProgress)
	checks.Nil(updated)
	current, err := f.store.GetPersonContext(t.Context(), f.person.ID)
	requirements.NoError(err)
	checks.Equal(f.person.Revision, current.Revision)
	checks.Equal(f.person.DisplayName, current.DisplayName)
}

func TestPersonEnrichmentAttemptRejectsPollingDifferentOpaqueJob(t *testing.T) {
	require := require.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "poll-job-binding")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "a"))
	require.NoError(err)
	require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
	require.NoError(f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
		State: personenrichment.AttemptPending, RequestID: "opaque-request", JobID: "opaque-job-42",
		StartedAt:      f.now,
		AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
		ProgramFingerprint: strings.Repeat("b", 64),
	}))

	err = f.store.SchedulePoll(t.Context(), attempt.Token, personenrichment.Result{
		State: personenrichment.ResultPending, RequestID: "opaque-request", JobID: "different-job",
		PollAfter: time.Minute, AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
	})
	require.Error(err)
}

func TestPersonEnrichmentAttemptPersistsOpaqueProviderIDsExactly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "opaque-id-bytes")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "a"))
	require.NoError(err)
	requestID := " request-id\t"
	jobID := "\njob-id "
	require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
	require.NoError(f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
		State: personenrichment.AttemptPending, RequestID: requestID, JobID: jobID,
		StartedAt:      f.now,
		AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
		ProgramFingerprint: strings.Repeat("b", 64),
	}))

	got, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
	require.NoError(err)
	require.NotNil(got.ProviderRequestID)
	require.NotNil(got.ProviderJobID)
	assert.Equal(requestID, *got.ProviderRequestID)
	assert.Equal(jobID, *got.ProviderJobID)

	require.NoError(f.store.SchedulePoll(t.Context(), attempt.Token, personenrichment.Result{
		State: personenrichment.ResultPending, RequestID: requestID, JobID: jobID,
		PollAfter: time.Minute, AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
	}))
}

func TestPersonEnrichmentAttemptDiagnosticsAreBoundedAndRedacted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "diagnostics")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "a"))
	require.NoError(err)

	got, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
	require.NoError(err)
	assert.Equal(attempt.ID, got.ID)
	listed, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, State: "starting",
		RunID: run.ID, Limit: 10,
	})
	require.NoError(err)
	require.Len(listed, 1)
	encoded, err := json.Marshal(listed[0])
	require.NoError(err)
	for _, forbidden := range []string{"credential", "suppression_key", "disclosed_identifier", "raw_request", "provider_response"} {
		assert.NotContains(string(encoded), forbidden)
	}

	_, err = f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		State: "credential", Limit: 10,
	})
	require.Error(err)
	_, err = f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{Limit: 201})
	require.Error(err)
}

func TestPersonEnrichmentWorkUncertainStartIsNotAutomaticallyReplayed(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "uncertain")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "a"))
	require.NoError(err)

	f.setNow(lease.LeaseUntil.Add(time.Nanosecond))
	reclaimed := f.claim(t, run.ID, "worker-b")
	require.NotNil(reclaimed.ActiveAttempt)
	assert.Equal("uncertain_start", reclaimed.ActiveAttempt.State)
	_, created, err := f.store.BeginAttempt(t.Context(), reclaimed.Token, testAttemptStart(&f, run.ID, "a"))
	require.NoError(err)
	assert.False(created)
	assert.Equal(attempt.ID, reclaimed.ActiveAttempt.ID)
	require.NoError(f.store.MarkUncertainStart(t.Context(), reclaimed.Token,
		personenrichment.SafeFailure{
			Class: personenrichment.FailureUncertainStart, Message: "start outcome is uncertain",
		}))
	work, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	require.NoError(err)
	assert.Empty(work)
	require.NoError(f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{
		CompletedAt: f.now,
	}))
	completed, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
	require.NoError(err)
	assert.Equal("failed", completed.State)
}

func TestPersonEnrichmentWorkReleasePreservesManualTrigger(t *testing.T) {
	require := require.New(t)
	f := newEnrichmentWorkFixture(t)
	run, _, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: "manual-release", RequestedAt: f.now,
	})
	require.NoError(err)
	require.NoError(f.store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerManual, Generation: "manual:key"},
		DueAt:   f.now,
	}))
	lease := f.claim(t, run.ID, "worker")
	require.NoError(f.store.ReleaseWork(t.Context(), lease.Token, personenrichment.WorkRelease{
		Outcome: "policy", PersonRevision: f.person.Revision,
		PayloadHash: strings.Repeat("a", 64), RequestHash: strings.Repeat("b", 64),
	}))
	attempts, err := f.store.ListPersonEnrichmentAttemptsContext(t.Context(), store.PersonEnrichmentAttemptFilter{
		RunID: run.ID, Limit: 10,
	})
	require.NoError(err)
	require.Len(attempts, 1)
	assert.Equal(t, "manual", attempts[0].TriggerKind)
	assert.Equal(t, "manual:key", attempts[0].TriggerGeneration)
}

func TestPersonEnrichmentTerminalReleasePreservesFreshTrigger(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "fresh-terminal-release")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker")
	requirements.NoError(f.store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerManual, Generation: "manual:fresh"},
		DueAt:   f.now,
	}))

	requirements.NoError(f.store.ReleaseWork(t.Context(), lease.Token, personenrichment.WorkRelease{
		Outcome: "policy", PersonRevision: f.person.Revision,
		PayloadHash: strings.Repeat("a", 64), RequestHash: strings.Repeat("b", 64),
	}))
	work := f.work(t)
	requirements.Len(work, 1)
	checks.Nil(work[0].RunID)
	checks.Nil(work[0].LeaseOwner)
	checks.False(work[0].HasFreshTrigger)

	reclaimed := f.claim(t, run.ID, "next-worker")
	checks.Equal(personenrichment.TriggerManual, reclaimed.Trigger.Kind)
	checks.Equal("manual:fresh", reclaimed.Trigger.Generation)
}

func TestPersonEnrichmentWorkLoadsCurrentMinimumRequestInput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentWorkFixture(t)
	catalog, err := f.store.BuildPersonFactCatalogContext(t.Context(), false)
	require.NoError(err)
	require.NotEmpty(catalog.Targets)
	profile, err := (personenrichment.ProviderConfig{
		Name: "exa-load", Kind: personenrichment.ProviderExa, Enabled: true,
		Endpoint: "https://load.example.test/search", APIKeyEnv: "PROVIDER_API_KEY",
		Mode: "deep", NumResults: 1,
		AllowedIdentifiers: []personenrichment.IdentifierClass{
			personenrichment.IdentifierEmail, personenrichment.IdentifierPublicProfileURL,
		},
		TargetKeys: []string{catalog.Targets[0].Key}, RetentionPosture: "zero_retention",
		TrainingPosture: "no_training", RefreshInterval: time.Hour,
		RequestTimeout: time.Minute, PollInterval: time.Minute, MaxJobAge: time.Hour,
		MaxRetries: 1, MaxRequestsPerRun: 10, MaxRequestsPerDay: 10,
	}).Profile(catalog)
	require.NoError(err)
	_, err = f.store.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(err)
	_, _, err = f.store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(err)
	_, err = f.store.AddPersonContactPointContext(t.Context(), f.person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressURL, OriginalValue: "https://profiles.example.test/work-person",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	f.person, err = f.store.GetPersonContext(t.Context(), f.person.ID)
	require.NoError(err)
	run := f.startRun(t, "load-request")
	require.NoError(f.store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
		PersonID: f.person.ID, ProfileFingerprint: profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "revision:1"},
		DueAt:   f.now,
	}))
	lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "worker", ProviderName: profile.Name,
		Now: f.now, LeaseDuration: time.Minute,
	})
	require.NoError(err)
	require.NotNil(lease)

	input, err := f.store.LoadRequestInput(t.Context(), *lease)
	require.NoError(err)
	assert.Equal(f.person.ID, input.PersonID)
	assert.Equal(f.person.Revision, input.PersonRevision)
	assert.Equal(lease.Trigger, input.Trigger)
	request, hashes, err := personenrichment.BuildRequest(input, profile)
	require.NoError(err)
	assert.Equal("work-person@example.com", request.Identity.Email)
	assert.Equal([]string{"https://profiles.example.test/work-person"}, request.Identity.PublicProfileURLs)
	assert.NotEmpty(hashes.PayloadHash)
	assert.NotEmpty(hashes.RequestHash)
}

func TestPersonEnrichmentRequestUsesPersonDisplayName(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	requirements.NotEmpty(f.person.ParticipantIDs)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE participants SET display_name = NULL WHERE id = ?`), f.person.ParticipantIDs[0])
	requirements.NoError(err)
	displayName := "Curated Work Person"
	f.person, err = f.store.UpdatePersonDisplayNameContext(
		t.Context(), f.person.ID, f.person.Revision, &displayName)
	requirements.NoError(err)
	organization, err := f.store.CreateOrganizationContext(t.Context(), store.OrganizationInput{
		Name: "Example Labs", Kind: store.OrganizationKindCompany,
	})
	requirements.NoError(err)
	_, err = f.store.AddEmploymentContext(t.Context(), store.EmploymentInput{
		PersonID: f.person.ID, OrganizationID: organization.ID,
		IsCurrent: new(true), IsPrimary: new(true), Source: store.ProvenanceUser,
	})
	requirements.NoError(err)

	input, err := f.store.LoadRequestInput(t.Context(), personenrichment.WorkLease{
		PersonID: f.person.ID,
		Trigger:  personenrichment.Trigger{Kind: personenrichment.TriggerManual, Generation: "manual:display-name"},
	})
	requirements.NoError(err)
	var target personfacts.TargetDescriptor
	for _, candidate := range input.Catalog.Targets {
		if !candidate.Sensitive {
			target = candidate
			break
		}
	}
	requirements.NotEmpty(target.Key)
	profile := f.profile
	profile.Kind = personenrichment.ProviderSixtyfour
	profile.AllowedIdentifiers = []personenrichment.IdentifierClass{
		personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
	}
	profile.Targets = []personfacts.TargetDescriptor{target}
	request, _, err := personenrichment.BuildRequest(input, profile)
	requirements.NoError(err)
	checks.Equal("curated work person", request.Identity.Name)
	checks.Equal("example labs", request.Identity.CurrentCompany)
}

func TestPersonEnrichmentTerminalRefreshBecomesUnboundWork(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "refresh")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "a"))
	require.NoError(err)
	refreshAt := f.now.Add(24 * time.Hour)
	require.NoError(store.CompletePersonEnrichmentAttemptForTest(t.Context(), f.store, attempt.Token, store.PersonEnrichmentAttemptCompletion{
		State: "succeeded", ActualCost: personenrichment.Cost{},
		RefreshAt: &refreshAt, RefreshGeneration: "refresh:revision:2",
	}))

	work, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	require.NoError(err)
	require.Len(work, 1)
	assert.Nil(work[0].RunID)
	assert.Nil(work[0].ActiveAttemptID)
	assert.Nil(work[0].LeaseOwner)
	assert.Equal("refresh:revision:2", work[0].TriggerGeneration)
	assert.Equal(refreshAt, work[0].DueAt)
	require.NoError(f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{
		State: "succeeded", CompletedAt: f.now,
	}))
}

func TestPersonEnrichmentSuccessfulCompletionComposesInsideCallerTransaction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentWorkFixture(t)
	run := f.startRun(t, "completion-caller-transaction")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "worker-a")
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, testAttemptStart(&f, run.ID, "a"))
	require.NoError(err)

	require.NoError(store.RollbackPersonEnrichmentAttemptCompletionForTest(
		t.Context(), f.store, attempt.Token, store.PersonEnrichmentAttemptCompletion{
			State: "succeeded", ActualCostMissing: true,
		},
	))
	got, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
	require.NoError(err)
	assert.Equal("starting", got.State)
	work, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 1,
	})
	require.NoError(err)
	require.Len(work, 1)
	require.NotNil(work[0].ActiveAttemptID)
	assert.Equal(attempt.ID, *work[0].ActiveAttemptID)
}
