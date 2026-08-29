package store_test

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func enrichmentTriggerProfile(t *testing.T, name, endpoint string) personenrichment.ProviderProfile {
	t.Helper()
	profile, err := (personenrichment.ProviderConfig{
		Name: name, Kind: personenrichment.ProviderExa, Enabled: true,
		Endpoint: endpoint, APIKeyEnv: "TEST_ENRICHMENT_KEY", Mode: "deep", NumResults: 1,
		AllowedIdentifiers: []personenrichment.IdentifierClass{
			personenrichment.IdentifierName, personenrichment.IdentifierEmail,
			personenrichment.IdentifierPhone, personenrichment.IdentifierCurrentCompany,
		},
		TargetKeys: []string{"attribute:bio"}, RetentionPosture: "zero_retention",
		TrainingPosture: "no_training", RefreshInterval: 24 * time.Hour,
		RequestTimeout: time.Minute, PollInterval: time.Second, MaxJobAge: time.Hour,
		MaxRetries: 2, MaxRequestsPerRun: 1000, MaxRequestsPerDay: 10000,
	}).Profile(personfacts.Catalog{Version: "trigger-v1", Targets: []personfacts.TargetDescriptor{{
		Kind: personfacts.TargetAttribute, Key: "attribute:bio", Revision: "revision-1",
		UniversalID: "attribute:bio", Slug: "bio", Description: "Synthetic biography",
		ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
		Choices: []personfacts.ChoiceDescriptor{}, Fields: []personfacts.FieldDescriptor{},
	}}})
	require.NoError(t, err)
	return profile
}

type enrichmentTriggerFixture struct {
	store    *store.Store
	person   *store.Person
	profiles []personenrichment.ProviderProfile
	now      time.Time
}

func newEnrichmentTriggerFixture(t *testing.T, profileCount int) enrichmentTriggerFixture {
	t.Helper()
	f := storetest.New(t)
	participant := f.EnsureParticipant("trigger-person@example.test", "Trigger Person", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participant)
	require.NoError(t, err)
	now := time.Date(2026, 8, 23, 14, 0, 0, 0, time.UTC)
	store.SetPersonEnrichmentClockForTest(f.Store, func() time.Time { return now })
	profiles := make([]personenrichment.ProviderProfile, 0, profileCount)
	for i := range profileCount {
		profile := enrichmentTriggerProfile(t,
			"provider-"+string(rune('a'+i)),
			"https://provider-"+string(rune('a'+i))+".example.test/search")
		_, err = f.Store.EnsurePersonEnrichmentProfile(t.Context(), profile)
		require.NoError(t, err)
		profiles = append(profiles, profile)
	}
	return enrichmentTriggerFixture{store: f.Store, person: person, profiles: profiles, now: now}
}

func (f enrichmentTriggerFixture) grant(t *testing.T, index int) *store.PersonEnrichmentConsent {
	t.Helper()
	consent, created, err := f.store.GrantPersonEnrichmentConsent(
		t.Context(), f.profiles[index].Fingerprint, "test")
	require.NoError(t, err)
	require.True(t, created)
	return consent
}

func (f enrichmentTriggerFixture) work(t *testing.T, profile int) []store.PersonEnrichmentWork {
	t.Helper()
	rows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profiles[profile].Fingerprint, Limit: 200,
	})
	require.NoError(t, err)
	return rows
}

func TestPersonEnrichmentTriggerCoalescesKindsAndAdvancesGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentTriggerFixture(t, 1)
	f.grant(t, 0)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)

	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profiles[0].Fingerprint,
		Kind: personenrichment.TriggerTracked, Generation: "revision:7", DueAt: f.now,
	}))
	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profiles[0].Fingerprint,
		Kind: personenrichment.TriggerIdentity, Generation: "revision:8", DueAt: f.now.Add(time.Minute),
	}))

	rows := f.work(t, 0)
	require.Len(rows, 1)
	assert.Equal(int64(3), rows[0].TriggerMask)
	assert.Equal("revision:8", rows[0].TriggerGeneration)
	assert.Equal(f.now, rows[0].DueAt)
}

func TestPersonEnrichmentIdempotentTrackingDoesNotRepublishWork(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentTriggerFixture(t, 1)
	f.grant(t, 0)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))
	insertProviderIdentity(t, f.store, f.person.ID,
		f.profiles[0].ProviderNamespace, "opaque-before-repeat")

	_, err = f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	assert.Empty(f.work(t, 0))
	assert.Equal(1, providerIdentityCount(t, f.store, f.person.ID))
}

func TestPersonEnrichmentManualRunRunningIdempotencyRejectsDifferentProfile(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentTriggerFixture(t, 2)
	f.grant(t, 0)
	f.grant(t, 1)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	requirements.NoError(err)
	first, created, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(),
		f.person.ID, f.profiles[0].Fingerprint, "target-bound-key", f.now)
	requirements.NoError(err)
	requirements.True(created)
	requirements.NotNil(first)
	beforeConflict := f.work(t, 1)

	conflict, created, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(),
		f.person.ID, f.profiles[1].Fingerprint, "target-bound-key", f.now.Add(time.Minute))
	requirements.ErrorIs(err, store.ErrManualRunIdempotencyConflict)
	checks.Nil(conflict)
	checks.False(created)
	checks.Equal(beforeConflict, f.work(t, 1))
	checks.Len(f.work(t, 0), 1)
}

