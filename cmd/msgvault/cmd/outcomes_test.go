package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/telemetry"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestScheduledSourceOutcomesCountEachType(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	state := invocationFromContext(ctx)
	var events []map[string]any
	state.syncFailures = &telemetry.SyncFailures{Capture: func(event string, props map[string]any) error {
		assert.Equal("sync_failed", event)
		events = append(events, props)
		return nil
	}}
	_, err := st.Store.GetOrCreateSource("gmail", "synthetic@example.com")
	require.NoError(err)
	_, err = st.Store.GetOrCreateSource("imap", "synthetic@example.com")
	require.NoError(err)
	oauthFailure := func(context.Context, string) (*oauth.Manager, error) {
		return nil, errors.New("synthetic OAuth failure")
	}
	require.Error(runScheduledSync(ctx, "synthetic@example.com", st.Store, oauthFailure, state))
	assert.Equal([]map[string]any{{"sync_source": "gmail", "reason": "other"}, {"sync_source": "imap", "reason": "other"}}, events)
}

func TestMeetingSetupFailuresAreRecorded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t)
	ctx := testInvocationContext(t.Context(), config.NewDefaultConfig(), invocationOptions{})
	state := invocationFromContext(ctx)
	var sources []any
	state.syncFailures = &telemetry.SyncFailures{Capture: func(_ string, props map[string]any) error {
		sources = append(sources, props["sync_source"])
		return nil
	}}
	for _, sourceType := range []string{"granola", "notion_meetings"} {
		_, err := st.Store.GetOrCreateSource(sourceType, "work")
		require.NoError(err)
	}
	require.ErrorContains(runConfiguredGranolaSync(ctx, st.Store, config.GranolaSource{Identifier: "work"}), "has no api_key")
	require.ErrorContains(runConfiguredNotionMeetingsSync(ctx, st.Store, config.NotionMeetingsSource{Identifier: "work"}), "has no token")
	assert.Equal([]any{"granola", "notion_meetings"}, sources)
}

func TestManualSyncUpdatesDaemonOutage(t *testing.T) {
	for _, command := range []string{"sync", "sync-granola", "sync-notion-meetings", "sync-calendar", "sync-synctech-sms", "sync-calendar-all", "sync-synctech-sms-all"} {
		t.Run(command, func(t *testing.T) {
			selector := "work"
			if single, all := strings.CutSuffix(command, "-all"); all {
				command, selector = single, ""
			}
			generic := command != "sync"
			st := testutil.NewTestStore(t)
			cfg := lifecycleTestConfig(t.TempDir())
			state := testInvocationWithConfig(cfg)
			events := 0
			state.syncFailures = &telemetry.SyncFailures{Capture: func(string, map[string]any) error { events++; return nil }}
			ctx := withInvocation(t.Context(), state)
			sourceType := "gmail"
			if generic {
				sourceType = strings.ReplaceAll(strings.TrimPrefix(command, "sync-"), "-", "_")
			}
			identifier, outageKey := "work", "work"
			switch command {
			case "sync-calendar":
				sourceType, identifier, outageKey = "gcal", "owner@example.com/primary", "owner@example.com"
				cfg.GCal = []config.GCalSource{{Name: "work", Email: outageKey}}
			case "sync-synctech-sms":
				identifier, outageKey = "+15550000000", "+15550000000"
				cfg.SynctechSMS.Sources = []config.SynctechSMSSource{{Name: "work", OwnerPhone: identifier}}
			}
			_, err := st.GetOrCreateSource(sourceType, identifier)
			require.NoError(t, err)
			adapter := &storeAPIAdapter{store: st, config: cfg, syncFailures: state.syncFailures}
			failure := errors.New("synthetic provider failure")
			state.syncFailures.Record(sourceType, outageKey, failure)
			outcome := failure
			// The daemon owns the child-process result even when setup failed before a sync run existed.
			run := func(context.Context) error { return outcome }
			for i, err := range []error{failure, nil, failure} {
				outcome = err
				var got error
				if generic {
					args := []string{command}
					if selector != "" {
						args = append(args, selector)
					}
					got = adapter.runCLICommandWithRunner(ctx, api.CLIRunRequest{Args: args}, nil, func(ctx context.Context, _ []string, _ map[string]string, _ string, _ func(string, string) error) error {
						return run(ctx)
					})
				} else {
					got = adapter.runCLISyncWithRunner(ctx, api.CLISyncRequest{Email: "work"}, nil, func(ctx context.Context, _ []string, _ func(string, string) error) error { return run(ctx) })
				}
				require.ErrorIs(t, got, err)
				assert.Equal(t, []int{1, 1, 2}[i], events)
			}
		})
	}
}

func TestManualOmiPausePreservesOutage(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("omi", "work")
	require.NoError(t, err)
	events := 0
	failures := &telemetry.SyncFailures{Capture: func(string, map[string]any) error { events++; return nil }}
	failure := errors.New("synthetic provider failure")
	failures.Record("omi", "work", failure)
	adapter := &storeAPIAdapter{store: st, syncFailures: failures}
	require.NoError(t, adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: []string{"sync-omi", "work"}}, nil, func(_ context.Context, _ []string, _ map[string]string, _ string, emit func(string, string) error) error {
		return emit("outcome", "paused")
	}))
	failures.Record("omi", "work", failure)
	assert.Equal(t, 1, events)
}
