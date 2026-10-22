package store_test

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"strings"
	"testing"
	"time"
)

func tierEnrichmentProfile(t *testing.T, name, endpoint string) personenrichment.ProviderProfile {
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

type tierEnrichmentFixture struct {
	store   *store.Store
	person  *store.Person
	profile personenrichment.ProviderProfile
	now     time.Time
}

func newTierEnrichmentFixture(t *testing.T) tierEnrichmentFixture {
	t.Helper()
	fixture := storetest.New(t)
	participantID := fixture.EnsureParticipant("tier-person@example.test", "Tier Person", "example.test")
	person, _, err := fixture.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(t, err)
	profile := tierEnrichmentProfile(t, "tier-provider", "https://tier.example.test/search")
	_, err = fixture.Store.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(t, err)
	_, _, err = fixture.Store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(t, err)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	store.SetTierEnrichmentClockForTest(fixture.Store, func() time.Time { return now })
	_, err = fixture.Store.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(t, err)
	_, err = fixture.Store.DB().ExecContext(t.Context(), fixture.Store.Rebind(
		`DELETE FROM person_enrichment_work WHERE person_id = ?`), person.ID)
	require.NoError(t, err)
	return tierEnrichmentFixture{store: fixture.Store, person: person, profile: profile, now: now}
}

func (f *tierEnrichmentFixture) setNow(t time.Time) {
	f.now = t
	store.SetTierEnrichmentClockForTest(f.store, func() time.Time { return f.now })
}

func (f *tierEnrichmentFixture) startRun(t *testing.T, key string) personenrichment.DurableRun {
	t.Helper()
	run, created, err := f.store.StartRun(t.Context(), personenrichment.RunStart{
		Kind: "scheduled", RequestedBy: key, RequestedAt: f.now,
	})
	require.NoError(t, err)
	require.True(t, created)
	return *run
}

func (f *tierEnrichmentFixture) enqueue(t *testing.T) {
	t.Helper()
	require.NoError(t, f.store.PutPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Trigger: personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "revision:1"},
		DueAt:   f.now,
	}))
}

func (f *tierEnrichmentFixture) claim(t *testing.T, runID int64, owner string) *personenrichment.WorkLease {
	t.Helper()
	lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: runID, Owner: owner, ProviderName: f.profile.Name, ProfileFingerprint: f.profile.Fingerprint,
		Now: f.now, LeaseDuration: 5 * time.Minute,
	})
	require.NoError(t, err)
	require.NotNil(t, lease)
	return lease
}

func (f *tierEnrichmentFixture) work(t *testing.T) []store.PersonEnrichmentWork {
	t.Helper()
	rows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), store.PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	require.NoError(t, err)
	return rows
}

func tierEnrichmentAttemptStart(f *tierEnrichmentFixture, runID int64, hashByte string) personenrichment.AttemptStart {
	return personenrichment.AttemptStart{
		RunID: runID, PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		PayloadHash: strings.Repeat(hashByte, 64), RequestHash: strings.Repeat(hashByte, 64),
		PersonRevision:    f.person.Revision,
		Trigger:           personenrichment.Trigger{Kind: personenrichment.TriggerTracked, Generation: "revision:1"},
		GuaranteedMaxCost: personenrichment.Cost{},
	}
}

func insertTierEnrichmentClaim(
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

type tierEnrichmentPersona struct {
	name, provenance         string
	eligible, optedOut, want bool
}

var tierEnrichmentPersonas = []tierEnrichmentPersona{
	{"manual", "manual", false, false, true},
	{"auto-tier-1", "auto", true, false, true},
	{"auto-tier-2", "auto", false, false, false},
	{"opted-out", "manual", true, true, false},
}

func setTierEnrichmentPersona(t *testing.T, f tierEnrichmentFixture, p tierEnrichmentPersona) {
	t.Helper()
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_tracking SET provenance = ? WHERE person_id = ?`), p.provenance, f.person.ID)
	require.NoError(t, err)
	tier := 2
	if p.eligible {
		tier = 1
	}
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_tier_state
 (person_id,tier,tier_since,lanes_json,enrichment_eligible,rule,rule_version,config_fingerprint,as_of_local_date,run_id,run_started_at,applied_at)
 VALUES (?,?,?,'[]',?,'synthetic',1,'synthetic','2026-08-22','synthetic',?,?)`), f.person.ID, tier, f.now, p.eligible, f.now, f.now)
	require.NoError(t, err)
	if p.optedOut {
		_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_tier_opt_outs(person_id,lane,actor,created_at) VALUES (?,'enrichment','test',?)`), f.person.ID, f.now)
		require.NoError(t, err)
	}
}
