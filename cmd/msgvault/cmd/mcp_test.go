package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeMCPHTTPAddr(t *testing.T) {
	t.Run("bare_port_defaults_to_loopback", func(t *testing.T) {
		got, err := normalizeMCPHTTPAddr("8080", false)
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1:8080", got)
	})

	t.Run("colon_port_defaults_to_loopback", func(t *testing.T) {
		got, err := normalizeMCPHTTPAddr(":8080", false)
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1:8080", got)
	})

	t.Run("explicit_loopback_passes", func(t *testing.T) {
		cases := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"}
		for _, c := range cases {
			got, err := normalizeMCPHTTPAddr(c, false)
			require.NoError(t, err, "%s", c)
			assert.Equal(t, c, got, "%s: should be unchanged", c)
		}
	})

	t.Run("non_loopback_rejected_without_optin", func(t *testing.T) {
		cases := []string{
			"0.0.0.0:8080",
			"192.168.1.5:8080",
			"vault.local:8080",
			// Regression: empty-bracket host parses cleanly via
			// net.SplitHostPort but binds to all interfaces. Must
			// be rejected, not silently treated as loopback.
			"[]:8080",
		}
		for _, c := range cases {
			_, err := normalizeMCPHTTPAddr(c, false)
			require.Error(t, err, "%s: expected refusal", c)
			assert.ErrorContains(t, err, "--http-allow-insecure", "%s: expected hint", c)
		}
	})

	t.Run("non_loopback_allowed_with_optin", func(t *testing.T) {
		got, err := normalizeMCPHTTPAddr("0.0.0.0:8080", true)
		require.NoError(t, err)
		require.Equal(t, "0.0.0.0:8080", got)
	})

	t.Run("empty_rejected", func(t *testing.T) {
		_, err := normalizeMCPHTTPAddr("", false)
		require.Error(t, err, "expected error for empty addr")
	})

	t.Run("garbage_rejected", func(t *testing.T) {
		_, err := normalizeMCPHTTPAddr("not-a-port", false)
		require.Error(t, err, "expected error for non-port, non-host:port")
	})
}