func TestPersonEnrichmentManualRunCompletedIdempotencyRejectsDifferentPerson(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newEnrichmentTriggerFixture(t, 1)
	f.grant(t, 0)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	requirements.NoError(err)
	otherParticipant, err := f.store.EnsureParticipant(
		"other-manual-target@example.test", "Other Manual Target", "example.test")
	requirements.NoError(err)
	other, _, err := f.store.CreatePersonFromParticipantContext(t.Context(), otherParticipant)
	requirements.NoError(err)
	_, err = f.store.SetPersonTrackingContext(t.Context(), other.ID, true)
	requirements.NoError(err)
	beforeConflict, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: other.ID, ProfileFingerprint: f.profiles[0].Fingerprint, Limit: 10,
	})
	requirements.NoError(err)
	run, created, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(),
		f.person.ID, f.profiles[0].Fingerprint, "completed-target-key", f.now)
	requirements.NoError(err)
	requirements.True(created)
	lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "manual-target-worker", ProviderName: f.profiles[0].Name,
		Now: f.now, LeaseDuration: time.Minute,
	})
	requirements.NoError(err)
	requirements.NotNil(lease)
	requirements.NoError(f.store.ReleaseWork(t.Context(), lease.Token, personenrichment.WorkRelease{
		Outcome: "suppressed", PersonRevision: f.person.Revision,
		PayloadHash: strings.Repeat("a", 64), RequestHash: strings.Repeat("b", 64),
	}))
	requirements.NoError(f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{
		State: "succeeded", CompletedAt: f.now.Add(time.Minute),
	}))

	conflict, created, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(),
		other.ID, f.profiles[0].Fingerprint, "completed-target-key", f.now.Add(2*time.Minute))
	requirements.ErrorIs(err, store.ErrManualRunIdempotencyConflict)
	checks.Nil(conflict)
	checks.False(created)
	work, listErr := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: other.ID, ProfileFingerprint: f.profiles[0].Fingerprint, Limit: 10,
	})
	requirements.NoError(listErr)
	checks.Equal(beforeConflict, work)
}

func TestPersonEnrichmentManualRunReplaysAfterPersonDeletion(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := newEnrichmentTriggerFixture(t, 1)
	f.grant(t, 0)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	requirements.NoError(err)
	run, created, err := f.store.StartManualPersonEnrichmentRunContext(
		t.Context(), f.person.ID, f.profiles[0].Fingerprint, "deleted-target-key", f.now)
	requirements.NoError(err)
	requirements.True(created)

	requirements.NoError(f.store.DeletePersonContext(
		t.Context(), f.person.ID, f.person.Revision))
	replayed, created, err := f.store.StartManualPersonEnrichmentRunContext(
		t.Context(), f.person.ID, f.profiles[0].Fingerprint,
		"deleted-target-key", f.now.Add(time.Minute))
	requirements.NoError(err)
	checks.False(created)
	requirements.NotNil(replayed)
	checks.Equal(run.ID, replayed.ID)
}

func TestPersonEnrichmentManualRunSerializesWithConsentRevocation(t *testing.T) {
	testPersonEnrichmentManualAuthorizationRaceBackends(t, "revoke")
}

func TestPersonEnrichmentManualRunSerializesWithUntracking(t *testing.T) {
	testPersonEnrichmentManualAuthorizationRaceBackends(t, "untrack")
}

func TestPersonEnrichmentTrackingSerializesWithMissingPersonRevocation(t *testing.T) {
	testPersonEnrichmentMissingPersonRevocationBackends(t, false)
}

func TestPersonEnrichmentManualRunSerializesWithMissingPersonRevocation(t *testing.T) {
	testPersonEnrichmentMissingPersonRevocationBackends(t, true)
}

func testPersonEnrichmentMissingPersonRevocationBackends(t *testing.T, manual bool) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		testPersonEnrichmentMissingPersonRevocation(t, testutil.NewSQLiteTestStore(t), manual)
	})
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("MSGVAULT_TEST_DB") == "" {
			t.Skip("PostgreSQL missing-person revocation race requires MSGVAULT_TEST_DB")
		}
		st := testutil.NewTestStore(t)
		require.True(t, st.IsPostgreSQL(), "MSGVAULT_TEST_DB must select PostgreSQL")
		testPersonEnrichmentMissingPersonRevocation(t, st, manual)
	})
}

