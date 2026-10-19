package telemetry

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
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
	require.Len(events, 6)
	for i, want := range []map[string]any{
		{"surface": "web", "search_type": "text"}, {"surface": "web", "search_type": "text"},
		{"surface": "tui", "kind": "other"}, {"surface": "tui", "kind": "other"},
		{"sync_source": "other", "reason": "other"}, {"sync_source": "other", "reason": "other"},
	} {
		assert.Equal([]string{"search_empty", "error_shown", "sync_failed"}[i/2], events[i]["event"])
		props, ok := events[i]["properties"].(map[string]any)
		require.True(ok)
		for key, value := range want {
			assert.Equal(value, props[key])
		}
		assert.NotContains(props, "private")
	}
}

func exerciseOutcomes(t *testing.T, handler http.Handler) {
	t.Helper()
	for _, body := range []string{
		`{"event":"search_empty","properties":{"surface":"web","search_type":"text","private":"synthetic"}}`,
		`{"event":"error_shown","properties":{"surface":"tui","kind":"unlisted"}}`,
		`{"event":"sync_failed","properties":{"sync_source":"unlisted","reason":"unlisted"}}`,
	} {
		for range 2 {
			require.Equal(t, http.StatusAccepted, screenRequest(handler, body).Code)
		}
	}
}

func TestSyncSourceVocabulary(t *testing.T) {
	assert := assert.New(t)
	sources := []string{"gmail", "imap", "msmail", "teams", "discord"}
	for _, tc := range []struct{ job, source string }{
		{"carddav", "carddav"}, {"carddav:synthetic", "carddav"}, {"chatwoot:https://example.com/accounts/1", "chatwoot"},
		{"synctech-sms:synthetic", "synctech_sms"}, {"notion-meetings:synthetic", "notion_meetings"},
		{"gcal:synthetic@example.com", "gcal"}, {"omi:synthetic", "omi"}, {"granola:synthetic", "granola"},
		{"plaud:synthetic", "plaud"}, {"circleback:synthetic", "circleback"}, {"twilio:synthetic", "twilio"},
		{"twenty:synthetic", "twenty"}, {"muesli:synthetic", "muesli"}, {"beeper", "beeper"}, {"matrix", "matrix"}, {"slack", "slack"},
	} {
		source, ok := api.SyncSourceForJob(tc.job)
		require.True(t, ok, tc.job)
		assert.Equal(tc.source, source)
		sources = append(sources, source)
	}
	for _, source := range sources {
		assert.Contains(actionProperties[EventSyncFailed]["sync_source"], source)
	}
	_, ok := api.SyncSourceForJob("embed")
	assert.False(ok)
}
