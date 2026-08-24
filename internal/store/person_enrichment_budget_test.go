package store_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func enrichmentBudgetProfile(t *testing.T, requestsPerRun, personCost, runCost, dayCost int64) personenrichment.ProviderProfile {
	t.Helper()
	profile, err := (personenrichment.ProviderConfig{
		Name: "exa-budget", Kind: personenrichment.ProviderExa, Enabled: true,
		Endpoint: "https://api.example.test/search", APIKeyEnv: "PROVIDER_API_KEY",
		Mode: "people", NumResults: 1,
		AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierEmail},
		TargetKeys:         []string{"attribute:bio"}, RetentionPosture: "zero_retention",
		TrainingPosture: "no_training", RefreshInterval: 24 * time.Hour,
		RequestTimeout: time.Minute, PollInterval: 30 * time.Second, MaxJobAge: 15 * time.Minute,
		MaxRetries: 5, MaxRequestsPerRun: requestsPerRun, MaxRequestsPerDay: 10,
		MaxCostUSDMicrosPerPersonPerDay: personCost, MaxCostUSDMicrosPerRun: runCost,
		MaxCostUSDMicrosPerDay: dayCost,
	}).Profile(personfacts.Catalog{Version: "1", Targets: []personfacts.TargetDescriptor{{
		Kind: personfacts.TargetAttribute, Key: "attribute:bio", Revision: "revision-1",
		UniversalID: "attribute:bio", Slug: "bio", Description: "Fixture biography",
		ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
		Choices: []personfacts.ChoiceDescriptor{}, Fields: []personfacts.FieldDescriptor{},
	}}})
	require.NoError(t, err)
	return profile
}

type budgetClaim struct {
	token personenrichment.LeaseToken
	start personenrichment.AttemptStart
}

func newBudgetClaims(t *testing.T, profile personenrichment.ProviderProfile) (*store.Store, []budgetClaim) {
	t.Helper()
	fixture := storetest.New(t)
	_, err := fixture.Store.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(t, err)
	_, _, err = fixture.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(t, err)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	store.SetPersonEnrichmentClockForTest(fixture.Store, func() time.Time { return now })
	run, _, err := fixture.Store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "manual", RequestedBy: "budget-run", RequestedAt: now,
	})
	require.NoError(t, err)
	claims := make([]budgetClaim, 0, 2)
	for i, email := range []string{"budget-a@example.com", "budget-b@example.com"} {
		participantID := fixture.EnsureParticipant(email, "Budget Person", "example.com")
		person, _, createErr := fixture.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
		require.NoError(t, createErr)
		generation := "budget:" + string(rune('a'+i))
		require.NoError(t, fixture.Store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
			PersonID: person.ID, ProfileFingerprint: profile.Fingerprint,
			Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerManual, Generation: generation}, DueAt: now,
		}))
		lease, claimErr := fixture.Store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
			RunID: run.ID, Owner: "worker-" + generation, ProviderName: profile.Name,
			Now: now, LeaseDuration: time.Minute,
		})
		require.NoError(t, claimErr)
		require.NotNil(t, lease)
		claims = append(claims, budgetClaim{token: lease.Token, start: personenrichment.AttemptStart{
			RunID: run.ID, PersonID: person.ID, ProfileFingerprint: profile.Fingerprint,
			PayloadHash: hashForTest(byte('a' + i)), RequestHash: hashForTest(byte('c' + i)),
			PersonRevision: person.Revision, Trigger: lease.Trigger,
		}})
	}
	return fixture.Store, claims
}

func hashForTest(value byte) string {
	result := make([]byte, 64)
	for i := range result {
		result[i] = value
	}
	return string(result)
}

func TestPersonEnrichmentRequestBudgetContention(t *testing.T) {
	profile := enrichmentBudgetProfile(t, 1, 0, 0, 0)
	st, claims := newBudgetClaims(t, profile)
	assertExactlyOneBudgetReservation(t, st, claims, false)
}

func TestPersonEnrichmentGuaranteedCostBudgetContention(t *testing.T) {
	profile := enrichmentBudgetProfile(t, 10, 1000, 1000, 1000)
	st, claims := newBudgetClaims(t, profile)
	for i := range claims {
		claims[i].start.HardCostCap = true
		claims[i].start.GuaranteedMaxCost = personenrichment.Cost{Currency: "USD", AmountMicros: 600}
	}
	assertExactlyOneBudgetReservation(t, st, claims, true)
}