func testPersonEnrichmentMissingPersonRevocation(t *testing.T, st *store.Store, manual bool) {
	t.Helper()
	suffix := "tracking"
	if manual {
		suffix = "manual"
	}
	profile := enrichmentTriggerProfile(t, "missing-person-revoke-"+suffix,
		"https://missing-person-revoke-"+suffix+".example.test/search")
	_, err := st.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(t, err)
	_, _, err = st.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(t, err)
	participantID, err := st.EnsureParticipant(
		"missing-person-"+suffix+"@example.test", "Missing Person", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(t, err)

	snapshotReached := make(chan struct{})
	releaseRevoke := make(chan struct{})
	trackingBeforeAuthority := make(chan struct{})
	trackingPersonLocked := make(chan struct{})
	manualPersonLocked := make(chan struct{})
	var snapshotOnce, trackingBeforeOnce, trackingLockedOnce, manualLockedOnce sync.Once
	store.SetPersonEnrichmentTxBarrierForTest(st, func(phase string) {
		switch phase {
		case "revoke_affected_people_snapshotted":
			snapshotOnce.Do(func() {
				close(snapshotReached)
				<-releaseRevoke
			})
		case "tracking_before_authority_lock":
			trackingBeforeOnce.Do(func() { close(trackingBeforeAuthority) })
		case "tracking_person_locked":
			trackingLockedOnce.Do(func() { close(trackingPersonLocked) })
		case "manual_person_locked":
			manualLockedOnce.Do(func() { close(manualPersonLocked) })
		}
	})
	revokeErr := make(chan error, 1)
	go func() {
		_, revokeCallErr := st.RevokePersonEnrichmentConsent(
			t.Context(), profile.Fingerprint, "privacy-test")
		revokeErr <- revokeCallErr
	}()
	requireChannelSignal(t, snapshotReached,
		"revocation did not pause after its missing-person snapshot")

	type publicationResult struct {
		run         *personenrichment.DurableRun
		trackingErr error
		manualErr   error
	}
	publication := make(chan publicationResult, 1)
	go func() {
		_, trackingErr := st.SetPersonTrackingContext(t.Context(), person.ID, true)
		result := publicationResult{trackingErr: trackingErr}
		if trackingErr == nil && manual {
			result.run, _, result.manualErr = st.StartManualPersonEnrichmentRunContext(
				t.Context(), person.ID, profile.Fingerprint,
				"missing-person-revoke-"+suffix, time.Now().UTC())
		}
		publication <- result
	}()
	requireChannelSignal(t, trackingBeforeAuthority,
		"tracking did not reach its authority/person gate")

	earlyPersonMutation := false
	select {
	case <-trackingPersonLocked:
		earlyPersonMutation = true
	case <-time.After(250 * time.Millisecond):
	}
	if manual && earlyPersonMutation {
		select {
		case <-manualPersonLocked:
		case <-time.After(2 * time.Second):
		}
	}
	close(releaseRevoke)
	require.NoError(t, <-revokeErr)
	result := <-publication
	require.NoError(t, result.trackingErr)
	assert.False(t, earlyPersonMutation,
		"tracking reached its person mutation after revocation's snapshot but before authority removal")
	if manual {
		require.Error(t, result.manualErr)
		assert.Nil(t, result.run)
	}

	status, err := st.PersonEnrichmentConsentStatus(t.Context(), profile.Fingerprint)
	require.NoError(t, err)
	assert.False(t, status.Active)
	work, err := st.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: person.ID, ProfileFingerprint: profile.Fingerprint, Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, work)
	var runs, attempts int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*)
		FROM person_enrichment_runs WHERE kind = 'manual' AND requested_by = ?`),
		"missing-person-revoke-"+suffix).Scan(&runs))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*)
		FROM person_enrichment_attempts WHERE person_id = ? AND profile_fingerprint = ?`),
		person.ID, profile.Fingerprint).Scan(&attempts))
	assert.Zero(t, runs)
	assert.Zero(t, attempts)

	_, _, err = st.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test-regrant")
	require.NoError(t, err)
	work, err = st.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: person.ID, ProfileFingerprint: profile.Fingerprint, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, work, 1)
	assert.Nil(t, work[0].RunID)
	assert.True(t, strings.HasPrefix(work[0].TriggerGeneration, "consent:"))
}

func testPersonEnrichmentManualAuthorizationRaceBackends(t *testing.T, removal string) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		testPersonEnrichmentManualAuthorizationRace(t, testutil.NewSQLiteTestStore(t), removal)
	})
	t.Run("postgres", func(t *testing.T) {
		if os.Getenv("MSGVAULT_TEST_DB") == "" {
			t.Skip("PostgreSQL manual authorization race requires MSGVAULT_TEST_DB")
		}
		st := testutil.NewTestStore(t)
		require.True(t, st.IsPostgreSQL(), "MSGVAULT_TEST_DB must select PostgreSQL")
		testPersonEnrichmentManualAuthorizationRace(t, st, removal)
	})
}

