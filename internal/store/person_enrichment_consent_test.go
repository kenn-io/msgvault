package store_test

import (
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func enrichmentTestProfile(t *testing.T) personenrichment.ProviderProfile {
	t.Helper()
	profile, err := (personenrichment.ProviderConfig{
		Name:               "exa-primary",
		Kind:               personenrichment.ProviderExa,
		Enabled:            true,
		Endpoint:           "https://api.example.test/search",
		APIKeyEnv:          "PROVIDER_API_KEY",
		Mode:               "deep",
		NumResults:         1,
		AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierEmail},
		TargetKeys:         []string{"attribute:bio"},
		RetentionPosture:   "zero_retention",
		TrainingPosture:    "no_training",
		RefreshInterval:    24 * time.Hour,
		RequestTimeout:     time.Minute,
		PollInterval:       30 * time.Second,
		MaxJobAge:          15 * time.Minute,
		MaxRetries:         5,
		MaxRequestsPerRun:  10,
		MaxRequestsPerDay:  100,
	}).Profile(personfacts.Catalog{
		Version: "1",
		Targets: []personfacts.TargetDescriptor{{
			Kind: personfacts.TargetAttribute, Key: "attribute:bio", Revision: "revision-1",
			UniversalID: "attribute:bio", Slug: "bio", Description: "Fixture biography",
			ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
			Choices: []personfacts.ChoiceDescriptor{}, Fields: []personfacts.FieldDescriptor{},
		}},
	})
	require.NoError(t, err)
	return profile
}

func TestPersonEnrichmentConsentLifecycleAndDistinctAuthority(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	profile := enrichmentTestProfile(t)

	status, err := st.PersonEnrichmentConsentStatus(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.Equal(profile.Fingerprint, status.Fingerprint)
	assert.False(status.ProfileExists)
	assert.False(status.Active)
	assert.Nil(status.Consent)

	_, _, err = st.GrantPersonEnrichmentConsent(t.Context(), profile.Fingerprint, "cli")
	require.ErrorContains(err, "does not exist")

	created, err := st.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(err)
	assert.True(created)
	created, err = st.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(err)
	assert.False(created)

	consent, created, err := st.GrantPersonEnrichmentConsent(
		t.Context(), profile.Fingerprint, "cli")
	require.NoError(err)
	assert.True(created)
	assert.Equal(profile.Fingerprint, consent.ProfileFingerprint)
	assert.Equal("cli", consent.GrantedBy)
	assert.Nil(consent.RevokedAt)

	again, created, err := st.GrantPersonEnrichmentConsent(
		t.Context(), profile.Fingerprint, "second-actor")
	require.NoError(err)
	assert.False(created)
	assert.Equal(consent.ID, again.ID)
	assert.Equal("cli", again.GrantedBy)

	active, err := st.HasActivePersonEnrichmentConsent(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.True(active)

	changed, err := st.RevokePersonEnrichmentConsent(
		t.Context(), profile.Fingerprint, "privacy-admin")
	require.NoError(err)
	assert.True(changed)
	changed, err = st.RevokePersonEnrichmentConsent(
		t.Context(), profile.Fingerprint, "privacy-admin")
	require.NoError(err)
	assert.False(changed)

	status, err = st.PersonEnrichmentConsentStatus(t.Context(), profile.Fingerprint)
	require.NoError(err)
	assert.True(status.ProfileExists)
	assert.False(status.Active)
	assert.Nil(status.Consent)
	require.NotNil(status.LastRevoked)
	assert.Equal(consent.ID, status.LastRevoked.ID)
	require.NotNil(status.LastRevoked.RevokedBy)
	assert.Equal("privacy-admin", *status.LastRevoked.RevokedBy)

	regranted, created, err := st.GrantPersonEnrichmentConsent(
		t.Context(), profile.Fingerprint, "cli")
	require.NoError(err)
	assert.True(created)
	assert.NotEqual(consent.ID, regranted.ID)

	inferenceProfile := inferenceTestProfile(t)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), inferenceProfile)
	require.NoError(err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), inferenceProfile.Fingerprint, "cli")
	require.NoError(err)
	active, err = st.HasActivePersonEnrichmentConsent(t.Context(), inferenceProfile.Fingerprint)
	require.NoError(err)
	assert.False(active, "inference consent must never authorize enrichment")
	_, _, err = st.GrantPersonEnrichmentConsent(t.Context(), inferenceProfile.Fingerprint, "cli")
	require.ErrorContains(err, "does not exist")
}

