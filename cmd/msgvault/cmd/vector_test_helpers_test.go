package cmd

import (
	"context"
	"testing"

	"go.kenn.io/msgvault/internal/config"
)

// withTestConfig binds a configuration to the test invocation context.
func withTestConfig(t *testing.T, c *config.Config) context.Context {
	t.Helper()
	return testInvocationContext(t.Context(), c, invocationOptions{})
}