func testPersonEnrichmentManualAuthorizationRace(t *testing.T, st *store.Store, removal string) {
	t.Helper()
	profile := enrichmentTriggerProfile(t, "authorization-race-"+removal,
		"https://authorization-race-"+removal+".example.test/search")
	_, err := st.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(t, err)
	_, _, err = st.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(t, err)
	participantID, err := st.EnsureParticipant(
		"authorization-race@example.test", "Authorization Race", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(t, err)
	_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(t, err)

	authorityRemoved := make(chan struct{})
	releaseRemoval := make(chan struct{})
	manualBeforeLock := make(chan struct{})
	store.SetPersonEnrichmentTxBarrierForTest(st, func(phase string) {
		if phase == removal+"_authority_removed" {
			close(authorityRemoved)
			<-releaseRemoval
		}
		if phase == "manual_before_person_lock" {
			close(manualBeforeLock)
		}
	})
	removalErr := make(chan error, 1)
	go func() {
		if removal == "revoke" {
			_, revokeErr := st.RevokePersonEnrichmentConsent(
				t.Context(), profile.Fingerprint, "privacy-test")
			removalErr <- revokeErr
			return
		}
		_, untrackErr := st.SetPersonTrackingContext(t.Context(), person.ID, false)
		removalErr <- untrackErr
	}()
	requireChannelSignal(t, authorityRemoved, removal+" did not remove authority under the person lock")
	manualErr := make(chan error, 1)
	go func() {
		_, _, runErr := st.StartManualPersonEnrichmentRunContext(t.Context(),
			person.ID, profile.Fingerprint, "authorization-race-key", time.Now().UTC())
		manualErr <- runErr
	}()
	requireChannelSignal(t, manualBeforeLock, "manual run did not reach person lock")
	select {
	case err := <-manualErr:
		require.Fail(t, "manual run bypassed authorization-removal person lock", "error=%v", err)
	default:
	}
	close(releaseRemoval)
	require.NoError(t, <-removalErr)
	require.Error(t, <-manualErr)
	var runs int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*)
		FROM person_enrichment_runs WHERE kind = 'manual' AND requested_by = ?`),
		"authorization-race-key").Scan(&runs))
	assert.Zero(t, runs)
	work, err := st.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: person.ID, ProfileFingerprint: profile.Fingerprint, Limit: 10,
	})
	require.NoError(t, err)
	for _, item := range work {
		assert.Nil(t, item.RunID)
	}
}

func requireChannelSignal(t *testing.T, channel <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(3 * time.Second):
		require.FailNow(t, message)
	}
}

func TestPersonEnrichmentTriggerCoalescingKeepsSelectedKindAndGenerationPaired(t *testing.T) {
	kinds := []personenrichment.TriggerKind{
		personenrichment.TriggerTracked,
		personenrichment.TriggerRefresh,
		personenrichment.TriggerExpiry,
		personenrichment.TriggerIdentity,
		personenrichment.TriggerManual,
	}
	priority := map[personenrichment.TriggerKind]int{
		personenrichment.TriggerTracked: 1, personenrichment.TriggerRefresh: 2,
		personenrichment.TriggerExpiry: 3, personenrichment.TriggerIdentity: 4,
		personenrichment.TriggerManual: 5,
	}
	mask := map[personenrichment.TriggerKind]int64{
		personenrichment.TriggerTracked: 1, personenrichment.TriggerIdentity: 2,
		personenrichment.TriggerExpiry: 4, personenrichment.TriggerRefresh: 8,
		personenrichment.TriggerManual: 16,
	}
	for _, first := range kinds {
		for _, second := range kinds {
			t.Run(string(first)+"_then_"+string(second), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := newEnrichmentTriggerFixture(t, 1)
				f.grant(t, 0)
				_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
				require.NoError(err)
				require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))
				firstGeneration := string(first) + ":first"
				secondGeneration := string(second) + ":second"
				for _, input := range []store.EnrichmentTriggerInput{
					{PersonID: f.person.ID, ProfileFingerprint: f.profiles[0].Fingerprint,
						Kind: first, Generation: firstGeneration, DueAt: f.now.Add(time.Minute)},
					{PersonID: f.person.ID, ProfileFingerprint: f.profiles[0].Fingerprint,
						Kind: second, Generation: secondGeneration, DueAt: f.now},
				} {
					require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), input))
				}
				run, _, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
					Kind: "manual", RequestedBy: string(first) + "-then-" + string(second), RequestedAt: f.now,
				})
				require.NoError(err)
				lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
					RunID: run.ID, Owner: "pair-worker", ProviderName: f.profiles[0].Name,
					Now: f.now, LeaseDuration: time.Minute,
				})
				require.NoError(err)
				require.NotNil(lease)
				wantKind, wantGeneration := first, firstGeneration
				if priority[second] >= priority[first] {
					wantKind, wantGeneration = second, secondGeneration
				}
				assert.Equal(wantKind, lease.Trigger.Kind)
				assert.Equal(wantGeneration, lease.Trigger.Generation)
				rows := f.work(t, 0)
				require.Len(rows, 1)
				assert.Equal(mask[first]|mask[second], rows[0].TriggerMask)
			})
		}
	}
}

func TestPersonEnrichmentTriggerPublishesTrackedIdentityAndConsentWorkPerProfile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentTriggerFixture(t, 2)
	f.grant(t, 0)
	f.grant(t, 1)
	assert.Empty(f.work(t, 0), "untracked people must not be queued on consent")

	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	for index := range f.profiles {
		rows := f.work(t, index)
		require.Len(rows, 1)
		assert.Equal(int64(1), rows[0].TriggerMask)
	}

	name := "Changed Person"
	createdName, err := f.store.AddPersonNameContext(t.Context(), f.person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: &name, OriginalValue: name,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	for index := range f.profiles {
		rows := f.work(t, index)
		require.Len(rows, 1)
		assert.Equal(int64(2), rows[0].TriggerMask)
		assert.True(strings.HasPrefix(rows[0].TriggerGeneration, "revision:"))
	}
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))
	require.NoError(f.store.SupersedePersonNameContext(
		t.Context(), f.person.ID, createdName.Envelope.ID, nil))
	for index := range f.profiles {
		require.Len(f.work(t, index), 1)
	}
}

func TestPersonEnrichmentTriggerMutationBoundariesRemainTransactionLocal(t *testing.T) {
	require := require.New(t)
	f := newEnrichmentTriggerFixture(t, 1)
	f.grant(t, 0)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))

	email := "changed@example.test"
	point, err := f.store.AddPersonContactPointContext(t.Context(), f.person.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: email,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	require.Len(f.work(t, 0), 1)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))
	require.NoError(f.store.SupersedePersonContactPointContext(
		t.Context(), f.person.ID, point.Envelope.ID, nil))
	require.Len(f.work(t, 0), 1)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))

	person, err := f.store.GetPersonContext(t.Context(), f.person.ID)
	require.NoError(err)
	display := "Changed Display"
	_, err = f.store.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, &display)
	require.NoError(err)
	require.Len(f.work(t, 0), 1)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))

	org, err := f.store.CreateOrganizationContext(t.Context(), store.OrganizationInput{
		Name: "Synthetic Company", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	title := "Engineer"
	job, err := f.store.AddEmploymentContext(t.Context(), store.EmploymentInput{
		PersonID: person.ID, OrganizationID: org.ID, Title: &title, Source: store.ProvenanceUser,
	})
	require.NoError(err)
	require.Len(f.work(t, 0), 1)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))

	newTitle := "Senior Engineer"
	job, err = f.store.UpdateEmploymentContext(t.Context(), job.ID, job.Revision, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: org.ID, Title: &newTitle, Source: store.ProvenanceUser,
	})
	require.NoError(err)
	require.Len(f.work(t, 0), 1)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))

	endDate, err := store.ParsePartialDate("2026-08")
	require.NoError(err)
	job, err = f.store.EndEmploymentContext(t.Context(), job.ID, job.Revision, endDate)
	require.NoError(err)
	require.Len(f.work(t, 0), 1)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))

	require.NoError(f.store.DeleteEmploymentContext(t.Context(), job.ID, job.Revision))
	require.Len(f.work(t, 0), 1)
}

func TestPersonEnrichmentCatchUpRepairsMissingTrackedWorkAndExcludesUntracked(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentTriggerFixture(t, 1)
	f.grant(t, 0)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))

	count, err := f.store.EnqueueDuePersonEnrichmentContext(
		t.Context(), f.now, 200, []string{f.profiles[0].Fingerprint})
	require.NoError(err)
	assert.Equal(1, count)
	rows := f.work(t, 0)
	require.Len(rows, 1)
	assert.Equal(int64(1), rows[0].TriggerMask)

	count, err = f.store.EnqueueDuePersonEnrichmentContext(
		t.Context(), f.now, 200, []string{f.profiles[0].Fingerprint})
	require.NoError(err)
	assert.Zero(count)
}

func TestPersonEnrichmentCatchUpSerializesWithConsentRevocation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentTriggerFixture(t, 1)
	f.grant(t, 0)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))

	snapshotted := make(chan struct{})
	releaseCatchUp := make(chan struct{})
	var snapshotOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCatchUp) }) }
	t.Cleanup(release)
	store.SetPersonEnrichmentTxBarrierForTest(f.store, func(phase string) {
		if phase == "catch_up_missing_snapshotted" {
			snapshotOnce.Do(func() {
				close(snapshotted)
				<-releaseCatchUp
			})
		}
	})
	catchUpDone := make(chan error, 1)
	go func() {
		_, catchUpErr := f.store.EnqueueDuePersonEnrichmentContext(
			t.Context(), f.now, 200, []string{f.profiles[0].Fingerprint})
		catchUpDone <- catchUpErr
	}()
	requireChannelSignal(t, snapshotted, "catch-up did not pause after selecting missing work")

	revokeDone := make(chan error, 1)
	go func() {
		_, revokeErr := f.store.RevokePersonEnrichmentConsent(
			t.Context(), f.profiles[0].Fingerprint, "privacy-test")
		revokeDone <- revokeErr
	}()
	earlyRemoval := false
	var revokeErr error
	select {
	case revokeErr = <-revokeDone:
		earlyRemoval = true
	case <-time.After(250 * time.Millisecond):
	}
	release()
	require.NoError(<-catchUpDone)
	if !earlyRemoval {
		revokeErr = <-revokeDone
	}
	require.NoError(revokeErr)
	assert.False(earlyRemoval, "consent revocation committed during catch-up publication")
	assert.Empty(f.work(t, 0))
}

func TestPersonEnrichmentCatchUpDoesNotRecreateUnavailableProfileWork(t *testing.T) {
	for _, expiredClaim := range []bool{false, true} {
		t.Run("expired_claim="+strconv.FormatBool(expiredClaim), func(t *testing.T) {
			requirements := require.New(t)
			checks := assert.New(t)
			f := newEnrichmentTriggerFixture(t, 2)
			f.grant(t, 0)
			f.grant(t, 1)
			_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
			requirements.NoError(err)
			if expiredClaim {
				insertProviderClaim(t, f.store, f.person.ID, f.profiles[1].Fingerprint,
					"unavailable-expired", f.now.Add(-time.Minute))
			}
			requirements.NoError(f.store.CancelPersonEnrichmentWorkOutsideProfilesContext(
				t.Context(), []string{f.profiles[0].Fingerprint}))
			checks.Empty(f.work(t, 1))

			count, err := f.store.EnqueueDuePersonEnrichmentContext(
				t.Context(), f.now, 200, []string{f.profiles[0].Fingerprint})
			requirements.NoError(err)
			checks.Zero(count)
			checks.Empty(f.work(t, 1))
		})
	}
}

func TestPersonEnrichmentCatchUpAdvancesExistingRefreshWhenProviderClaimExpires(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentTriggerFixture(t, 1)
	f.grant(t, 0)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))

	future := f.now.Add(12 * time.Hour)
	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profiles[0].Fingerprint,
		Kind: personenrichment.TriggerRefresh, Generation: "attempt:future-refresh", DueAt: future,
	}))
	claimID := insertProviderClaim(t, f.store, f.person.ID, f.profiles[0].Fingerprint,
		"expired", f.now.Add(-time.Minute))

	count, err := f.store.EnqueueDuePersonEnrichmentContext(
		t.Context(), f.now, 200, []string{f.profiles[0].Fingerprint})
	require.NoError(err)
	assert.Equal(1, count)
	rows := f.work(t, 0)
	require.Len(rows, 1)
	assert.Equal(f.now, rows[0].DueAt)
	assert.Equal(int64(12), rows[0].TriggerMask)
	assert.Equal("claim:"+formatEnrichmentTriggerID(claimID), rows[0].TriggerGeneration)
}

func TestPersonEnrichmentCatchUpIgnoresHistoricalExpiredProviderClaim(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentTriggerFixture(t, 1)
	f.grant(t, 0)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))

	future := f.now.Add(12 * time.Hour)
	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profiles[0].Fingerprint,
		Kind: personenrichment.TriggerRefresh, Generation: "attempt:fresh-refresh", DueAt: future,
	}))
	insertProviderClaim(t, f.store, f.person.ID, f.profiles[0].Fingerprint,
		"historical-expired", f.now.Add(-time.Minute))
	insertProviderClaim(t, f.store, f.person.ID, f.profiles[0].Fingerprint,
		"latest-fresh", f.now.Add(6*time.Hour))

	count, err := f.store.EnqueueDuePersonEnrichmentContext(
		t.Context(), f.now, 200, []string{f.profiles[0].Fingerprint})
	require.NoError(err)
	assert.Zero(count)
	rows := f.work(t, 0)
	require.Len(rows, 1)
	assert.Equal(future, rows[0].DueAt)
	assert.Equal(int64(8), rows[0].TriggerMask)
	assert.Equal("attempt:fresh-refresh", rows[0].TriggerGeneration)
}

func TestPersonEnrichmentTriggerConsentGrantAndRevocationCancelPendingWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentTriggerFixture(t, 1)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	consent := f.grant(t, 0)
	rows := f.work(t, 0)
	require.Len(rows, 1)
	assert.Equal("consent:"+formatEnrichmentTriggerID(consent.ID), rows[0].TriggerGeneration)

	run, _, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: "consent-revoke", RequestedAt: f.now,
	})
	require.NoError(err)
	lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "test-worker", ProviderName: f.profiles[0].Name,
		Now: f.now, LeaseDuration: time.Minute,
	})
	require.NoError(err)
	require.NotNil(lease)
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, personenrichment.AttemptStart{
		RunID: run.ID, PersonID: f.person.ID, ProfileFingerprint: f.profiles[0].Fingerprint,
		PayloadHash: strings.Repeat("a", 64), RequestHash: strings.Repeat("b", 64),
		PersonRevision: f.person.Revision, Trigger: lease.Trigger,
	})
	require.NoError(err)
	require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
	require.NoError(f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
		State: personenrichment.AttemptPending, JobID: "opaque-consent-job", StartedAt: f.now,
		AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
		ProgramFingerprint: strings.Repeat("c", 64),
	}))
	require.NoError(f.store.SchedulePoll(t.Context(), attempt.Token, personenrichment.Result{
		State: personenrichment.ResultPending, JobID: "opaque-consent-job", PollAfter: time.Minute,
		AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
	}))

	changed, err := f.store.RevokePersonEnrichmentConsent(t.Context(), f.profiles[0].Fingerprint, "test")
	require.NoError(err)
	assert.True(changed)
	assert.Empty(f.work(t, 0))
	stored, err := f.store.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
	require.NoError(err)
	assert.Equal("terminal", stored.State)
}

func TestPersonEnrichmentMergeAndSplitInvalidateProviderIdentities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("merge-a@example.test", "Merge A", "example.test")
	b := f.EnsureParticipant("merge-b@example.test", "Merge B", "example.test")
	_, err := f.Store.LinkParticipants(a, b)
	require.NoError(err)
	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), a)
	require.NoError(err)
	profile := enrichmentTriggerProfile(t, "merge-provider", "https://merge.example.test/search")
	_, err = f.Store.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(err)
	_, _, err = f.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(err)
	_, err = f.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(err)
	require.NoError(clearEnrichmentWork(t, f.Store, person.ID))
	insertProviderIdentity(t, f.Store, person.ID, profile.ProviderNamespace, "opaque-before-merge")

	require.NoError(f.Store.MergeParticipants(b, a))
	assert.Zero(providerIdentityCount(t, f.Store, person.ID))
	rows, err := f.Store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: person.ID, ProfileFingerprint: profile.Fingerprint, Limit: 200,
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(int64(2), rows[0].TriggerMask)

	require.NoError(clearEnrichmentWork(t, f.Store, person.ID))
	c := f.EnsureParticipant("split-c@example.test", "Split C", "example.test")
	_, err = f.Store.LinkParticipants(a, c)
	require.NoError(err)
	require.NoError(clearEnrichmentWork(t, f.Store, person.ID))
	insertProviderIdentity(t, f.Store, person.ID, profile.ProviderNamespace, "opaque-before-split")
	_, err = f.Store.UnlinkParticipants(a, c)
	require.NoError(err)
	assert.Zero(providerIdentityCount(t, f.Store, person.ID))
	rows, err = f.Store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: person.ID, ProfileFingerprint: profile.Fingerprint, Limit: 200,
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(int64(2), rows[0].TriggerMask)
}

func TestPersonEnrichmentRepromotionInvalidatesNewParticipantIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEnrichmentTriggerFixture(t, 1)
	f.grant(t, 0)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))
	insertProviderIdentity(t, f.store, f.person.ID,
		f.profiles[0].ProviderNamespace, "opaque-before-repromotion")
	aliasID, err := f.store.EnsureParticipant(
		"repromotion-alias@example.test", "Repromotion Alias", "example.test")
	require.NoError(err)
	participantID := f.person.ParticipantIDs[0]
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO participant_links (participant_a, participant_b) VALUES (?, ?)`),
		participantID, aliasID)
	require.NoError(err)

	promoted, created, err := f.store.CreatePersonFromParticipantContext(t.Context(), aliasID)
	require.NoError(err)
	assert.False(created)
	assert.Equal([]int64{participantID, aliasID}, promoted.ParticipantIDs)
	assert.Zero(providerIdentityCount(t, f.store, f.person.ID))
	work := f.work(t, 0)
	require.Len(work, 1)
	assert.Equal(int64(2), work[0].TriggerMask)
}

