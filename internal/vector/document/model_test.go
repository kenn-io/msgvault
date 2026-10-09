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

func TestRawEmbeddingRecipeKeepsDocbankV014Identity(t *testing.T) {
	require := require.New(t)
	recipe, err := NewRecipe(RecipeConfig{Mode: RepresentationRaw, MaxInputRunes: 8192})
	require.NoError(err)
	encoded, err := recipe.CanonicalJSON()
	require.NoError(err)
	//nolint:testifylint // Canonical JSON bytes are part of the compatibility contract.
	assert.Equal(t,
		`{"version":1,"input_format_version":1,"mode":"raw","max_input_runes":8192,"max_filename_runes":256,"max_title_runes":512,"max_heading_runes":512}`,
		string(encoded))
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

// Both corpora and both document egress purposes must separate changes in the
// serving recipe. The serving alias is an operator convention, not verified
// checkpoint metadata returned by the provider.
func TestEmbeddingGemma2PolicyIdentities(t *testing.T) {
	check := assert.New(t)
	legacy := vector.Config{Embeddings: vector.EmbeddingsConfig{
		Endpoint: "https://embeddings.example.test/v1", Model: "embed-v1",
		Dimension: 768, MaxInputChars: 8192,
	}}
	old := embeddingPolicyIdentities(t, legacy)
	check.Equal("embed-v1:768:p1-111111:c8192:e1", old[0])
	check.Equal("9e8ac42634c6e981a20950e20f612f4bf692dd460a07491643e2cb5853278f64", old[1])
	explicitEmpty := legacy
	explicitEmpty.Embeddings.APIFormat = vector.APIFormatOpenAI
	check.Equal(old, embeddingPolicyIdentities(t, explicitEmpty))

	gemma := legacy
	gemma.Embeddings.Model = "embeddinggemma-2-914f7f89142e33e77833254d9c9b90c3cef7303b-text-fp32-768-v1"
	gemma.Embeddings.DocumentPrefix = "title: none | text: "
	gemma.Embeddings.QueryPrefix = "task: search result | query: "
	baseline := embeddingPolicyIdentities(t, gemma)
	for i := range baseline {
		check.NotEqual(old[i], baseline[i])
	}
	for _, test := range []struct {
		name   string
		mutate func(*vector.Config)
	}{
		{"serving revision", func(c *vector.Config) { c.Embeddings.Model += "-v2" }},
		{"reduced width", func(c *vector.Config) { c.Embeddings.Dimension = 512 }},
		{"document prompt", func(c *vector.Config) { c.Embeddings.DocumentPrefix = "title: Synthetic | text: " }},
		{"query prompt", func(c *vector.Config) { c.Embeddings.QueryPrefix = "task: question answering | query: " }},
		{"document trailing space", func(c *vector.Config) { c.Embeddings.DocumentPrefix = "title: none | text:" }},
		{"query trailing space", func(c *vector.Config) { c.Embeddings.QueryPrefix = "task: search result | query:" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := assert.New(t)
			changed := gemma
			test.mutate(&changed)
			got := embeddingPolicyIdentities(t, changed)
			for i := range baseline {
				check.NotEqual(baseline[i], got[i])
			}
		})
	}
}

// The tuple contains message, attachment-document, document-consent and
// query-consent identities. All own the configured vector semantics.
func embeddingPolicyIdentities(t *testing.T, cfg vector.Config) [4]string {
	t.Helper()
	document, err := Fingerprint("extract-v1", cfg)
	require.NoError(t, err)
	consent, err := EgressFingerprint("extract-v1", cfg)
	require.NoError(t, err)
	queryConsent, err := QueryEgressFingerprint("extract-v1", cfg)
	require.NoError(t, err)
	return [4]string{cfg.GenerationFingerprint(), document, consent, queryConsent}
}

// Literal affixes have role boundaries: moving bytes from query to document
// changes all identities even when the concatenated prefix bytes are equal.
func FuzzEmbeddingPrefixRoleBoundary(f *testing.F) {
	for _, seed := range [][2]string{
		{"", ""},
		{"title: none | text: ", "task: search result | query: "},
		{"a\x1fb", "c"},
		{"", " "},
		{"日本語", "查询"},
		{"a:b", "\x00"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, documentPrefix, queryPrefix string) {
		// Empty query has no bytes to move and therefore describes the same
		// policy. Keep that equivalence in the oracle rather than filtering it.
		left := vector.Config{Embeddings: vector.EmbeddingsConfig{
			Endpoint: "https://embeddings.example.test/v1", Model: "embed-v1",
			Dimension: 768, MaxInputChars: 8192,
			DocumentPrefix: documentPrefix, QueryPrefix: queryPrefix,
		}}
		right := left
		right.Embeddings.DocumentPrefix = documentPrefix + queryPrefix
		right.Embeddings.QueryPrefix = ""
		before, after := embeddingPolicyIdentities(t, left), embeddingPolicyIdentities(t, right)
		for i := range before {
			if queryPrefix == "" {
				assert.Equal(t, before[i], after[i])
			} else {
				assert.NotEqual(t, before[i], after[i])
			}
		}
	})
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
