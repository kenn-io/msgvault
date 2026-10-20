package telemetry

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"
)

func TestSyncFailureTransitions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var events []map[string]any
	failures := &SyncFailures{Capture: func(event string, props map[string]any) error {
		assert.Equal("sync_failed", event)
		events = append(events, props)
		return nil
	}}
	failures.Record("gmail", "synthetic", context.Canceled)
	assert.Empty(events)
	failure := errors.New("synthetic private detail")
	failures.Record("gmail", "synthetic", failure)
	failures.Record("gmail", "synthetic", context.Canceled)
	failures.Record("gmail", "synthetic", failure)
	require.Equal([]map[string]any{{"sync_source": "gmail", "reason": "other"}}, events)
	failures.Record("imap", "synthetic", failure)
	assert.Len(events, 2, "different source types sharing an identifier have separate outages")
	failures.Record("gmail", "synthetic", nil)
	failures.Record("gmail", "synthetic", failure)
	assert.Len(events, 3)
}

func TestOutcomeWire(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	stub := newWireStub(t)
	runWireHelper(t, stub.server.URL, t.TempDir(), "MSGVAULT_OUTCOMES_TEST=1")
	events := batchEvents(t, stub)
	require.Len(events, 5)
	for i, want := range []map[string]any{
		{"surface": "web", "search_type": "text"},
		{"surface": "tui", "kind": "other"}, {"surface": "tui", "kind": "other"},
		{"sync_source": "other", "reason": "other"}, {"sync_source": "other", "reason": "other"},
	} {
		assert.Equal([]string{"search_empty", "error_shown", "error_shown", "sync_failed", "sync_failed"}[i], events[i]["event"])
		props, ok := events[i]["properties"].(map[string]any)
		require.True(ok)
		for key, value := range want {
			assert.Equal(value, props[key])
		}
		assert.NotContains(props, "private")
	}
}

func exerciseOutcomes(t *testing.T, handler http.Handler, reporter *posthog.Reporter) {
	t.Helper()
	for _, body := range []string{
		`{"event":"search_empty","properties":{"surface":"web","search_type":"text","private":"synthetic"}}`,
		`{"event":"error_shown","properties":{"surface":"tui","kind":"unlisted"}}`,
		`{"event":"sync_failed","properties":{"sync_source":"unlisted","reason":"unlisted"}}`,
	} {
		for range 2 {
			want := http.StatusAccepted
			if strings.Contains(body, "sync_failed") {
				want = http.StatusBadRequest
			}
			require.Equal(t, want, screenRequest(handler, body).Code)
		}
	}
	for range 2 {
		require.NoError(t, reporter.Capture(EventSyncFailed, map[string]any{"sync_source": "unlisted", "reason": "unlisted", "private": "synthetic"}))
	}
}
