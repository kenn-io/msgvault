package omi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientPersistsCooldownBeforeBodyCompletes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cancel  bool
		wantErr error
	}{
		{name: "truncated", wantErr: io.ErrUnexpectedEOF},
		{name: "canceled", cancel: true, wantErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				headersSent := make(chan struct{})
				finishBody := make(chan struct{})
				var requests atomic.Int32
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if requests.Add(1) > 1 {
						_, _ = io.WriteString(w, "[]")
						return
					}
					w.Header().Set("Retry-After", "7200")
					w.Header().Set("Content-Length", "100")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, "throttled")
					assert.NoError(t, http.NewResponseController(w).Flush())
					close(headersSent)
					select {
					case <-finishBody:
					case <-r.Context().Done():
					}
				}))
				client := testOmiClient(t, "http://127.0.0.1", "omi_dev_synthetic")
				client.http.Transport = server.Client().Transport
				result := make(chan error, 1)
				go func() {
					_, err := client.ListConversations(ctx, ListParams{})
					result <- err
				}()
				<-headersSent
				synctest.Wait()
				next := NewPacedClient(client.baseURL, client.apiKey, client.paceDir)
				next.http.Transport = server.Client().Transport
				nextCtx, nextCancel := context.WithTimeout(t.Context(), time.Minute)
				defer nextCancel()
				_, err := next.ListConversations(nextCtx, ListParams{})
				var cooldown *CooldownError
				if assert.ErrorAs(t, err, &cooldown) {
					assert.Greater(t, time.Until(cooldown.Until), time.Hour)
				}
				assert.Equal(t, int32(1), requests.Load(), "the next pass must honor Retry-After")

				if tc.cancel {
					cancel()
				} else {
					close(finishBody)
				}
				require.ErrorIs(t, <-result, tc.wantErr)
			})
		})
	}
}

func TestClientRetainsBodyAndCooldownSaveErrors(t *testing.T) {
	client := testOmiClient(t, "http://127.0.0.1", "omi_dev_synthetic")
	pacePath := client.pacePath(client.baseURL)
	server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !assert.NoError(t, os.Remove(pacePath)) || !assert.NoError(t, os.Mkdir(pacePath, 0o700)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Retry-After", "7200")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "throttled")
	}))
	client.http.Transport = server.Client().Transport
	_, err := client.ListConversations(t.Context(), ListParams{})
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	var pathErr *os.PathError
	require.ErrorAs(t, err, &pathErr)
	assert.Equal(t, pacePath, pathErr.Path)
}
