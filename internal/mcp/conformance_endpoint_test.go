package mcp

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPConformanceEndpoint(t *testing.T) {
	if os.Getenv("MCP_CONFORMANCE_SERVER") != "1" {
		t.Skip("set MCP_CONFORMANCE_SERVER=1 to run the conformance endpoint")
	}

	addr := strings.TrimSpace(os.Getenv("MCP_CONFORMANCE_ADDR"))
	require.NotEmpty(t, addr, "MCP_CONFORMANCE_ADDR is required")
	fixture := newTask5Fixture(t, "000")
	err := ServeHTTPWithOptions(t.Context(), fixture.opts, HTTPOptions{Addr: addr})
	if !errors.Is(err, context.Canceled) {
		require.NoError(t, err)
	}
}
