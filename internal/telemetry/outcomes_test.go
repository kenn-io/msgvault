package telemetry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"
)

func TestSourceOutagesSurviveRestartAndRecoverIndependently(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := t.TempDir()
	var reasons []any
	capture := func(event string, properties map[string]any) error {
		assert.Equal("sync_failed", event)
		assert.Len(properties, 1)
		reasons = append(reasons, properties["reason"])
		return nil
	}
	tracker := NewOutcomes(dir, capture)
	require.NoError(tracker.SyncResult("account:alpha@example.com", errors.New("private")))
	require.NoError(tracker.SyncResult("account:alpha@example.com", errors.New("retry")))
	tracker = NewOutcomes(dir, capture)
	require.NoError(tracker.SyncResult("account:alpha@example.com", errors.New("restart")))
	require.NoError(tracker.SyncResult("account:beta@example.com", errors.New("private")))
	require.NoError(tracker.SyncResult("account:alpha@example.com", nil))
	require.NoError(tracker.SyncResult("account:beta@example.com", errors.New("retry")))
	require.NoError(tracker.SyncResult("account:alpha@example.com", errors.New("new outage")))
	require.NoError(tracker.SyncResult("account:gamma@example.com", context.Canceled))
	assert.Equal([]any{"other", "other", "other"}, reasons)
	data, err := os.ReadFile(filepath.Join(dir, "telemetry-outages.json"))
	require.NoError(err)
	assert.NotContains(string(data), "example.com")
	require.NoError(tracker.SyncResult("", nil))
	require.NoError(tracker.SyncResult("account:beta@example.com", errors.New("new outage")))
	assert.Len(reasons, 4)
}

func TestOutageReasonsUseTypedFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		reason string
	}{
		{"authorization", &googleapi.Error{Code: 401}, "auth"},
		{"missing", &googleapi.Error{Code: 404}, "not_found"},
		{"quota", &googleapi.Error{Code: 429}, "rate_limited"},
		{"provider", &googleapi.Error{Code: 503}, "provider"},
		{"timeout", context.DeadlineExceeded, "network"},
		{"unknown", errors.New("private unclassified failure"), "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var properties map[string]any
			tracker := NewOutcomes(t.TempDir(), func(_ string, captured map[string]any) error { properties = captured; return nil })
			require.NoError(t, tracker.SyncResult("source", test.err))
			assert.Equal(t, map[string]any{"reason": test.reason}, properties)
		})
	}
}

func TestOutageCaptureFailureLeavesClaimAvailable(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	calls := 0
	tracker := NewOutcomes(t.TempDir(), func(string, map[string]any) error {
		calls++
		if calls == 1 {
			return errors.New("queue full")
		}
		return nil
	})
	require.Error(tracker.SyncResult("source", errors.New("provider")))
	require.NoError(tracker.SyncResult("source", errors.New("retry")))
	require.NoError(tracker.SyncResult("source", errors.New("retry")))
	assert.Equal(2, calls)
}
