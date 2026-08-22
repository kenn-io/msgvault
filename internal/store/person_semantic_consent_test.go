package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
)

func semanticPersonTestProfile(t *testing.T) vector.SemanticPersonEmbeddingProfile {
	t.Helper()
	config := vector.Config{
		Enabled: true,
		Backend: "sqlite-vec",
		Embeddings: vector.EmbeddingsConfig{
			APIFormat: vector.APIFormatOpenAI,
			Endpoint:  "https://embedding.example.test/v1",
			APIKeyEnv: "SEMANTIC_PERSON_TEST_KEY",
			Model:     "synthetic-embedding-model",
			Dimension: 4,
			BatchSize: 8,
		},
		People: vector.PeopleConfig{
			Enabled:          true,
			RetentionPosture: "zero_data_retention",
			TrainingPosture:  "no_training",
		},
	}
	profile, err := config.SemanticPersonEmbeddingProfile()
	require.NoError(t, err)
	return profile
}

// TestPersonSemanticConsentLifecycle catches semantic grants being stored in
// the people-sweep consent namespace or revocation failing to stop authority.
func TestPersonSemanticConsentLifecycle(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	st := testutil.NewTestStore(t)
	profile := semanticPersonTestProfile(t)

	status, err := st.GetPersonSemanticEmbeddingConsentStatus(t.Context(), profile.Fingerprint)
	must.NoError(err)
	check.False(status.ProfileExists)
	check.False(status.Active)

	created, err := st.EnsurePersonSemanticEmbeddingProfile(t.Context(), profile)
	must.NoError(err)
	check.True(created)
	created, err = st.EnsurePersonSemanticEmbeddingProfile(t.Context(), profile)
	must.NoError(err)
	check.False(created)

	consent, created, err := st.GrantPersonSemanticEmbeddingConsent(
		t.Context(), profile.Fingerprint, "cli")
	must.NoError(err)
	check.True(created)
	check.Equal(profile.Fingerprint, consent.ProfileFingerprint)
	active, err := st.HasActivePersonSemanticEmbeddingConsent(t.Context(), profile.Fingerprint)
	must.NoError(err)
	check.True(active)

	changed, err := st.RevokePersonSemanticEmbeddingConsent(
		t.Context(), profile.Fingerprint, "cli")
	must.NoError(err)
	check.True(changed)
	active, err = st.HasActivePersonSemanticEmbeddingConsent(t.Context(), profile.Fingerprint)
	must.NoError(err)
	check.False(active)

	status, err = st.GetPersonSemanticEmbeddingConsentStatus(t.Context(), profile.Fingerprint)
	must.NoError(err)
	check.False(status.Active)
	must.NotNil(status.LastRevoked)
	check.Equal(consent.ID, status.LastRevoked.ID)

	_, created, err = st.GrantPersonSemanticEmbeddingConsent(
		t.Context(), profile.Fingerprint, "cli")
	must.NoError(err)
	check.True(created)
}

// TestPeopleSweepConsentCannotAuthorizeSemanticPersonEmbeddings catches a
// purpose-confused lookup accepting a grant from the pre-existing subsystem.
func TestPeopleSweepConsentCannotAuthorizeSemanticPersonEmbeddings(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	st := testutil.NewTestStore(t)
	inference := inferenceTestProfile(t)
	_, err := st.EnsurePersonInferenceProfile(t.Context(), inference)
	must.NoError(err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), inference.Fingerprint, "cli")
	must.NoError(err)

	semantic := semanticPersonTestProfile(t)
	active, err := st.HasActivePersonSemanticEmbeddingConsent(t.Context(), semantic.Fingerprint)
	must.NoError(err)
	check.False(active)
}

// TestPersonSemanticStoredProfileRejectsImmutablePolicyCorruption catches an
// audit/list path trusting mutable columns or noncanonical policy JSON.
func TestPersonSemanticStoredProfileRejectsImmutablePolicyCorruption(t *testing.T) {
	st := testutil.NewTestStore(t)
	profile := semanticPersonTestProfile(t)
	_, err := st.EnsurePersonSemanticEmbeddingProfile(t.Context(), profile)
	require.NoError(t, err)

	_, err = st.DB().Exec(st.Rebind(`
		UPDATE person_semantic_embedding_profiles
		SET renderer_policy = ? WHERE fingerprint = ?`),
		"person-semantic-corrupted", profile.Fingerprint)
	require.NoError(t, err)

	_, err = st.ListPersonSemanticEmbeddingProfiles(t.Context())
	assert.ErrorContains(t, err, "immutable policy")
}

var _ interface {
	HasActivePersonSemanticEmbeddingConsent(ctx context.Context, fingerprint string) (bool, error)
} = (*store.Store)(nil)
