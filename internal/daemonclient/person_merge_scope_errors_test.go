package daemonclient

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSafeMCPErrorPreservesScopedPersonMergeCodesWithoutProse(t *testing.T) {
	for _, tc := range []struct {
		code   string
		status int
	}{
		{"person_scope_denied", 403},
		{"person_scope_unavailable", 501},
		{"person_scope_too_large", 413},
		{"person_merge_scope_unsupported", 501},
		{"person_merge_lineage_conflict", 409},
	} {
		t.Run(tc.code, func(t *testing.T) {
			err := SafeMCPError(&APIError{Status: tc.status, Code: tc.code, Message: "Synthetic private contact failure detail"})
			require.Error(t, err)
			assert.Equal(t, fmt.Sprintf("daemon request failed (%d, %s)", tc.status, tc.code), err.Error())
			assert.NotContains(t, err.Error(), "Synthetic private contact failure detail")
		})
	}
}
