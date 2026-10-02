package daemonclient

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSafeMCPErrorPreservesIdentityReviewCodes(t *testing.T) {
	for _, code := range []string{
		"identity_match_state_unavailable", "identity_match_not_found", "identity_match_review_stale",
		"identity_match_not_acceptable", "identity_match_already_accepted", "identity_match_already_applied",
		"identity_match_state_changed", "identity_match_endpoint_unsupported", "identity_match_failed",
		"person_binding_conflict",
	} {
		t.Run(code, func(t *testing.T) {
			err := SafeMCPError(&APIError{Status: 409, Code: code, Message: "Synthetic daemon failure detail"})
			require.EqualError(t, err, fmt.Sprintf("daemon request failed (409, %s)", code))
		})
	}
}

func TestSafeMCPErrorIncludesInvalidLimitCode(t *testing.T) {
	err := SafeMCPError(&APIError{
		Status:  400,
		Code:    "invalid_limit",
		Message: "Limit exceeds configured batch size",
	})

	require.Error(t, err)
	require.EqualError(t, err, "daemon request failed (400, invalid_limit)")
}

func TestSafeMCPErrorHidesUnknownDaemonCodeAndMessage(t *testing.T) {
	err := SafeMCPError(&APIError{
		Status:  400,
		Code:    "private_detail",
		Message: "private contact data",
	})

	require.Error(t, err)
	require.EqualError(t, err, "daemon request failed (400)")
}
