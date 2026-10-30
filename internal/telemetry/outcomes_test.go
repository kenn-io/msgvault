package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/telemetry/posthog"
	"go.kenn.io/msgvault/internal/chatwoot"
	"go.kenn.io/msgvault/internal/circleback"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/granola"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/matrix"
	"go.kenn.io/msgvault/internal/msmail"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/omi"
	"go.kenn.io/msgvault/internal/plaud"
	"go.kenn.io/msgvault/internal/synctechsms"
	"go.kenn.io/msgvault/internal/twenty"
	"go.kenn.io/msgvault/internal/twilio"
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

	for _, stop := range []error{context.Canceled, jobctx.ErrYieldedToWaiter, jobctx.ErrRunBudgetExceeded} {
		failures.Record(t.Context(), "gmail", "synthetic", stop)
		assert.Empty(events)
	}
	budgetCtx, cancel := context.WithTimeoutCause(t.Context(), 0, jobctx.ErrRunBudgetExceeded)
	defer cancel()
	failures.Record(budgetCtx, "gmail", "synthetic", context.DeadlineExceeded)
	assert.Empty(events)
	failure := context.DeadlineExceeded
	failures.Record(t.Context(), "gmail", "synthetic", failure)
	failures.Record(budgetCtx, "gmail", "synthetic", nil)
	failures.Record(t.Context(), "gmail", "synthetic", context.Canceled)
	failures.Record(t.Context(), "gmail", "synthetic", failure)
	require.Equal([]map[string]any{{"sync_source": "gmail", "reason": "network"}}, events)
	failures.Record(t.Context(), "imap", "synthetic", failure)
	assert.Len(events, 2, "different source types sharing an identifier have separate outages")
	failures.Record(t.Context(), "gmail", "synthetic", nil)
	failures.Record(t.Context(), "gmail", "synthetic", failure)
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
		{"sync_source": "gmail", "reason": "auth"}, {"sync_source": "gmail", "reason": "auth"},
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
		`{"event":"error_shown","properties":{"surface":"web"}}`,
		`{"event":"error_shown","properties":{"kind":"search"}}`,
		`{"event":"search_empty","properties":{"surface":"web","search_type":"text","private":"synthetic"}}`,
		`{"event":"error_shown","properties":{"surface":"tui","kind":"unlisted"}}`,
		`{"event":"sync_failed","properties":{"sync_source":"gmail","reason":"auth"}}`,
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
		require.NoError(t, reporter.Capture(EventSyncFailed, map[string]any{"sync_source": "gmail", "reason": "auth", "private": "synthetic"}))
	}
}

func TestSyncFailureRegisteredSourceTypes(t *testing.T) {
	enableTelemetryEnv(t)
	stub := newWireStub(t)
	reporter := newReporterOrDisabled(Options{DataDir: t.TempDir(), ConfigEnabled: true}, testAPIKey, stub.server.URL, slog.Default())
	defer func() { require.NoError(t, reporter.Close()) }()
	for _, sourceType := range []string{"gmail", "imap", "teams", "discord", "beeper", "slack", "carddav", "line", circleback.SourceType, gcal.SourceType, granola.SourceType, matrix.SourceType, msmail.SourceType, muesli.SourceType, notionmeetings.SourceType, omi.SourceType, plaud.SourceType, synctechsms.SourceType, twenty.SourceType, twilio.SourceType, chatwoot.SourceType} {
		properties, _ := reporter.SanitizeProperties(EventSyncFailed, map[string]any{"sync_source": sourceType})
		assert.Equal(t, sourceType, properties["sync_source"], sourceType)
	}
}