func TestPersonEnrichmentGuaranteedCostChecksOnlyConfiguredCaps(t *testing.T) {
	tests := []struct {
		name                         string
		personCost, runCost, dayCost int64
	}{
		{name: "person-day only", personCost: 1000},
		{name: "run only", runCost: 1000},
		{name: "provider-day only", dayCost: 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			profile := enrichmentBudgetProfile(t, 10, tt.personCost, tt.runCost, tt.dayCost)
			st, claims := newBudgetClaims(t, profile)
			for i := range claims {
				claims[i].start.HardCostCap = true
				claims[i].start.GuaranteedMaxCost = personenrichment.Cost{Currency: "USD", AmountMicros: 600}
			}
			_, created, err := st.BeginAttempt(t.Context(), claims[0].token, claims[0].start)
			requirements.NoError(err)
			assert.True(t, created)
			if tt.personCost > 0 {
				run, _, runErr := st.StartRun(t.Context(), personenrichment.RunStart{
					Kind: "manual", RequestedBy: "single-person-cap", RequestedAt: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC),
				})
				requirements.NoError(runErr)
				second := claims[0].start
				second.RunID = run.ID
				err = store.ReservePersonEnrichmentBudgetForTest(t.Context(), st, second)
			} else {
				_, _, err = st.BeginAttempt(t.Context(), claims[1].token, claims[1].start)
			}
			requirements.ErrorIs(err, store.ErrCostBudgetExceeded)
		})
	}
}

func TestPersonEnrichmentPersonDayBudgetContentionUsesProductionReservation(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	fixture := storetest.New(t)
	if !fixture.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL row-lock interleaving requires MSGVAULT_TEST_DB")
	}
	profile := enrichmentBudgetProfile(t, 10, 1000, 0, 0)
	_, err := fixture.Store.EnsurePersonEnrichmentProfile(t.Context(), profile)
	requirements.NoError(err)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	store.SetPersonEnrichmentClockForTest(fixture.Store, func() time.Time { return now })
	participantID := fixture.EnsureParticipant("same-budget-person@example.com", "Same Budget Person", "example.com")
	person, _, err := fixture.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)

	starts := make([]personenrichment.AttemptStart, 0, 2)
	for i := range 2 {
		run, _, startErr := fixture.Store.StartRun(t.Context(), personenrichment.RunStart{
			Kind: "manual", RequestedBy: "person-day-contention-" + string(rune('a'+i)), RequestedAt: now,
		})
		requirements.NoError(startErr)
		starts = append(starts, personenrichment.AttemptStart{
			RunID: run.ID, PersonID: person.ID, ProfileFingerprint: profile.Fingerprint,
			HardCostCap:       true,
			GuaranteedMaxCost: personenrichment.Cost{Currency: "USD", AmountMicros: 600},
		})
	}

	for _, start := range starts {
		requirements.NoError(store.EnsurePersonEnrichmentBudgetCountersForTest(t.Context(), fixture.Store, start))
	}
	installTwoPartyPersonEnrichmentBudgetBarrier(fixture.Store)
	errs := make(chan error, len(starts))
	var wg sync.WaitGroup
	for _, start := range starts {
		wg.Go(func() {
			errs <- store.ReservePersonEnrichmentBudgetForTest(t.Context(), fixture.Store, start)
		})
	}
	wg.Wait()
	close(errs)
	var success, exceeded int
	for reserveErr := range errs {
		switch {
		case reserveErr == nil:
			success++
		case errors.Is(reserveErr, store.ErrCostBudgetExceeded):
			exceeded++
		default:
			requirements.NoError(reserveErr)
		}
	}
	checks.Equal(1, success)
	checks.Equal(1, exceeded)
}

func assertExactlyOneBudgetReservation(t *testing.T, st *store.Store, claims []budgetClaim, cost bool) {
	t.Helper()
	if st.IsPostgreSQL() {
		for _, claim := range claims {
			require.NoError(t, store.EnsurePersonEnrichmentBudgetCountersForTest(t.Context(), st, claim.start))
		}
		installTwoPartyPersonEnrichmentBudgetBarrier(st)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(claims))
	created := make(chan bool, len(claims))
	for _, claim := range claims {
		wg.Go(func() {
			_, wasCreated, err := st.BeginAttempt(t.Context(), claim.token, claim.start)
			created <- wasCreated
			errs <- err
		})
	}
	wg.Wait()
	close(created)
	close(errs)
	var successes int
	for wasCreated := range created {
		if wasCreated {
			successes++
		}
	}
	assert.Equal(t, 1, successes)
	var budgetFailures int
	for err := range errs {
		if err != nil {
			if cost {
				require.ErrorIs(t, err, store.ErrCostBudgetExceeded)
			} else {
				require.ErrorIs(t, err, store.ErrRequestBudgetExceeded)
			}
			budgetFailures++
		}
	}
	assert.Equal(t, 1, budgetFailures)
}

