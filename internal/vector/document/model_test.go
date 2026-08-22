package document

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func TestFingerprintBindsDocumentExtractionAndEmbeddingPolicy(t *testing.T) {
	config := vector.Config{Embeddings: vector.EmbeddingsConfig{
		APIFormat: vector.APIFormatOpenAI, Model: "embed-v1", Dimension: 768, MaxInputChars: 8192,
	}}

	baseline := Fingerprint("extract-v1", config)
	assert.Regexp(t, "^[0-9a-f]{64}$", baseline)
	assert.Equal(t, baseline, Fingerprint("extract-v1", config))

	tests := []struct {
		name   string
		mutate func(*vector.Config)
	}{
		{name: "api format", mutate: func(c *vector.Config) { c.Embeddings.APIFormat = vector.APIFormatVoyageContextual }},
		{name: "model", mutate: func(c *vector.Config) { c.Embeddings.Model = "embed-v2" }},
		{name: "dimension", mutate: func(c *vector.Config) { c.Embeddings.Dimension++ }},
		{name: "max input chars", mutate: func(c *vector.Config) { c.Embeddings.MaxInputChars++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := config
			test.mutate(&changed)
			assert.NotEqual(t, baseline, Fingerprint("extract-v1", changed))
		})
	}
	assert.NotEqual(t, baseline, Fingerprint("extract-v2", config))
}

func TestFingerprintExcludesMessageCorpusAndCredentials(t *testing.T) {
	config := vector.Config{Embeddings: vector.EmbeddingsConfig{
		APIFormat: vector.APIFormatOpenAI, Model: "embed-v1", Dimension: 768, MaxInputChars: 8192,
	}}
	baseline := Fingerprint("extract-v1", config)

	changed := config
	changed.Embeddings.Endpoint = "https://embeddings.example.test/v1"
	changed.Embeddings.APIKeyEnv = "SYNTHETIC_EMBEDDING_KEY"
	stripQuotes := false
	changed.Preprocess.StripQuotes = &stripQuotes
	changed.Embed.Scope.MessageTypes = []string{"email"}
	changed.Embed.Scope.SourceIDs = []int64{7}

	assert.Equal(t, baseline, Fingerprint("extract-v1", changed))
}

func TestEgressFingerprintBindsCanonicalDestinationWithoutInvalidatingCorpus(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	config := vector.Config{Embeddings: vector.EmbeddingsConfig{
		Endpoint:  "https://user:secret@trusted.example.test/v1?api_key=secret#fragment",
		APIFormat: vector.APIFormatOpenAI, Model: "embed-v1", Dimension: 768, MaxInputChars: 8192,
	}}
	corpus := Fingerprint("extract-v1", config)
	baseline, err := EgressFingerprint("extract-v1", config)
	requirements.NoError(err)

	credentialChange := config
	credentialChange.Embeddings.Endpoint = "https://other:credential@trusted.example.test/v1?api_key=other"
	credentialFingerprint, err := EgressFingerprint("extract-v1", credentialChange)
	requirements.NoError(err)
	assertions.Equal(baseline, credentialFingerprint)

	hosted := config
	hosted.Embeddings.Endpoint = "https://hosted.example.test/v1"
	hostedFingerprint, err := EgressFingerprint("extract-v1", hosted)
	requirements.NoError(err)
	assertions.NotEqual(baseline, hostedFingerprint)
	assertions.Equal(corpus, Fingerprint("extract-v1", hosted))
}

func TestParseSearchMode(t *testing.T) {
	for _, test := range []struct {
		input string
		want  SearchMode
	}{
		{input: "", want: SearchModeAuto},
		{input: " AUTO ", want: SearchModeAuto},
		{input: "lexical", want: SearchModeLexical},
		{input: "SEMANTIC", want: SearchModeSemantic},
		{input: "hybrid", want: SearchModeHybrid},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := ParseSearchMode(test.input)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}

	_, err := ParseSearchMode("unsupported")
	require.ErrorContains(t, err, "unsupported")
}
