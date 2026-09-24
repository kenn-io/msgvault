package daemonclient

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
