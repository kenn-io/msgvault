package cmd

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/metawebhook"
)

type webhookActivityReader struct {
	io.Reader

	probe func()
}

func (r webhookActivityReader) Read(p []byte) (int, error) { r.probe(); return r.Reader.Read(p) }

func TestMetaWebhookActivityUsesSharedIdleTracker(t *testing.T) {
	for _, activity := range []string{"request", "worker"} {
		t.Run(activity, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				cfg := webhookTestConfig(t)
				var shutdown atomic.Bool
				idle := api.NewIdleTracker(time.Nanosecond, func() { shutdown.Store(true) })
				probe := func() {
					// Run always checks current activity before observing cancellation. An
					// expired idle timer fires synchronously if this work is not tracked.
					time.Sleep(time.Nanosecond)
					ctx, cancel := context.WithCancel(t.Context())
					cancel()
					idle.Run(ctx)
				}
				registry := metawebhook.NewRegistry()
				registry.Register("page", "feed", func(_ context.Context, _ string, items []metawebhook.Item) []metawebhook.ItemOutcome {
					probe()
					out := make([]metawebhook.ItemOutcome, len(items))
					for i := range out {
						out[i].Class = metawebhook.Applied
					}
					return out
				})
				receiver, err := startMetaWebhooksWithRegistry(t.Context(), cfg, nil, registry, idle, nil)
				require.NoError(err)
				defer stopWebhook(t, receiver)
				receiver.cancel()
				<-receiver.workerDone
				require.NoError(receiver.server.Close())
				<-receiver.serverDone
				body := webhookBody()
				if activity == "request" {
					request := httptest.NewRequest(http.MethodPost, "/meta/webhook/9", webhookActivityReader{Reader: strings.NewReader(body), probe: probe})
					signature := hmac.New(sha256.New, []byte("synthetic-app-secret"))
					_, err = signature.Write([]byte(body))
					require.NoError(err)
					request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(signature.Sum(nil)))
					response := httptest.NewRecorder()
					receiver.server.Handler.ServeHTTP(response, request)
					assert.Equal(http.StatusOK, response.Code)
				} else {
					_, _, err = receiver.spool.Accept("9", []byte(body))
					require.NoError(err)
					summary, err := receiver.worker.DrainOnce(t.Context())
					require.NoError(err)
					assert.Equal(1, summary.Applied)
				}
				assert.False(shutdown.Load(), "active webhook work must prevent idle shutdown")
				probe()
				assert.True(shutdown.Load(), "the idle tracker must be released after webhook work")
			})
		})
	}
}
