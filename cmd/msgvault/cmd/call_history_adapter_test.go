package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCallImportRequestsCacheRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.Analytics.AutoBuildCache = true
	adapter := &storeAPIAdapter{store: st, config: cfg}
	requested := make(chan string, 1)
	refresher := newBackgroundCacheRefresher(adapter.invocationContext(t.Context()), func(_ context.Context, id string) error {
		requested <- id
		return nil
	}, nil)
	previous := daemonCacheRefresher
	daemonCacheRefresher = refresher
	t.Cleanup(func() {
		daemonCacheRefresher = previous
		require.NoError(refresher.Shutdown(context.Background()))
	})
	o := callhistory.Observation{
		Version: 1, Provider: "signal", Account: "example",
		Kind: "call", Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined",
		OccurredAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Precision: "instant",
		RecordID: "call-1",
		Raw:      []byte(`{"callId":"call-1"}`),
	}
	source, err := st.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(err)
	_, err = adapter.PutCallObservationContext(t.Context(), source.ID, o)
	require.NoError(err)
	select {
	case label := <-requested:
		assert.Equal(callhistory.SourceType, label)
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("imported call did not request a cache refresh")
	}
}
