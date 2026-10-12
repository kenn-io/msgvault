package telemetry

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDurationBucket(t *testing.T) {
	for _, tc := range []struct {
		duration time.Duration
		bucket   string
	}{
		{0, "under_1m"}, {time.Minute - time.Nanosecond, "under_1m"},
		{time.Minute, "1_to_5m"}, {5*time.Minute - time.Nanosecond, "1_to_5m"},
		{5 * time.Minute, "5_to_30m"}, {30 * time.Minute, "5_to_30m"},
		{30*time.Minute + time.Nanosecond, "over_30m"},
	} {
		assert.Equal(t, tc.bucket, DurationBucket(tc.duration))
	}
}

func TestSessionEndedWire(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	stub := newWireStub(t)
	runWireHelper(t, stub.server.URL, t.TempDir(), "MSGVAULT_SESSION_TEST=1")
	messages := batchEvents(t, stub)
	require.Len(messages, 3)
	for i, expected := range []map[string]any{
		{"surface": "web", "duration_bucket": "1_to_5m"},
		{"surface": "tui", "duration_bucket": "5_to_30m"}, {},
	} {
		assert.Equal(EventSessionEnded, messages[i]["event"])
		props, ok := messages[i]["properties"].(map[string]any)
		require.True(ok)
		for _, key := range []string{"surface", "duration_bucket"} {
			assert.Equal(expected[key], props[key])
		}
	}
}
