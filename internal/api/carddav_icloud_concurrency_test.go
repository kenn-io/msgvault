package api

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
)

func TestICloudConcurrentPreflightReturnsExistingSyncActiveError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := savedICloudAccountFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	f.discoverHook = func(string) {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	done := make(chan error, 1)
	go func() { _, err := f.controller.Current().Sync(t.Context(), carddav.SyncOptions{}); done <- err }()
	select {
	case <-entered:
	case err := <-done:
		require.FailNow("sync returned before preflight", "%v", err)
	}
	_, overlapErr := f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	close(release)
	require.NoError(<-done)
	require.ErrorIs(overlapErr, store.ErrCardDAVSyncActive)
	assert.Equal(1, f.syncCalls)
	assert.Equal([]string{"https://p42-contacts.icloud.com"}, f.discovered)
}

func TestICloudNewRetryGateDuringBootstrapPreventsReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := savedICloudAccountFixture(t)
	f.partition, f.partitionErr = "p43", &carddav.StatusError{StatusCode: http.StatusGone}
	f.discoverHook = func(base string) {
		if base == carddav.ICloudDiscoveryURL {
			require.NoError(f.controller.store.SetCardDAVRetryAfterContext(t.Context(), time.Now().Add(time.Hour), store.DefaultCardDAVAccountID))
		}
	}
	_, err := f.controller.Current().Sync(t.Context(), carddav.SyncOptions{})
	require.ErrorIs(err, store.ErrCardDAVRetryAfter)
	assert.Equal("https://p42-contacts.icloud.com", f.controller.cardDAVConfigSnapshot().BaseURL)
	assert.Zero(f.syncCalls)
	gate, err := f.controller.store.GetCardDAVRetryAfterContext(t.Context(), store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.NotNil(gate)
}
