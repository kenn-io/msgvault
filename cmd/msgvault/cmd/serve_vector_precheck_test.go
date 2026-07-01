//go:build sqlite_vec || pgvector

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

// TestPrecheckVectorFeaturesDisabled verifies precheckVectorFeatures is a
// no-op when vector search is disabled, regardless of what else is
// misconfigured.
func TestPrecheckVectorFeaturesDisabled(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = false
	withTestConfig(t, c)

	assert.NoError(t, precheckVectorFeatures("/tmp/msgvault.db"))
}

// TestPrecheckVectorFeaturesRejectsBadCron verifies the precheck validates
// the embed cron expression even when the rest of the vector config is
// otherwise valid.
func TestPrecheckVectorFeaturesRejectsBadCron(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	c.Vector.Embeddings.Endpoint = "http://localhost:11434/v1/embeddings"
	c.Vector.Embeddings.Model = "test-model"
	c.Vector.Embeddings.Dimension = 768
	c.Vector.Embed.Schedule.Cron = "not a cron"
	withTestConfig(t, c)

	err := precheckVectorFeatures("/tmp/msgvault.db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cron")
}

// TestPrecheckVectorFeaturesRejectsInvalidConfig verifies the precheck
// surfaces cfg.Vector.Validate() failures (e.g. missing required
// embeddings fields) without attempting the expensive backend open.
func TestPrecheckVectorFeaturesRejectsInvalidConfig(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	// Leave required embeddings fields (endpoint, model, dimension) empty
	// so Validate() fails.
	withTestConfig(t, c)

	assert.Error(t, precheckVectorFeatures("/tmp/msgvault.db"))
}

// TestPrecheckVectorFeaturesAcceptsValidConfig verifies the precheck
// succeeds for a fully valid, enabled vector config with no cron set.
func TestPrecheckVectorFeaturesAcceptsValidConfig(t *testing.T) {
	c := config.NewDefaultConfig()
	c.Vector.Enabled = true
	c.Vector.Embeddings.Endpoint = "http://localhost:11434/v1/embeddings"
	c.Vector.Embeddings.Model = "test-model"
	c.Vector.Embeddings.Dimension = 768
	withTestConfig(t, c)

	assert.NoError(t, precheckVectorFeatures("/tmp/msgvault.db"))
}
