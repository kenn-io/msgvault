package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
)

// insertRawConsentProfiles stores one fingerprint in all three profile tables
// so a purpose leak cannot hide behind distinct fingerprints.
func insertRawConsentProfiles(t *testing.T, st *Store, fingerprint string) {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO person_inference_profiles
			(fingerprint, provider_kind, endpoint, model, api_key_env,
			 retention_posture, training_posture, allowed_sources, source_since, policy_json)
		 VALUES (?, 'openai_compatible', 'https://api.example.test/v1', 'gpt-test', 'TEST_KEY',
			 'zero_retention', 'no_training', '["conversation_text"]', '2025-01-01', '{}')`,
		`INSERT INTO person_enrichment_profiles
			(fingerprint, provider_name, provider_kind, provider_namespace, endpoint, api_key_env, policy_json)
		 VALUES (?, 'exa-primary', 'exa', 'https://api.example.test', 'https://api.example.test/search',
			 'PROVIDER_API_KEY', '{}')`,
		`INSERT INTO person_semantic_embedding_profiles
			(fingerprint, purpose, destination, api_format, model, api_key_env,
			 retention_posture, training_posture, renderer_policy,
			 disclosed_field_classes, corpus_scope, policy_json)
		 VALUES (?, 'semantic_person_embeddings', 'https://embedding.example.test/v1/embeddings',
			 'openai', 'synthetic-model', 'TEST_KEY', 'zero_data_retention', 'no_training',
			 'person-semantic-v1', '["person_display_name"]', 'all_durable_people', '{}')`,
	} {
		_, err := st.db.ExecContext(t.Context(), statement, fingerprint)
		require.NoError(t, err)
	}
}

func TestProviderConsentPurposeExact(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st, personID := newPersonFactLedgerStore(t)
	fingerprint := strings.Repeat("a", 64)
	insertRawConsentProfiles(t, st, fingerprint)
	_, err := st.SetPersonTrackingContext(ctx, personID, true)
	require.NoError(err)

	_, created, err := st.GrantPersonInferenceConsent(ctx, fingerprint, "cli")
	require.NoError(err)
	require.True(created)

	active, err := st.HasActivePersonInferenceConsent(ctx, fingerprint)
	require.NoError(err)
	assert.True(active)
	active, err = st.HasActivePersonEnrichmentConsent(ctx, fingerprint)
	require.NoError(err)
	assert.False(active, "an inference grant must not authorize enrichment")
	active, err = st.HasActivePersonSemanticEmbeddingConsent(ctx, fingerprint)
	require.NoError(err)
	assert.False(active, "an inference grant must not authorize semantic embeddings")

	require.NoError(st.EnqueuePersonEnrichmentContext(ctx, EnrichmentTriggerInput{
		PersonID: personID, ProfileFingerprint: fingerprint,
		Kind: personenrichment.TriggerManual, Generation: "manual:purpose-exact",
		DueAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}))
	work, err := st.ListPersonEnrichmentWorkContext(ctx, PersonEnrichmentWorkFilter{
		PersonID: personID, ProfileFingerprint: fingerprint, Limit: 10,
	})
	require.NoError(err)
	assert.Empty(work, "enrichment work must wait for an enrichment grant")

	_, created, err = st.GrantPersonEnrichmentConsent(ctx, fingerprint, "cli")
	require.NoError(err)
	require.True(created)
	revoked, err := st.RevokeAllPersonInferenceConsents(ctx, "cli")
	require.NoError(err)
	assert.Equal(int64(1), revoked)
	active, err = st.HasActivePersonEnrichmentConsent(ctx, fingerprint)
	require.NoError(err)
	assert.True(active, "revoking all inference grants must leave enrichment active")
	active, err = st.HasActivePersonInferenceConsent(ctx, fingerprint)
	require.NoError(err)
	assert.False(active)
}

func TestProviderConsentIDsGrowWithinPurpose(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	st, _ := newPersonFactLedgerStore(t)
	fingerprint := strings.Repeat("b", 64)
	insertRawConsentProfiles(t, st, fingerprint)

	first, _, err := st.GrantPersonInferenceConsent(ctx, fingerprint, "cli")
	require.NoError(err)
	enrichment, _, err := st.GrantPersonEnrichmentConsent(ctx, fingerprint, "cli")
	require.NoError(err)
	semantic, _, err := st.GrantPersonSemanticEmbeddingConsent(ctx, fingerprint, "cli")
	require.NoError(err)
	assert.Equal(int64(1), first.ID)
	assert.Equal(int64(1), enrichment.ID)
	assert.Equal(int64(1), semantic.ID)

	again, created, err := st.GrantPersonInferenceConsent(ctx, fingerprint, "cli")
	require.NoError(err)
	assert.False(created)
	assert.Equal(first.ID, again.ID)

	ids := []int64{first.ID}
	for range 2 {
		changed, err := st.RevokePersonInferenceConsent(ctx, fingerprint, "cli")
		require.NoError(err)
		require.True(changed)
		next, created, err := st.GrantPersonInferenceConsent(ctx, fingerprint, "cli")
		require.NoError(err)
		require.True(created)
		ids = append(ids, next.ID)
	}
	assert.Equal([]int64{1, 2, 3}, ids)

	status, err := st.PersonEnrichmentConsentStatus(ctx, fingerprint)
	require.NoError(err)
	require.NotNil(status.Consent)
	assert.Equal(int64(1), status.Consent.ID, "inference grants must not advance enrichment IDs")
}
