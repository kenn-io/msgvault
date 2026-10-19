package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calcontrol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/telemetry"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSyncJobPreLoopFailureRecordsRegisteredSourcesOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.OAuth.ClientSecrets = filepath.Join(cfg.HomeDir, "client_secret.json")
	require.NoError(os.WriteFile(cfg.OAuth.ClientSecrets, []byte(fakeClientSecrets), 0600))
	state := testInvocationWithConfig(cfg)
	ctx := withInvocation(t.Context(), state)
	for _, identifier := range []string{"user@example.com/first", "user@example.com/second"} {
		_, err := st.Store.GetOrCreateSource("gcal", identifier)
		require.NoError(err)
	}
	_, err := st.Store.GetOrCreateSource("gcal", "other@example.com/first")
	require.NoError(err)
	var events []map[string]any
	state.syncFailures = &telemetry.SyncFailures{Capture: func(event string, props map[string]any) error {
		assert.Equal("sync_failed", event)
		events = append(events, props)
		return nil
	}}
	results := syncJobResults(st.Store, state)
	for range 2 {
		runCtx, report := results(ctx, "gcal:user@example.com")
		err := runConfiguredGCalSync(runCtx, st.Store, config.GCalSource{Email: "user@example.com"})
		require.ErrorIs(err, calcontrol.ErrDenied)
		report(err)
		assert.Equal([]map[string]any{{"sync_source": "gcal", "reason": "other"}, {"sync_source": "gcal", "reason": "other"}}, events)
	}
	// A covered run clears only the calendar that succeeded, even if the job fails.
	runCtx, report := results(ctx, "gcal:user@example.com")
	recordSyncAttempt(runCtx, "gcal", "user@example.com/first", nil)
	report(errors.New("another calendar failed"))
	runCtx, report = results(ctx, "gcal:user@example.com")
	err = runConfiguredGCalSync(runCtx, st.Store, config.GCalSource{Email: "user@example.com"})
	require.ErrorIs(err, calcontrol.ErrDenied)
	report(err)
	assert.Len(events, 3, "only the recovered calendar starts another outage")
}

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
