package document

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

func requireFingerprint(t *testing.T, extractionProfileID string, config vector.Config) string {
	t.Helper()
	fingerprint, err := Fingerprint(extractionProfileID, config)
	require.NoError(t, err)
	return fingerprint
}

func TestFingerprintBindsDocumentExtractionAndEmbeddingPolicy(t *testing.T) {
	check := assert.New(t)
	config := vector.Config{Embeddings: vector.EmbeddingsConfig{
		Endpoint:  "https://embeddings.example.test/v1",
		APIFormat: vector.APIFormatOpenAI, Model: "embed-v1", Dimension: 768, MaxInputChars: 8192,
	}}

	baseline := requireFingerprint(t, "extract-v1", config)
	check.Regexp("^[0-9a-f]{64}$", baseline)
	check.Equal("9e8ac42634c6e981a20950e20f612f4bf692dd460a07491643e2cb5853278f64", baseline)
	check.Equal(baseline, requireFingerprint(t, "extract-v1", config))

	tests := []struct {
		name   string
		mutate func(*vector.Config)
	}{
		{name: "api format", mutate: func(c *vector.Config) { c.Embeddings.APIFormat = vector.APIFormatVoyageContextual }},
		{name: "endpoint", mutate: func(c *vector.Config) { c.Embeddings.Endpoint = "https://other.example.test/v1" }},
		{name: "model", mutate: func(c *vector.Config) { c.Embeddings.Model = "embed-v2" }},
		{name: "dimension", mutate: func(c *vector.Config) { c.Embeddings.Dimension++ }},
		{name: "max input chars", mutate: func(c *vector.Config) { c.Embeddings.MaxInputChars++ }},
		{name: "document prefix", mutate: func(c *vector.Config) { c.Embeddings.DocumentPrefix = "search_document: " }},
		{name: "query prefix", mutate: func(c *vector.Config) { c.Embeddings.QueryPrefix = "search_query: " }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := config
			test.mutate(&changed)
			assert.New(t).NotEqual(baseline, requireFingerprint(t, "extract-v1", changed))
		})
	}
	check.NotEqual(baseline, requireFingerprint(t, "extract-v2", config))
}

func TestFingerprintExcludesMessageCorpusAndCredentials(t *testing.T) {
	config := vector.Config{Embeddings: vector.EmbeddingsConfig{
		Endpoint:  "https://embeddings.example.test/v1",
		APIFormat: vector.APIFormatOpenAI, Model: "embed-v1", Dimension: 768, MaxInputChars: 8192,
	}}
	baseline := requireFingerprint(t, "extract-v1", config)

	changed := config
	changed.Embeddings.APIKeyEnv = "SYNTHETIC_EMBEDDING_KEY"
	stripQuotes := false
	changed.Preprocess.StripQuotes = &stripQuotes
	changed.Embed.Scope.MessageTypes = []string{"email"}
	changed.Embed.Scope.SourceIDs = []int64{7}

	assert.Equal(t, baseline, requireFingerprint(t, "extract-v1", changed))
}

func TestEgressFingerprintBindsCanonicalDestinationAndCorpus(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	config := vector.Config{Embeddings: vector.EmbeddingsConfig{
		Endpoint:  "https://trusted.example.test/v1",
		APIFormat: vector.APIFormatOpenAI, Model: "embed-v1", Dimension: 768, MaxInputChars: 8192,
	}}
	corpus := requireFingerprint(t, "extract-v1", config)
	baseline, err := EgressFingerprint("extract-v1", config)
	requirements.NoError(err)

	credentialChange := config
	credentialChange.Embeddings.APIKeyEnv = "OTHER_SYNTHETIC_EMBEDDING_KEY"
	credentialFingerprint, err := EgressFingerprint("extract-v1", credentialChange)
	requirements.NoError(err)
	assertions.Equal(baseline, credentialFingerprint)

	hosted := config
	hosted.Embeddings.Endpoint = "https://hosted.example.test/v1"
	hostedFingerprint, err := EgressFingerprint("extract-v1", hosted)
	requirements.NoError(err)
	assertions.NotEqual(baseline, hostedFingerprint)
	assertions.NotEqual(corpus, requireFingerprint(t, "extract-v1", hosted))
	rotatedCorpusFingerprint, err := EgressFingerprint("extract-v2", config)
	requirements.NoError(err)
	assertions.NotEqual(baseline, rotatedCorpusFingerprint)
	queryFingerprint, err := QueryEgressFingerprint("extract-v1", config)
	requirements.NoError(err)
	assertions.NotEqual(baseline, queryFingerprint)

	unsafeEndpoint := config
	unsafeEndpoint.Embeddings.Endpoint = "https://user:secret@trusted.example.test/v1?api_key=secret"
	_, err = EgressFingerprint("extract-v1", unsafeEndpoint)
	requirements.Error(err)
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
