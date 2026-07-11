package cmd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatShowingResults(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "Showing 0 results"},
		{1, "Showing 1 result"},
		{2, "Showing 2 results"},
		{100, "Showing 100 results"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, formatShowingResults(tt.n), "formatShowingResults(%d)", tt.n)
	}
}

func TestParseCommonFlagsRejectsNonPositiveLimit(t *testing.T) {
	saved := aggLimit
	t.Cleanup(func() { aggLimit = saved })

	for _, n := range []int{0, -1, -100} {
		aggLimit = n
		_, err := parseCommonFlags()
		require.Error(t, err, "limit %d should be rejected", n)
		assert.Contains(t, err.Error(), "limit must be a positive integer", "error text for %d", n)
	}
}

func TestParseCommonFlagsUsesFlagLimit(t *testing.T) {
	savedLimit := aggLimit
	savedAfter := aggAfter
	savedBefore := aggBefore
	t.Cleanup(func() {
		aggLimit = savedLimit
		aggAfter = savedAfter
		aggBefore = savedBefore
	})
	aggLimit = 25
	aggAfter = ""
	aggBefore = ""

	opts, err := parseCommonFlags()
	require.NoError(t, err, "parseCommonFlags")
	assert.Equal(t, 25, opts.Limit, "opts.Limit should track the flag")
}

// JSON mode must emit valid empty JSON ([]) for zero results, never
// prose like "No senders found." — agents pipe --json output to jq.
func TestOutputAggregateJSON_EmptyEmitsEmptyArray(t *testing.T) {
	done := captureStdout(t)
	require.NoError(t, outputAggregateJSON(nil))
	out := done()

	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &rows),
		"empty aggregate --json output must be valid JSON, got: %q", out)
	assert.Empty(t, rows)
}

func TestOutputAccountStats_JSONEmptyEmitsEmptyArray(t *testing.T) {
	savedJSON := listAccountsJSON
	listAccountsJSON = true
	defer func() { listAccountsJSON = savedJSON }()

	done := captureStdout(t)
	require.NoError(t, outputAccountStats(nil))
	out := done()

	var entries []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &entries),
		"empty list-accounts --json output must be valid JSON, got: %q", out)
	assert.Empty(t, entries)
}
