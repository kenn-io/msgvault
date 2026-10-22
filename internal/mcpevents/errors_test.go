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
		{CodeUnavailable, "https://callback-marker.example.net/secret", false},
		{CodeUnavailable, "whsec_synthetic-marker", false},
		{CodeInvalidParams, "tls_error", false},
		{CodeForbidden, "unknown_scope", false},
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

func TestSafeErrorSEPCodes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		oldCode int
		code    int
		reasons []string
	}{
		{"not found", -32602, CodeNotFound, []string{"unknown_event", "subscription_not_found"}},
		{"forbidden", -32012, CodeForbidden, []string{"owner_required", "subscription_unavailable", "principal_revoked", "subscription_inactive"}},
		{"resource exhausted", -32013, CodeResourceExhausted, []string{"subscription_limit"}},
		{"unsupported", -32014, CodeUnsupported, []string{"unsupported_delivery"}},
		{"callback endpoint", -32015, CodeCallbackEndpointError, []string{"connection_refused", "timeout", "tls_error", "http_4xx", "http_5xx", "challenge_failed"}},
		{"conflict", -32013, CodeConflict, []string{"concurrent_update", "events_already_running"}},
		{"unavailable", -32015, CodeUnavailable, []string{"events_storage_unavailable", "events_key_unavailable", "events_unavailable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := require.New(t)
			for _, reason := range tc.reasons {
				got, ok := SafeError(tc.code, reason)
				require.True(ok, reason)
				assert.Equal(&Error{Code: tc.code, Reason: reason}, got)
				if tc.oldCode != tc.code {
					got, ok = SafeError(tc.oldCode, reason)
					assert.False(ok, reason)
					assert.Nil(got, reason)
				}
			}
		})
	}
}

func TestErrorWireData(t *testing.T) {
	for _, tc := range []struct {
		code    int
		reason  string
		data    map[string]any
		message string
	}{
		{CodeResourceExhausted, "subscription_limit", map[string]any{"reason": "subscription_limit", "limit": "subscriptions", "max": MaxActiveSubscriptions}, "ResourceExhausted"},
		{CodeUnsupported, "unsupported_delivery", map[string]any{"reason": "unsupported_delivery", "feature": "deliveryMode"}, "Unsupported"},
		{CodeNotFound, "unknown_event", map[string]any{"reason": "unknown_event", "kind": "event"}, "NotFound"},
		{CodeNotFound, "subscription_not_found", map[string]any{"reason": "subscription_not_found", "kind": "subscription"}, "NotFound"},
		{CodeCallbackEndpointError, "challenge_failed", map[string]any{"reason": "challenge_failed"}, "CallbackEndpointError"},
		{CodeForbidden, "owner_required", map[string]any{"reason": "owner_required"}, "Forbidden"},
		// Task 5 builds a terminated envelope from a reason outside the allowlist.
		{CodeForbidden, "scope_removed", map[string]any{"reason": "scope_removed"}, "Forbidden"},
		{CodeInvalidParams, "invalid_request", map[string]any{"reason": "invalid_request"}, "event request failed"},
		{CodeConflict, "concurrent_update", map[string]any{"reason": "concurrent_update"}, "event request failed"},
		{CodeUnavailable, "events_unavailable", map[string]any{"reason": "events_unavailable"}, "event request failed"},
	} {
		err := &Error{Code: tc.code, Reason: tc.reason}
		Assert.Equal(t, tc.data, err.WireData(), tc.reason)
		Assert.Equal(t, tc.message, err.WireMessage(), tc.reason)
	}
}
