package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/apiprotocol"
)

func TestNewBackupFreezerUsesCommandContextAndCLIMode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var marker atomic.Value
	requestCanceled := make(chan struct{})
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/backup/freeze/begin", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/backup/freeze/begin", r.URL.Path)
		marker.Store(r.Header.Get(apiprotocol.ClientClassHeader))
		<-r.Context().Done()
		close(requestCanceled)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	rt := daemonRuntimeForHTTPServer(t, srv, daemonAPIKeyFingerprint(""))
	_, err := daemonRuntimeStore(dataDir).Write(rt.Record)
	require.NoError(err, "write daemon runtime")

	ctx, cancel := context.WithCancel(context.Background())
	freezer, closeFreezer, err := newBackupFreezer(ctx)
	require.NoError(err, "newBackupFreezer")
	t.Cleanup(closeFreezer)

	done := make(chan error, 1)
	go func() {
		done <- freezer.Begin(ctx)
	}()
	require.Eventually(func() bool {
		return marker.Load() != nil
	}, 2*time.Second, 10*time.Millisecond, "freeze request starts")
	cancel()

	require.Eventually(func() bool {
		select {
		case <-requestCanceled:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(apiprotocol.ClientClassCLI, marker.Load())
	require.Error(<-done, "freeze request canceled")
}
