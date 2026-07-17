package cmd

import (
	"testing"

	"go.kenn.io/msgvault/internal/config"
)

// withTestConfig swaps the package-level cfg for the duration of a test
// and restores the previous value on cleanup. Untagged (no build
// constraint) so both the vector-tagged precheck tests and the untagged
// background-init tests (Task 5) can share it.
func withTestConfig(t *testing.T, c *config.Config) {
	t.Helper()
	prev := cfg
	cfg = c
	t.Cleanup(func() { cfg = prev })
}
