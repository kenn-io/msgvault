package store_test

import (
	"bytes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"testing"
	"time"
)

// A manual result must remain applicable even when scheduled enrichment is
// ineligible; otherwise tier automation would discard the human's request.
func TestTierEnrichmentManualResultCommitsForAutoTierTwo(t *testing.T) {
	f := newTierEnrichmentFixture(t)
	require.NoError(t, f.store.EnsureSeededAttributeDefinitionsContext(t.Context()))
	catalog, err := f.store.BuildPersonFactCatalogContext(t.Context(), false)
	require.NoError(t, err)
	require.NotEmpty(t, catalog.Targets)
	profile, err := (personenrichment.ProviderConfig{
		Name: "tier-result", Kind: personenrichment.ProviderExa, Enabled: true,
		Endpoint: "https://tier-result.example.test/search", APIKeyEnv: "TEST_TIER_RESULT_KEY", Mode: "deep", NumResults: 1,
		AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierEmail},
		TargetKeys:         []string{catalog.Targets[0].Key}, RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		RefreshInterval: 24 * time.Hour, RequestTimeout: time.Minute, PollInterval: time.Second, MaxJobAge: time.Hour,
		MaxRetries: 2, MaxRequestsPerRun: 100, MaxRequestsPerDay: 1000,
	}).Profile(catalog)
	require.NoError(t, err)
	_, err = f.store.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(t, err)
	_, _, err = f.store.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "test")
	require.NoError(t, err)
	f.profile = profile
	f.person, err = f.store.GetPersonContext(t.Context(), f.person.ID)
	require.NoError(t, err)
	setTierEnrichmentPersona(t, f, tierEnrichmentPersonas[2])
	run, _, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, profile.Fingerprint, "tier-two-result", f.now)
	require.NoError(t, err)
	lease := f.claim(t, run.ID, "result-worker")
	start := tierEnrichmentAttemptStart(&f, run.ID, "c")
	start.Trigger = lease.Trigger
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
	require.NoError(t, err)
	require.NoError(t, f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
	result := personenrichment.Result{State: personenrichment.ResultComplete, FreshAsOf: f.now,
		AdapterVersion: "tier-result-adapter", SchemaVersion: "tier-result-schema", ProviderVersion: "fixture-v1",
		Model: "synthetic-model", ModelVersion: "fixture-v1", Cost: personenrichment.Cost{Currency: "USD"}}
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x6a}, 32))
	require.NoError(t, err)
	commit, err := personenrichment.NewClaimCommit(personenrichment.ClaimCommitInput{
		AttemptID: attempt.ID, RunID: run.ID, PersonID: f.person.ID, LeaseFence: attempt.Token.Fence,
		ProfileFingerprint: profile.Fingerprint, ProviderNamespace: profile.ProviderNamespace, RequestHash: start.RequestHash,
		IdentityAssessment: personenrichment.IdentityAssessment{Accepted: true, Score: 1000, Reason: "strong_identifier_match", MatchedClasses: []personenrichment.IdentifierClass{personenrichment.IdentifierEmail}},
	}, result, hasher)
	require.NoError(t, err)
	outcome, err := f.store.CommitEnrichmentClaims(t.Context(), commit)
	require.NoError(t, err)
	assert.Equal(t, personenrichment.ClaimApplied, outcome.Status)
	var state string
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT state FROM person_enrichment_attempts WHERE id=?`), attempt.ID).Scan(&state))
	assert.Equal(t, "succeeded", state)
}