func TestPersonEnrichmentPersonMergeInvalidatesSurvivorIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	survivorParticipant := f.EnsureParticipant(
		"profile-merge-survivor@example.test", "Profile Merge Survivor", "example.test",
	)
	absorbedParticipant := f.EnsureParticipant(
		"profile-merge-absorbed@example.test", "Profile Merge Absorbed", "example.test",
	)
	survivor, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), survivorParticipant)
	require.NoError(err)
	absorbed, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), absorbedParticipant)
	require.NoError(err)
	profile := enrichmentTriggerProfile(
		t, "profile-merge-provider", "https://profile-merge.example.test/search",
	)
	_, err = f.Store.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(err)
	_, _, err = f.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(err)
	_, err = f.Store.SetPersonTrackingContext(t.Context(), survivor.ID, true)
	require.NoError(err)
	require.NoError(clearEnrichmentWork(t, f.Store, survivor.ID))
	insertProviderIdentity(
		t, f.Store, survivor.ID, profile.ProviderNamespace, "opaque-before-profile-merge",
	)
	survivor, err = f.Store.GetPersonContext(t.Context(), survivor.ID)
	require.NoError(err)
	absorbed, err = f.Store.GetPersonContext(t.Context(), absorbed.ID)
	require.NoError(err)

	_, err = f.Store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision,
		ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey:           "enrichment-profile-merge", Actor: "test",
	})
	require.NoError(err)
	assert.Zero(providerIdentityCount(t, f.Store, survivor.ID))
	rows, err := f.Store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: survivor.ID, ProfileFingerprint: profile.Fingerprint, Limit: 200,
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(int64(2), rows[0].TriggerMask)
}

