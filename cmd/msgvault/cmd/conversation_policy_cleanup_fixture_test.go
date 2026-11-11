package cmd

import (
	"context"
	"testing"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// policyCleanupCommandFixture gives cleanup tests an archive with a default
// room and an invocation whose data directory stays inside the test.
func policyCleanupCommandFixture(t *testing.T) (*storetest.Fixture, context.Context) {
	t.Helper()
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	fixture := storetest.New(t)
	return fixture, testInvocationContext(t.Context(), cfg, invocationOptions{})
}
