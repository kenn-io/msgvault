package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/telemetry"
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