func installTwoPartyPersonEnrichmentBudgetBarrier(st *store.Store) {
	var arrived atomic.Int32
	release := make(chan struct{})
	store.SetPersonEnrichmentBudgetBarrierForTest(st, func() {
		if arrived.Add(1) == 2 {
			close(release)
		}
		<-release
	})
}

func TestPersonEnrichmentBudgetRejectsUnsafeGuaranteesAndOnlyReconcilesDown(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	profile := enrichmentBudgetProfile(t, 10, 1000, 1000, 1000)
	st, claims := newBudgetClaims(t, profile)
	unsafe := claims[0].start
	unsafe.HardCostCap = true
	unsafe.GuaranteedMaxCost = personenrichment.Cost{Currency: "USD", AmountMicros: 600, Estimated: true}
	_, _, err := st.BeginAttempt(t.Context(), claims[0].token, unsafe)
	require.Error(err)

	valid := claims[0].start
	valid.HardCostCap = true
	valid.GuaranteedMaxCost = personenrichment.Cost{Currency: "USD", AmountMicros: 600}
	attempt, created, err := st.BeginAttempt(t.Context(), claims[0].token, valid)
	require.NoError(err)
	assert.True(created)
	require.NoError(store.CompletePersonEnrichmentAttemptForTest(t.Context(), st, attempt.Token, store.PersonEnrichmentAttemptCompletion{
		State: "succeeded", ActualCost: personenrichment.Cost{Currency: "USD", AmountMicros: 400},
	}))

	diagnostic, err := st.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
	require.NoError(err)
	require.NotNil(diagnostic.ActualCostUSDMicros)
	assert.Equal(int64(400), *diagnostic.ActualCostUSDMicros)
	counters, err := st.GetPersonEnrichmentRunCountersContext(t.Context(), attempt.RunID)
	require.NoError(err)
	assert.Equal(int64(0), counters.CostReservedUSDMicros)
	assert.Equal(int64(400), counters.CostChargedUSDMicros)
}

func TestPersonEnrichmentBudgetDisablesStartsWhenActualExceedsGuarantee(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	profile := enrichmentBudgetProfile(t, 10, 1000, 1000, 1000)
	st, claims := newBudgetClaims(t, profile)
	first := claims[0].start
	first.HardCostCap = true
	first.GuaranteedMaxCost = personenrichment.Cost{Currency: "USD", AmountMicros: 600}
	attempt, _, err := st.BeginAttempt(t.Context(), claims[0].token, first)
	require.NoError(err)

	err = store.CompletePersonEnrichmentAttemptForTest(t.Context(), st, attempt.Token, store.PersonEnrichmentAttemptCompletion{
		State: "succeeded", ActualCost: personenrichment.Cost{Currency: "USD", AmountMicros: 700},
	})
	require.ErrorIs(err, store.ErrProviderCostBoundExceeded)
	diagnostic, err := st.GetPersonEnrichmentAttemptContext(t.Context(), attempt.ID)
	require.NoError(err)
	assert.Equal("terminal", diagnostic.State)
	require.NotNil(diagnostic.FailureClass)
	assert.Equal("terminal", *diagnostic.FailureClass)

	second := claims[1].start
	second.HardCostCap = true
	second.GuaranteedMaxCost = personenrichment.Cost{Currency: "USD", AmountMicros: 100}
	_, _, err = st.BeginAttempt(t.Context(), claims[1].token, second)
	require.ErrorIs(err, store.ErrAccountingDisabled)
}

func TestPersonEnrichmentAttemptRejectsRequestHashCollisionAcrossPeople(t *testing.T) {
	require := require.New(t)
	profile := enrichmentBudgetProfile(t, 10, 0, 0, 0)
	st, claims := newBudgetClaims(t, profile)
	first, _, err := st.BeginAttempt(t.Context(), claims[0].token, claims[0].start)
	require.NoError(err)

	colliding := claims[1].start
	colliding.RequestHash = claims[0].start.RequestHash
	got, created, err := st.BeginAttempt(t.Context(), claims[1].token, colliding)
	require.ErrorIs(err, store.ErrRequestHashConflict)
	assert.Nil(t, got)
	assert.False(t, created)
	assert.Equal(t, claims[0].start.PersonID, first.Token.WorkPersonID)
}
