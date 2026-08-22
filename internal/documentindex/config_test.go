package documentindex

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mistral"
)

func TestDocumentsConfigDefaultsAreValidAndOptIn(t *testing.T) {
	assert := assert.New(t)
	config := DefaultDocumentsConfig()

	require.NoError(t, config.Validate())
	assert.False(config.Enabled)
	assert.Equal(ProviderMistral, config.Provider)
	assert.Equal(RegionMistralEU, config.Region)
	assert.Equal(ModelMistralOCR, config.Model)
	assert.Equal(RetentionUnknown, config.RetentionPosture)
	assert.Equal(TrainingUnknown, config.TrainingPosture)
	assert.True(config.LexicalEnabled())
	assert.True(config.StoresChunkText())
	assert.False(config.Index.Embeddings.Enabled)
	assert.Equal("vector.embeddings", config.Index.Embeddings.Profile)
	assert.Equal(int64(512<<20), config.MaxSpoolBytes)
	assert.Equal(int64(1<<30), config.MinFreeSpaceBytes)
}

func TestDocumentsConfigDoesNotDefaultExplicitZeroSafetyLimits(t *testing.T) {
	config := DefaultDocumentsConfig()
	config.MaxFileBytes = 0
	config.ApplyDefaults()

	require.ErrorContains(t, config.Validate(), "max_file_bytes: must be positive")
	assert.Zero(t, config.MaxFileBytes)
}

func TestDocumentsConfigRejectsUnavailableIndexOptOut(t *testing.T) {
	lexical := false
	store := false
	config := DefaultDocumentsConfig()
	config.Index = IndexConfig{Lexical: &lexical, StoreChunkText: &store}

	require.ErrorContains(t, config.Validate(), "must both be true")
	assert.False(t, config.LexicalEnabled())
	assert.False(t, config.StoresChunkText())
}

func TestDocumentsConfigAllowsNamedEmbeddingProfileWithLexicalFallback(t *testing.T) {
	config := DefaultDocumentsConfig()
	config.Index.Embeddings.Enabled = true
	config.Index.Embeddings.Profile = "vector.embeddings"

	require.NoError(t, config.Validate())
	assert.True(t, config.LexicalEnabled())
	assert.True(t, config.StoresChunkText())
}

func TestDocumentsConfigRejectsUnknownEmbeddingProfile(t *testing.T) {
	config := DefaultDocumentsConfig()
	config.Index.Embeddings.Enabled = true
	config.Index.Embeddings.Profile = "other.embeddings"

	require.ErrorContains(t, config.Validate(), "profile must be \"vector.embeddings\"")
}

func TestDocumentsConfigRejectsUnsafePolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*DocumentsConfig)
		want   string
	}{
		{name: "provider", mutate: func(c *DocumentsConfig) { c.Provider = "other" }, want: "provider"},
		{name: "model", mutate: func(c *DocumentsConfig) { c.Model = "latest" }, want: "pinned"},
		{name: "region", mutate: func(c *DocumentsConfig) { c.Region = "us" }, want: "unknown region"},
		{name: "environment", mutate: func(c *DocumentsConfig) { c.APIKeyEnv = "bad-name" }, want: "environment variable"},
		{name: "negative cap", mutate: func(c *DocumentsConfig) { c.MaxFileBytes = -1 }, want: "must be positive"},
		{name: "unbounded cap", mutate: func(c *DocumentsConfig) { c.MaxResponseBytes = mistral.MaxResponseBytes + 1 }, want: "hard safety limit"},
		{name: "spool below file", mutate: func(c *DocumentsConfig) { c.MaxSpoolBytes = c.MaxFileBytes - 1 }, want: "at least max_file_bytes"},
		{name: "spool hard cap", mutate: func(c *DocumentsConfig) { c.MaxSpoolBytes = hardMaxSpoolBytes + 1 }, want: "hard safety limit"},
		{name: "free space hard cap", mutate: func(c *DocumentsConfig) { c.MinFreeSpaceBytes = hardMaxFreeSpaceBytes + 1 }, want: "hard safety limit"},
		{name: "timeout", mutate: func(c *DocumentsConfig) { c.RequestTimeout = mistral.MaxTimeout + time.Second }, want: "hard safety limit"},
		{name: "cost", mutate: func(c *DocumentsConfig) { c.MaxEstimatedCostUSDPerRun = hardMaxEstimatedCostUSD + 1 }, want: "hard safety limit"},
		{name: "pricing pair", mutate: func(c *DocumentsConfig) { c.EstimatedCostUSDPerKUnits = 4 }, want: "pricing assumption requires both"},
		{name: "pricing date", mutate: func(c *DocumentsConfig) { c.EstimatedCostUSDPerKUnits = 4; c.PricingAssumptionOn = "today" }, want: "YYYY-MM-DD"},
		{name: "retention", mutate: func(c *DocumentsConfig) { c.RetentionPosture = "no-retention" }, want: "retention_posture"},
		{name: "training", mutate: func(c *DocumentsConfig) { c.TrainingPosture = "never" }, want: "training_posture"},
		{name: "lexical without text", mutate: func(c *DocumentsConfig) { disabled := false; c.Index.StoreChunkText = &disabled }, want: "must both be true"},
		{name: "stored text without lexical", mutate: func(c *DocumentsConfig) { disabled := false; c.Index.Lexical = &disabled }, want: "must both be true"},
		{name: "unknown embedding profile", mutate: func(c *DocumentsConfig) {
			c.Index.Embeddings.Enabled = true
			c.Index.Embeddings.Profile = "other.embeddings"
		}, want: "profile must be \"vector.embeddings\""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultDocumentsConfig()
			test.mutate(&config)
			require.ErrorContains(t, config.Validate(), test.want)
		})
	}
}

func TestDocumentsConfigReservesWorstCaseRunBudget(t *testing.T) {
	config := DefaultDocumentsConfig()
	config.MaxPagesPerDocument = 500
	config.MaxPagesPerRun = 2_000
	config.MaxEstimatedCostUSDPerRun = 3
	config.EstimatedCostUSDPerKUnits = 4
	config.PricingAssumptionOn = "2026-08-13"

	limit, err := config.MaxDocumentsWithinRunBudget(100)
	require.NoError(t, err)
	assert.Equal(t, 1, limit, "the cost cap reserves two dollars for every worst-case request")
	config.MaxEstimatedCostUSDPerRun = 1
	_, err = config.MaxDocumentsWithinRunBudget(100)
	require.ErrorContains(t, err, "smaller than one")
}

func TestDocumentsProfileFingerprintIsDeterministicAndPolicyBound(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	config := DefaultDocumentsConfig()
	config.RetentionPosture = RetentionZDR
	config.TrainingPosture = TrainingOptedOut
	config.Scope.MessageTypes = []string{"email", "chat"}
	config.ApplyDefaults()
	policy, err := config.MistralPolicy()
	require.NoError(err)
	manifest := testCapabilityManifest(t, policy)

	first, err := config.ProfileFingerprint(manifest, []string{"text/csv", "application/pdf", "text/csv"})
	require.NoError(err)
	second, err := config.ProfileFingerprint(manifest, []string{"application/pdf", "text/csv"})
	require.NoError(err)
	assert.Equal(first, second)
	assert.Regexp(`^[0-9a-f]{64}$`, first)

	changed := config
	changed.RetentionPosture = RetentionStandard
	third, err := changed.ProfileFingerprint(manifest, []string{"application/pdf", "text/csv"})
	require.NoError(err)
	assert.NotEqual(first, third)

	fourth, err := config.ProfileFingerprint(manifest, []string{"application/pdf"})
	require.NoError(err)
	assert.NotEqual(first, fourth)

	changed = config
	changed.MaxSpoolBytes++
	fifth, err := changed.ProfileFingerprint(manifest, []string{"application/pdf", "text/csv"})
	require.NoError(err)
	assert.NotEqual(first, fifth)

	changed = config
	changed.MinFreeSpaceBytes++
	sixth, err := changed.ProfileFingerprint(manifest, []string{"application/pdf", "text/csv"})
	require.NoError(err)
	assert.NotEqual(first, sixth)

	changedManifest := manifest
	changedManifest.Results = append([]mistral.CapabilityResult(nil), manifest.Results...)
	for index := range changedManifest.Results {
		if changedManifest.Results[index].FormatID == "pdf" {
			changedManifest.Results[index].FixtureDigest = strings.Repeat("1", 16)
			break
		}
	}
	seventh, err := config.ProfileFingerprint(changedManifest, []string{"application/pdf", "text/csv"})
	require.NoError(err)
	assert.NotEqual(first, seventh)
}

