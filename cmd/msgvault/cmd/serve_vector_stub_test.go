//go:build !sqlite_vec

package cmd

import (
	"context"
	"strings"
	"testing"

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
	if err == nil {
		t.Fatal("setupVectorFeatures with Enabled=true but no tag = nil error, want descriptive error")
	}
	if vf != nil {
		t.Errorf("vf = %+v, want nil when error is returned", vf)
	}
	msg := err.Error()
	for _, want := range []string{"sqlite_vec", "enabled = false"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want to contain %q", msg, want)
		}
	}
}
