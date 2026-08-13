package documentindex

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	endpoint, err := config.Endpoint()
	require.NoError(t, err)
	assert.Equal("https://api.mistral.ai/v1/ocr", endpoint)
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
		{name: "unbounded cap", mutate: func(c *DocumentsConfig) { c.MaxResponseBytes = hardMaxResponseBytes + 1 }, want: "hard safety limit"},
		{name: "spool below file", mutate: func(c *DocumentsConfig) { c.MaxSpoolBytes = c.MaxFileBytes - 1 }, want: "at least max_file_bytes"},
		{name: "spool hard cap", mutate: func(c *DocumentsConfig) { c.MaxSpoolBytes = hardMaxSpoolBytes + 1 }, want: "hard safety limit"},
		{name: "free space hard cap", mutate: func(c *DocumentsConfig) { c.MinFreeSpaceBytes = hardMaxFreeSpaceBytes + 1 }, want: "hard safety limit"},
		{name: "timeout", mutate: func(c *DocumentsConfig) { c.RequestTimeout = hardMaxRequestTimeout + time.Second }, want: "hard safety limit"},
		{name: "cost", mutate: func(c *DocumentsConfig) { c.MaxEstimatedCostUSDPerRun = hardMaxEstimatedCostUSD + 1 }, want: "hard safety limit"},
		{name: "pricing pair", mutate: func(c *DocumentsConfig) { c.EstimatedCostUSDPerKUnits = 4 }, want: "pricing assumption requires both"},
		{name: "pricing date", mutate: func(c *DocumentsConfig) { c.EstimatedCostUSDPerKUnits = 4; c.PricingAssumptionOn = "today" }, want: "YYYY-MM-DD"},
		{name: "schedule manifest", mutate: func(c *DocumentsConfig) {
			c.Schedule = "0 * * * *"
			c.EstimatedCostUSDPerKUnits = 4
			c.PricingAssumptionOn = "2026-08-13"
		}, want: "requires capability_manifest"},
		{name: "schedule pricing", mutate: func(c *DocumentsConfig) {
			c.Schedule = "0 * * * *"
			c.CapabilityManifest = "/private/capabilities.json"
		}, want: "pricing assumption"},
		{name: "retention", mutate: func(c *DocumentsConfig) { c.RetentionPosture = "no-retention" }, want: "retention_posture"},
		{name: "training", mutate: func(c *DocumentsConfig) { c.TrainingPosture = "never" }, want: "training_posture"},
		{name: "lexical without text", mutate: func(c *DocumentsConfig) { disabled := false; c.Index.StoreChunkText = &disabled }, want: "must both be true"},
		{name: "stored text without lexical", mutate: func(c *DocumentsConfig) { disabled := false; c.Index.Lexical = &disabled }, want: "must both be true"},
		{name: "premature vectors", mutate: func(c *DocumentsConfig) { c.Index.Embeddings.Enabled = true }, want: "not available"},
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
	config.Scope.MessageTypes = []string{"email", "chat"}
	config.ApplyDefaults()

	first, err := config.ProfileFingerprint([]string{"text/csv", "application/pdf", "text/csv"})
	require.NoError(err)
	second, err := config.ProfileFingerprint([]string{"application/pdf", "text/csv"})
	require.NoError(err)
	assert.Equal(first, second)
	assert.Regexp(`^[0-9a-f]{64}$`, first)

	changed := config
	changed.RetentionPosture = RetentionZDR
	third, err := changed.ProfileFingerprint([]string{"application/pdf", "text/csv"})
	require.NoError(err)
	assert.NotEqual(first, third)

	fourth, err := config.ProfileFingerprint([]string{"application/pdf"})
	require.NoError(err)
	assert.NotEqual(first, fourth)

	changed = config
	changed.MaxSpoolBytes++
	fifth, err := changed.ProfileFingerprint([]string{"application/pdf", "text/csv"})
	require.NoError(err)
	assert.NotEqual(first, fifth)

	changed = config
	changed.MinFreeSpaceBytes++
	sixth, err := changed.ProfileFingerprint([]string{"application/pdf", "text/csv"})
	require.NoError(err)
	assert.NotEqual(first, sixth)
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