func TestDocumentsProfileFingerprintExcludesEmbeddingOptIn(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	config := DefaultDocumentsConfig()
	config.RetentionPosture = RetentionZDR
	config.TrainingPosture = TrainingOptedOut
	policy, err := config.MistralPolicy()
	requirements.NoError(err)
	manifest := testCapabilityManifest(t, policy)

	before, err := config.ProfileFingerprint(manifest, []string{"application/pdf"})
	requirements.NoError(err)
	beforePolicy, err := config.ProfilePolicyJSON(manifest, []string{"application/pdf"})
	requirements.NoError(err)

	config.Index.Embeddings.Enabled = true
	config.Index.Embeddings.Profile = "vector.embeddings"
	after, err := config.ProfileFingerprint(manifest, []string{"application/pdf"})
	requirements.NoError(err)
	afterPolicy, err := config.ProfilePolicyJSON(manifest, []string{"application/pdf"})
	requirements.NoError(err)

	assertions.Equal(before, after)
	assertions.Equal(string(beforePolicy), string(afterPolicy))
}

func TestDocumentsProfilePolicyJSONRemainsByteStable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	config := DefaultDocumentsConfig()
	config.RetentionPosture = RetentionZDR
	config.TrainingPosture = TrainingOptedOut
	config.Scope.MessageTypes = []string{"email", "chat", "email"}
	config.ApplyDefaults()
	policy, err := config.MistralPolicy()
	require.NoError(err)
	manifest := testCapabilityManifest(t, policy)

	policyJSON, err := config.ProfilePolicyJSON(manifest, []string{"text/csv", "application/pdf", "text/csv"})
	require.NoError(err)
	expected := `{"version":1,"provider":"mistral","endpoint":"https://api.eu.mistral.ai/v1/ocr","model":"mistral-ocr-4-0","retention":"zdr","training":"opted-out","max_file_bytes":52428800,"max_pages_per_document":500,"max_response_bytes":67108864,"max_normalized_chars":25000000,"max_spool_bytes":536870912,"min_free_space_bytes":1073741824,"request_timeout_nanos":300000000000,"max_retries":3,"max_pages_per_run":10000,"max_estimated_cost_usd_per_run":50,"message_types":["chat","email"],"allowed_media_types":["application/pdf","text/csv"],"document_policy_fingerprint":"2614ee7d1bcac019ca6a7b78147be1954af97ebb22600ce0c22a36384f0813cd","lexical":true,"store_chunk_text":true,"extract_header":true,"extract_footer":true,"normalization_version":2,"max_unit_chars":1000000,"max_source_unit_bytes":4000000,"max_metadata_source_bytes":65536,"max_link_chars":2048,"max_chunk_runes":4000,"chunk_overlap":200,"max_chunks":20000}`
	assert.JSONEq(expected, string(policyJSON))

	fingerprint, err := config.ProfileFingerprint(manifest, []string{"application/pdf", "text/csv"})
	require.NoError(err)
	digest := sha256.Sum256([]byte(expected))
	assert.Equal(hex.EncodeToString(digest[:]), fingerprint)
}

func TestDocumentsConfigResolvesAPIKeyOnlyOnDemand(t *testing.T) {
	config := DefaultDocumentsConfig()
	t.Setenv(config.APIKeyEnv, "synthetic-secret")

	key, err := config.ResolveAPIKey()
	require.NoError(t, err)
	assert.Equal(t, "synthetic-secret", key)

	config.APIKeyEnv = "MISSING_SYNTHETIC_MISTRAL_KEY"
	_, err = config.ResolveAPIKey()
	require.ErrorContains(t, err, config.APIKeyEnv)
}