func TestPersonEnrichmentConsentRejectsImmutableProfileCollisions(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	profile := enrichmentTestProfile(t)

	_, err := st.EnsurePersonEnrichmentProfile(t.Context(), profile)
	requirements.NoError(err)

	mutations := []struct {
		name   string
		mutate func(*personenrichment.ProviderProfile)
	}{
		{"provider name", func(p *personenrichment.ProviderProfile) { p.Name = "different" }},
		{"fingerprint", func(p *personenrichment.ProviderProfile) { p.Fingerprint = "invalid" }},
		{"provider kind", func(p *personenrichment.ProviderProfile) { p.Kind = personenrichment.ProviderSixtyfour }},
		{"provider namespace", func(p *personenrichment.ProviderProfile) { p.ProviderNamespace = "exa:different" }},
		{"catalog fingerprint", func(p *personenrichment.ProviderProfile) { p.CatalogFingerprint = "different" }},
		{"start endpoint", func(p *personenrichment.ProviderProfile) { p.Endpoint = "https://other.example.test/search" }},
		{"poll endpoint", func(p *personenrichment.ProviderProfile) { p.PollEndpoint = "https://other.example.test/poll" }},
		{"credential name", func(p *personenrichment.ProviderProfile) { p.APIKeyEnv = "OTHER_API_KEY" }},
		{"mode", func(p *personenrichment.ProviderProfile) { p.Mode = "deep-reasoning" }},
		{"tier", func(p *personenrichment.ProviderProfile) { p.Tier = "standard" }},
		{"result count", func(p *personenrichment.ProviderProfile) { p.NumResults++ }},
		{"allowed identifiers", func(p *personenrichment.ProviderProfile) {
			p.AllowedIdentifiers = []personenrichment.IdentifierClass{personenrichment.IdentifierName}
		}},
		{"target catalog", func(p *personenrichment.ProviderProfile) { p.Targets[0].Revision = "revision-2" }},
		{"sensitive target posture", func(p *personenrichment.ProviderProfile) { p.AllowSensitiveTargets = true }},
		{"retention posture", func(p *personenrichment.ProviderProfile) { p.RetentionPosture = "provider_retained" }},
		{"training posture", func(p *personenrichment.ProviderProfile) { p.TrainingPosture = "provider_training" }},
		{"refresh cadence", func(p *personenrichment.ProviderProfile) { p.RefreshInterval++ }},
		{"per-run request cap", func(p *personenrichment.ProviderProfile) { p.MaxRequestsPerRun++ }},
		{"daily request cap", func(p *personenrichment.ProviderProfile) { p.MaxRequestsPerDay++ }},
		{"per-person daily cost cap", func(p *personenrichment.ProviderProfile) {
			p.MaxCostUSDMicrosPerPersonPerDay++
		}},
		{"per-run cost cap", func(p *personenrichment.ProviderProfile) { p.MaxCostUSDMicrosPerRun++ }},
		{"daily cost cap", func(p *personenrichment.ProviderProfile) { p.MaxCostUSDMicrosPerDay++ }},
		{"policy bytes", func(p *personenrichment.ProviderProfile) { p.PolicyJSON = append(p.PolicyJSON, '\n') }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			colliding := profile
			colliding.AllowedIdentifiers = slices.Clone(profile.AllowedIdentifiers)
			colliding.Targets = slices.Clone(profile.Targets)
			colliding.PolicyJSON = slices.Clone(profile.PolicyJSON)
			mutation.mutate(&colliding)
			_, collisionErr := st.EnsurePersonEnrichmentProfile(t.Context(), colliding)
			require.Error(t, collisionErr)
			if mutation.name == "provider name" {
				assert.ErrorContains(t, collisionErr, "different immutable policy")
			}
		})
	}
}