func TestPersonEnrichmentUnlinkWithoutAuthorizationPreservesPersonRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("unauthorized-unlink-a@example.test", "Unlink A", "example.test")
	b := f.EnsureParticipant("unauthorized-unlink-b@example.test", "Unlink B", "example.test")
	_, err := f.Store.LinkParticipants(a, b)
	require.NoError(err)
	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), a)
	require.NoError(err)
	insertProviderIdentity(t, f.Store, person.ID, "unauthorized-unlink", "opaque-before-unlink")

	_, err = f.Store.UnlinkParticipants(a, b)
	require.NoError(err)
	current, err := f.Store.GetPersonContext(t.Context(), person.ID)
	require.NoError(err)
	assert.Equal(person.Revision, current.Revision)
	assert.Zero(providerIdentityCount(t, f.Store, person.ID))
}

func TestPersonEnrichmentEmploymentWithoutAuthorizationPreservesPersonRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	participant := f.EnsureParticipant(
		"unauthorized-employment@example.test", "Employment", "example.test",
	)
	person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participant)
	require.NoError(err)
	organization, err := f.Store.CreateOrganizationContext(t.Context(), store.OrganizationInput{
		Name: "Synthetic Employer", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)

	_, err = f.Store.AddEmploymentContext(t.Context(), store.EmploymentInput{
		PersonID: person.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceCardDAVImport,
	})
	require.NoError(err)
	current, err := f.Store.GetPersonContext(t.Context(), person.ID)
	require.NoError(err)
	assert.Equal(person.Revision, current.Revision)
}

