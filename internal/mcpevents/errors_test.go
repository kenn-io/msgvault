package mcpevents

import (
	"testing"

	Assert "github.com/stretchr/testify/assert" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"github.com/stretchr/testify/require"
)

func TestSafeErrorRejectsUntrustedReasonsAndCodes(t *testing.T) {
	assert := Assert.New(t)
	for _, tc := range []struct {
		code   int
		reason string
		ok     bool
	}{
		{-32602, "unknown_scope", true},
		{-32012, "subscription_unavailable", true},
		{-32013, "concurrent_update", true},
		{-32014, "unsupported_delivery", true},
		{-32015, "tls_error", true},
		{-32015, "events_key_unavailable", true},
		{-32015, "events_unavailable", true},
		{-32015, "https://callback-marker.example.net/secret", false},
		{-32015, "whsec_synthetic-marker", false},
		{-32602, "tls_error", false},
		{-32012, "unknown_scope", false},
		{500, "tls_error", false},
	} {
		err, ok := SafeError(tc.code, tc.reason)
		assert.Equal(tc.ok, ok)
		if tc.ok {
			require.NotNil(t, err)
			assert.Equal(tc.code, err.Code)
			assert.Equal(tc.reason, err.Reason)
		} else {
			assert.Nil(err)
		}
	}
}