func TestPersonEnrichmentConsentConcurrentGrantAndRevoke(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	profile := enrichmentTestProfile(t)
	_, err := st.EnsurePersonEnrichmentProfile(t.Context(), profile)
	require.NoError(err)

	const workers = 8
	var grants sync.WaitGroup
	var createdCount atomic.Int64
	grantIDs := make(chan int64, workers)
	grantErrors := make(chan error, workers)
	for range workers {
		grants.Go(func() {
			consent, created, grantErr := st.GrantPersonEnrichmentConsent(
				t.Context(), profile.Fingerprint, "cli")
			if grantErr != nil {
				grantErrors <- grantErr
				return
			}
			if created {
				createdCount.Add(1)
			}
			grantIDs <- consent.ID
		})
	}
	grants.Wait()
	close(grantErrors)
	close(grantIDs)
	for grantErr := range grantErrors {
		require.NoError(grantErr)
	}
	assert.Equal(int64(1), createdCount.Load())
	var firstID int64
	for id := range grantIDs {
		if firstID == 0 {
			firstID = id
		}
		assert.Equal(firstID, id)
	}

	var revokes sync.WaitGroup
	var changedCount atomic.Int64
	revokeErrors := make(chan error, workers)
	for range workers {
		revokes.Go(func() {
			changed, revokeErr := st.RevokePersonEnrichmentConsent(
				t.Context(), profile.Fingerprint, "cli")
			if revokeErr != nil {
				revokeErrors <- revokeErr
				return
			}
			if changed {
				changedCount.Add(1)
			}
		})
	}
	revokes.Wait()
	close(revokeErrors)
	for revokeErr := range revokeErrors {
		require.NoError(revokeErr)
	}
	assert.Equal(int64(1), changedCount.Load())
}

func TestRevokeAllPersonEnrichmentConsentsSerializesRacingGrant(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	st := testutil.NewTestStore(t)
	first := enrichmentTriggerProfile(t, "revoke-all-first", "https://first.example.test/search")
	second := enrichmentTriggerProfile(t, "revoke-all-second", "https://second.example.test/search")
	for _, profile := range []personenrichment.ProviderProfile{first, second} {
		_, err := st.EnsurePersonEnrichmentProfile(t.Context(), profile)
		requirements.NoError(err)
	}
	_, _, err := st.GrantPersonEnrichmentConsent(t.Context(), first.Fingerprint, "test")
	requirements.NoError(err)

	snapshotted := make(chan struct{})
	release := make(chan struct{})
	var snapshotOnce sync.Once
	store.SetPersonEnrichmentTxBarrierForTest(st, func(phase string) {
		if phase == "revoke_all_consents_snapshotted" {
			snapshotOnce.Do(func() { close(snapshotted) })
			<-release
		}
	})
	type revokeResult struct {
		count int64
		err   error
	}
	revokeDone := make(chan revokeResult, 1)
	go func() {
		count, revokeErr := st.RevokeAllPersonEnrichmentConsents(t.Context(), "privacy-test")
		revokeDone <- revokeResult{count: count, err: revokeErr}
	}()
	<-snapshotted

	type grantResult struct {
		created bool
		err     error
	}
	grantStarted := make(chan struct{})
	grantDone := make(chan grantResult, 1)
	go func() {
		close(grantStarted)
		_, created, grantErr := st.GrantPersonEnrichmentConsent(
			t.Context(), second.Fingerprint, "racing-grant")
		grantDone <- grantResult{created: created, err: grantErr}
	}()
	<-grantStarted
	var grant grantResult
	grantFinishedBeforeRelease := false
	select {
	case grant = <-grantDone:
		grantFinishedBeforeRelease = true
	case <-time.After(time.Second):
	}
	close(release)
	revoked := <-revokeDone
	if !grantFinishedBeforeRelease {
		grant = <-grantDone
	}

	requirements.NoError(revoked.err)
	requirements.NoError(grant.err)
	checks.False(grantFinishedBeforeRelease,
		"grant must wait until revoke-all releases the authority lock")
	checks.Equal(int64(1), revoked.count)
	checks.True(grant.created)
	active, err := st.HasActivePersonEnrichmentConsent(t.Context(), second.Fingerprint)
	requirements.NoError(err)
	checks.True(active, "the serialized grant occurs after revoke-all")
}

var _ personenrichment.ConsentChecker = (*store.Store)(nil)