func TestOrganizationIdentityChangesInvalidateEnrichment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentTriggerFixture(t, 1)
	f.grant(t, 0)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.person.ID, true)
	require.NoError(err)
	organization, err := f.store.CreateOrganizationContext(t.Context(), store.OrganizationInput{
		Name: "Initial Employer", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	_, err = f.store.AddEmploymentContext(t.Context(), store.EmploymentInput{
		PersonID: f.person.ID, OrganizationID: organization.ID,
		Title: new("Engineer"), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))
	insertProviderIdentity(t, f.store, f.person.ID,
		f.profiles[0].ProviderNamespace, "opaque-before-organization-rename")
	before, err := f.store.GetPersonContext(t.Context(), f.person.ID)
	require.NoError(err)

	organization, err = f.store.ReplaceOrganizationContext(t.Context(), organization.ID,
		organization.Revision, store.OrganizationInput{
			Name: "Renamed Employer", Kind: store.OrganizationKindCompany,
		}, false)
	require.NoError(err)
	after, err := f.store.GetPersonContext(t.Context(), f.person.ID)
	require.NoError(err)
	assert.Equal(before.Revision+1, after.Revision)
	assert.Zero(providerIdentityCount(t, f.store, f.person.ID))
	work := f.work(t, 0)
	require.Len(work, 1)
	assert.Equal(int64(2), work[0].TriggerMask)

	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))
	insertProviderIdentity(t, f.store, f.person.ID,
		f.profiles[0].ProviderNamespace, "opaque-before-organization-retirement")
	before = after
	organization, err = f.store.ReplaceOrganizationContext(t.Context(), organization.ID,
		organization.Revision, store.OrganizationInput{
			Name: organization.Name, Kind: store.OrganizationKindCompany,
		}, true)
	require.NoError(err)
	after, err = f.store.GetPersonContext(t.Context(), f.person.ID)
	require.NoError(err)
	assert.Equal(before.Revision+1, after.Revision)
	assert.Zero(providerIdentityCount(t, f.store, f.person.ID))
	work = f.work(t, 0)
	require.Len(work, 1)
	assert.Equal(int64(2), work[0].TriggerMask)

	require.NoError(clearEnrichmentWork(t, f.store, f.person.ID))
	insertProviderIdentity(t, f.store, f.person.ID,
		f.profiles[0].ProviderNamespace, "opaque-before-organization-merge")
	survivor, err := f.store.CreateOrganizationContext(t.Context(), store.OrganizationInput{
		Name: "Surviving Employer", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	before = after
	_, err = f.store.MergeOrganizationsContext(t.Context(), survivor.ID, survivor.Revision,
		organization.ID, organization.Revision)
	require.NoError(err)
	after, err = f.store.GetPersonContext(t.Context(), f.person.ID)
	require.NoError(err)
	assert.Equal(before.Revision+1, after.Revision)
	assert.Zero(providerIdentityCount(t, f.store, f.person.ID))
	work = f.work(t, 0)
	require.Len(work, 1)
	assert.Equal(int64(2), work[0].TriggerMask)
}

func TestCardDAVProjectionRetirementInvalidatesEnrichment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	input := remoteResource(book.CanonicalURL+"retired.vcf", "remote-retired",
		"Retired Person", "retired@example.test", `"one"`)
	_, err := st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, input.Href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	personID := *resource.PersonID
	profile := enrichmentTriggerProfile(t, "carddav-retirement-provider",
		"https://carddav-retirement.example.test/search")
	_, err = st.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(err)
	_, _, err = st.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(err)
	_, err = st.SetPersonTrackingContext(t.Context(), personID, true)
	require.NoError(err)
	require.NoError(clearEnrichmentWork(t, st, personID))
	insertProviderIdentity(t, st, personID, profile.ProviderNamespace,
		"opaque-before-carddav-retirement")
	before, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)

	discovery := cardDAVRediscoveryForBook(account, book)
	discovery.Username = "replacement-user"
	_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), discovery)
	require.NoError(err)
	after, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(before.Revision+1, after.Revision)
	assert.Zero(providerIdentityCount(t, st, personID))
	work, err := st.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: personID, ProfileFingerprint: profile.Fingerprint, Limit: 200,
	})
	require.NoError(err)
	require.Len(work, 1)
	assert.Equal(int64(2), work[0].TriggerMask)
}

