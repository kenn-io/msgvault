package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMuesliRemoteUnchangedPreservesDeferredCacheRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Analytics.AutoBuildCache = true
	adapter := &storeAPIAdapter{store: st, config: cfg}
	requested := make(chan string, 2)
	var refresher *backgroundCacheRefresher
	refresher = newBackgroundCacheRefresher(adapter.invocationContext(t.Context()), func(_ context.Context, id string) error {
		refresher.RequestAfter(time.Hour, id)
		requested <- id
		return nil
	}, nil)
	previous := daemonCacheRefresher
	daemonCacheRefresher = refresher
	t.Cleanup(func() {
		daemonCacheRefresher = previous
		require.NoError(refresher.Shutdown(context.WithoutCancel(t.Context())))
	})
	source := meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}
	_, err := adapter.ImportMuesli(t.Context(), muesli.RemoteRequest{Action: "register", Source: source})
	require.NoError(err)
	request := muesli.RemoteRequest{Action: "upsert", Source: source, Meeting: muesli.NewRemoteMeeting(muesli.Meeting{ID: 42, Title: "Planning", Status: "completed", CreatedAt: "2026-09-01 14:00:03", StartTime: "2026-09-01T14:00:00Z", RawTranscript: "Synthetic transcript"})}
	_, err = adapter.ImportMuesli(t.Context(), request)
	require.NoError(err)
	select {
	case <-requested:
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("changed meeting did not request a background refresh")
	}
	refresher.mu.Lock()
	delayed := refresher.delayed
	refresher.mu.Unlock()
	require.NotNil(delayed)
	unchanged, err := adapter.ImportMuesli(t.Context(), request)
	require.NoError(err)
	assert.False(unchanged.Changed)
	refresher.mu.Lock()
	remaining := refresher.delayed
	refresher.mu.Unlock()
	assert.Same(delayed, remaining, "unchanged scan must leave the scheduled retry intact")
}
