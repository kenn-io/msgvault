//go:build !sqlite_vec

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

	vf, err := setupVectorFeatures(context.Background(), nil, "")
	require.Error(t, err, "setupVectorFeatures with Enabled=true but no tag")
	assert.Nil(t, vf, "vf should be nil when error is returned")
	msg := err.Error()
	for _, want := range []string{"sqlite_vec", "enabled = false"} {
		assert.Contains(t, msg, want)
	}
}