func clearEnrichmentWork(t *testing.T, st *store.Store, personID int64) error {
	t.Helper()
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(
		`DELETE FROM person_enrichment_work WHERE person_id = ?`), personID)
	return err
}

func insertProviderIdentity(t *testing.T, st *store.Store, personID int64, namespace, providerID string) {
	t.Helper()
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
		INSERT INTO person_enrichment_provider_identities
			(person_id, provider_namespace, provider_person_id, confidence, verified_at)
		VALUES (?, ?, ?, 1000, ?)`), personID, namespace, providerID, time.Now().UTC())
	require.NoError(t, err)
}

func providerIdentityCount(t *testing.T, st *store.Store, personID int64) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM person_enrichment_provider_identities WHERE person_id = ?`),
		personID).Scan(&count))
	return count
}

func insertProviderClaim(
	t *testing.T, st *store.Store, personID int64, fingerprint, suffix string, validUntil time.Time,
) int64 {
	t.Helper()
	var generationID int64
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_generations
			(person_id, generation_key, source_cursors_json, program_id, program_version,
			 program_fingerprint, catalog_fingerprint, provider, provider_version,
			 model, model_version, provider_policy_fingerprint, resolved_at)
		VALUES (?, ?, '[]', 'trigger-test', 'v1', ?, 'trigger-catalog', 'fixture', 'v1',
		        'fixture', 'v1', ?, ?)
		RETURNING id`), personID, "provider-claim-generation-"+suffix, strings.Repeat("e", 64),
		fingerprint, validUntil.Add(-time.Hour)).Scan(&generationID)
	require.NoError(t, err)
	var claimID int64
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`
		INSERT INTO person_fact_claims
			(person_id, generation_id, claim_key, target_kind, target_key, target_revision,
			 relation, submitted_value_json, valid_until, origin, confidence_json)
		VALUES (?, ?, ?, 'attribute', 'attribute:bio', 'revision-1',
		        'support', '"synthetic"', ?, 'enrichment', '{}')
		RETURNING id`), personID, generationID, "provider-claim-"+suffix, validUntil).Scan(&claimID)
	require.NoError(t, err)
	return claimID
}

func formatEnrichmentTriggerID(id int64) string { return strconv.FormatInt(id, 10) }
