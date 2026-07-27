package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/pkg/client/generated"
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

func TestNewBackupFreezerEndsFreezeAfterCommandCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var beginMarker atomic.Value
	var endMarker atomic.Value

	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: Version,
	}))
	mux.HandleFunc("/api/v1/backup/freeze/begin", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		beginMarker.Store(r.Header.Get(apiprotocol.ClientClassHeader))
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.NewEncoder(w).Encode(generated.BackupFreezeBeginResponse{
			Token: "freeze-token",
		}))
	})
	mux.HandleFunc("/api/v1/backup/freeze/end", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		endMarker.Store(r.Header.Get(apiprotocol.ClientClassHeader))
		var request generated.BackupFreezeEndRequest
		assert.NoError(json.NewDecoder(r.Body).Decode(&request))
		assert.Equal("freeze-token", request.Token)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{}`))
		assert.NoError(err)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	rt := daemonRuntimeForHTTPServer(t, srv, daemonAPIKeyFingerprint(""))
	_, err := daemonRuntimeStore(dataDir).Write(rt.Record)
	require.NoError(err, "write daemon runtime")

	commandCtx, cancelCommand := context.WithCancel(context.Background())
	freezer, closeFreezer, err := newBackupFreezer(commandCtx)
	require.NoError(err, "newBackupFreezer")
	t.Cleanup(closeFreezer)

	require.NoError(freezer.Begin(commandCtx), "begin freeze")
	cancelCommand()

	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelCleanup()
	require.NoError(freezer.End(cleanupCtx), "end freeze after command cancellation")
	assert.Equal(apiprotocol.ClientClassCLI, beginMarker.Load())
	assert.Equal(apiprotocol.ClientClassCLI, endMarker.Load())
}
