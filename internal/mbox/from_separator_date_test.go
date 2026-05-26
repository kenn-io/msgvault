package mbox

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseFromSeparatorDateStrict_ParsesKnownTZAbbrev(t *testing.T) {
	line := "From a@b Mon Jan 1 00:00:00 PST 2024"
	ts, ok := ParseFromSeparatorDateStrict(line)
	require.True(t, ok, "expected ok")
	require.Equal(t, "2024-01-01T08:00:00Z", ts.UTC().Format(time.RFC3339))
}

func TestParseFromSeparatorDateStrict_ParsesKnownTZAbbrevAfterYear(t *testing.T) {
	line := "From a@b Mon Jan 1 00:00:00 2024 PST"
	ts, ok := ParseFromSeparatorDateStrict(line)
	require.True(t, ok, "expected ok")
	require.Equal(t, "2024-01-01T08:00:00Z", ts.UTC().Format(time.RFC3339))
}

func TestParseFromSeparatorDateStrict_ParsesNumericOffset(t *testing.T) {
	line := "From a@b Mon Jan 1 00:00:00 -0700 2024"
	ts, ok := ParseFromSeparatorDateStrict(line)
	require.True(t, ok, "expected ok")
	require.Equal(t, "2024-01-01T07:00:00Z", ts.UTC().Format(time.RFC3339))
}

func TestParseFromSeparatorDateStrict_RejectsUnknownTZAbbrev(t *testing.T) {
	line := "From a@b Mon Jan 1 00:00:00 FOO 2024"
	_, ok := ParseFromSeparatorDateStrict(line)
	require.False(t, ok, "expected not ok")
	lineAfterYear := "From a@b Mon Jan 1 00:00:00 2024 FOO"
	_, ok = ParseFromSeparatorDateStrict(lineAfterYear)
	require.False(t, ok, "expected not ok")
	_, ok = ParseFromSeparatorDate(line)
	require.True(t, ok, "expected permissive ParseFromSeparatorDate to accept line for separator detection")
}
