//go:build !sqlite_vec && !pgvector

package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

// TestSetupVectorFeatures_EnabledWithoutTag verifies the untagged stub
// returns a descriptive error when the user enables vector search in
// config but builds the binary without -tags sqlite_vec. Runs only
// under the untagged build, which is where this error path exists.
func TestSetupVectorFeatures_EnabledWithoutTag(t *testing.T) {
	prev := cfg
	t.Cleanup(func() { cfg = prev })

	cfg = &config.Config{}
	cfg.Vector.Enabled = true

	vf, err := setupVectorFeatures(context.Background(), nil, "", false)
	require.Error(t, err, "setupVectorFeatures with Enabled=true but no tag")
	assert.Nil(t, vf, "vf should be nil when error is returned")
	msg := err.Error()
	for _, want := range []string{"sqlite_vec", "enabled = false"} {
		assert.Contains(t, msg, want)
	}
}

// TestPrecheckVectorFeatures_Stub verifies the untagged precheck mirrors
// setupVectorFeatures's gate: nil when vector search is disabled, and the
// same build-unsupported error (mentioning sqlite_vec) when enabled.
func TestPrecheckVectorFeatures_Stub(t *testing.T) {
	c := &config.Config{}
	c.Vector.Enabled = false
	withTestConfig(t, c)

	assert.NoError(t, precheckVectorFeatures("/tmp/x.db"), "disabled: precheck should be a no-op")

	c.Vector.Enabled = true
	err := precheckVectorFeatures("/tmp/x.db")
	require.Error(t, err, "enabled without vector build tags")
	assert.Contains(t, err.Error(), "sqlite_vec")
}
