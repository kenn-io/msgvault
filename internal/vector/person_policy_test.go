package vector

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const semanticPersonSearchQueryDisclosureToken = "caller_supplied_free_text_query_for_semantic_person_search"

func semanticPersonPolicyTestConfig() Config {
	return Config{
		Enabled: true,
		Backend: "sqlite-vec",
		Embeddings: EmbeddingsConfig{
			APIFormat: APIFormatOpenAI,
			Endpoint:  "https://embedding.example.test/v1",
			APIKeyEnv: "SEMANTIC_PERSON_TEST_KEY",
			Model:     "synthetic-embedding-model",
			Dimension: 4,
			BatchSize: 8,
		},
		People: PeopleConfig{
			Enabled:          true,
			RetentionPosture: "zero_data_retention",
			TrainingPosture:  "no_training",
		},
	}
}

func historicalSemanticPersonEmbeddingProfile(t *testing.T) SemanticPersonEmbeddingProfile {
	t.Helper()
	policy, err := semanticPersonEmbeddingPolicyForConfig(semanticPersonPolicyTestConfig())
	require.NoError(t, err)
	policy.DisclosedFieldClasses = slices.DeleteFunc(
		policy.DisclosedFieldClasses,
		func(field string) bool { return field == semanticPersonSearchQueryDisclosureToken },
	)
	profile, err := newSemanticPersonEmbeddingProfile(policy)
	require.NoError(t, err)
	return profile
}

// TestSemanticPersonEmbeddingsDefaultDisabled catches an upgrade silently
// opting existing vector configurations into curated-person egress.
func TestSemanticPersonEmbeddingsDefaultDisabled(t *testing.T) {
	assert.False(t, (Config{}).People.Enabled)
}

// TestSemanticPersonEmbeddingProfileBindsEveryDisclosureDimension catches an
// old exact consent authorizing any changed destination, wire format, model,
// credential name, provider posture, renderer, field class, or corpus scope.
func TestSemanticPersonEmbeddingProfileBindsEveryDisclosureDimension(t *testing.T) {
	basePolicy, err := semanticPersonEmbeddingPolicyForConfig(semanticPersonPolicyTestConfig())
	require.NoError(t, err)
	base, err := newSemanticPersonEmbeddingProfile(basePolicy)
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*semanticPersonEmbeddingPolicy)
	}{
		{name: "purpose", mutate: func(p *semanticPersonEmbeddingPolicy) { p.Purpose = "different_purpose" }},
		{name: "destination", mutate: func(p *semanticPersonEmbeddingPolicy) { p.Destination = "https://elsewhere.example.test/v1/embeddings" }},
		{name: "api format", mutate: func(p *semanticPersonEmbeddingPolicy) { p.APIFormat = APIFormatVoyageContextual }},
		{name: "model", mutate: func(p *semanticPersonEmbeddingPolicy) { p.Model = "different-model" }},
		{name: "credential env", mutate: func(p *semanticPersonEmbeddingPolicy) { p.APIKeyEnv = "DIFFERENT_KEY" }},
		{name: "retention posture", mutate: func(p *semanticPersonEmbeddingPolicy) { p.RetentionPosture = "provider_retains_30_days" }},
		{name: "training posture", mutate: func(p *semanticPersonEmbeddingPolicy) { p.TrainingPosture = "provider_may_train" }},
		{name: "renderer", mutate: func(p *semanticPersonEmbeddingPolicy) { p.RendererPolicy = "person-semantic-v2" }},
		{name: "disclosed fields", mutate: func(p *semanticPersonEmbeddingPolicy) {
			p.DisclosedFieldClasses = append(slices.Clone(p.DisclosedFieldClasses), "new_curated_field")
		}},
		{name: "corpus scope", mutate: func(p *semanticPersonEmbeddingPolicy) { p.CorpusScope = "subset_of_people" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changedPolicy := basePolicy
			changedPolicy.DisclosedFieldClasses = slices.Clone(basePolicy.DisclosedFieldClasses)
			test.mutate(&changedPolicy)
			changed, err := newSemanticPersonEmbeddingProfile(changedPolicy)
			require.NoError(t, err)
			assert.NotEqual(t, base.Fingerprint, changed.Fingerprint)
		})
	}
}

func TestSemanticPersonEmbeddingProfileDisclosesGlobalCuratedCorpusWithoutCredentialValue(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	t.Setenv("SEMANTIC_PERSON_TEST_KEY", "credential-value-must-not-persist")
	config := semanticPersonPolicyTestConfig()
	profile, err := config.SemanticPersonEmbeddingProfile()
	must.NoError(err)

	check.Equal(SemanticPersonEmbeddingPurpose, profile.Purpose)
	check.Equal(SemanticPersonCorpusAllDurablePeople, profile.CorpusScope)
	check.Equal(SemanticPersonRendererPolicy, profile.RendererPolicy)
	check.Equal(SemanticPersonDisclosedFieldClasses(), profile.DisclosedFieldClasses)
	check.Contains(profile.DisclosedFieldClasses, semanticPersonSearchQueryDisclosureToken)
	check.NotContains(string(profile.PolicyJSON), "credential-value-must-not-persist")
	check.Contains(string(profile.PolicyJSON), "SEMANTIC_PERSON_TEST_KEY")
	check.NoError(profile.Validate())
}

// TestHistoricalSemanticPersonEmbeddingProfileWithoutQueryDisclosureRemainsCanonical
// catches a current-policy disclosure expansion making stored audit profiles
// unreadable or silently rewriting their immutable fingerprint.
func TestHistoricalSemanticPersonEmbeddingProfileWithoutQueryDisclosureRemainsCanonical(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	profile := historicalSemanticPersonEmbeddingProfile(t)

	check.NotContains(profile.DisclosedFieldClasses, semanticPersonSearchQueryDisclosureToken)
	must.NoError(profile.Validate())
	canonical, err := profile.Canonical()
	must.NoError(err)
	check.Equal(profile.Fingerprint, canonical.Fingerprint)
	check.JSONEq(string(profile.PolicyJSON), string(canonical.PolicyJSON))
}

func TestSemanticPersonEmbeddingValidationRequiresExplicitProviderPostures(t *testing.T) {
	for _, field := range []string{"retention", "training"} {
		t.Run(field, func(t *testing.T) {
			config := semanticPersonPolicyTestConfig()
			if field == "retention" {
				config.People.RetentionPosture = "unknown"
			} else {
				config.People.TrainingPosture = ""
			}
			err := config.Validate()
			assert.ErrorContains(t, err, field+"_posture must be explicit")
		})
	}
}

// TestSemanticPersonEmbeddingValidationRejectsAmbiguousRuntimeIdentity catches
// canonical disclosure silently trimming or reinterpreting values that the
// initialized provider client would send verbatim.
func TestSemanticPersonEmbeddingValidationRejectsAmbiguousRuntimeIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "endpoint query", mutate: func(c *Config) {
			c.Embeddings.Endpoint += "?tenant=synthetic"
		}, want: "query"},
		{name: "endpoint fragment", mutate: func(c *Config) {
			c.Embeddings.Endpoint += "#fragment"
		}, want: "fragment"},
		{name: "model whitespace", mutate: func(c *Config) {
			c.Embeddings.Model = " " + c.Embeddings.Model
		}, want: "model"},
		{name: "credential environment whitespace", mutate: func(c *Config) {
			c.Embeddings.APIKeyEnv += " "
		}, want: "api_key_env"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := semanticPersonPolicyTestConfig()
			test.mutate(&config)
			assert.ErrorContains(t, config.Validate(), test.want)
		})
	}
}
