package daemonclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/hex"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
)

var recorderContractCases = []struct {
	name, path string
	send       func(context.Context, *Client) (int64, error)
}{
	{"Hex", "/api/v1/import/hex", func(ctx context.Context, c *Client) (int64, error) {
		r, err := c.ImportHex(ctx, hex.RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}})
		return r.SourceID, err
	}},
	{"Muesli", "/api/v1/import/muesli", func(ctx context.Context, c *Client) (int64, error) {
		r, err := c.ImportMuesli(ctx, muesli.RemoteRequest{Action: "register", Source: meetingimport.Source{Identifier: "recorder", AccountEmail: "user@example.com"}})
		return r.SourceID, err
	}},
}

func TestRecorderImportRetriesImmutableAuthenticatedBody(t *testing.T) {
	for _, tc := range recorderContractCases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var bodies []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("test-owner-key", r.Header.Get("X-Api-Key"))
				assert.Equal(tc.path, r.URL.Path)
				body, err := io.ReadAll(r.Body)
				if !assert.NoError(err) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				bodies = append(bodies, string(body))
				w.Header().Set("Content-Type", "application/json")
				if len(bodies) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":"operation_in_progress","message":"import busy"}`))
					return
				}
				_, _ = w.Write([]byte(`{"status":"registered","source_id":1,"changed":false}`))
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, APIKey: "test-owner-key"})
			require.NoError(err)
			client.httpClient = server.Client()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sourceID, err := tc.send(ctx, client)
			require.NoError(err)
			assert.Equal(int64(1), sourceID)
			require.Len(bodies, 2)
			assert.Equal(bodies[0], bodies[1])
		})
	}
}

func TestRecorderImportWaitsOutRateLimit(t *testing.T) {
	for _, tc := range recorderContractCases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var attempts []time.Time
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts = append(attempts, time.Now())
				w.Header().Set("Content-Type", "application/json")
				if len(attempts) == 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":"rate_limit_exceeded","message":"Too many requests. Please slow down."}`))
					return
				}
				_, _ = w.Write([]byte(`{"status":"registered","source_id":1,"changed":false}`))
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, APIKey: "test-owner-key"})
			require.NoError(err)
			client.httpClient = server.Client()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sourceID, err := tc.send(ctx, client)
			require.NoError(err, "a rate-limited upload resumes instead of ending the scan")
			assert.Equal(int64(1), sourceID)
			require.Len(attempts, 2)
			assert.GreaterOrEqual(attempts[1].Sub(attempts[0]), 900*time.Millisecond, "the retry honors Retry-After")
		})
	}
}

func TestRecorderImportRateLimitStopsWhenCanceled(t *testing.T) {
	for _, tc := range recorderContractCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				require := require.New(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				attempts := 0
				server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					attempts++
					w.Header().Set("Retry-After", "3600")
					w.WriteHeader(http.StatusTooManyRequests)
				}))
				client, err := New(Config{URL: "http://127.0.0.1", AllowInsecure: true, APIKey: "test-owner-key", HTTPClient: server.Client()})
				require.NoError(err)
				done := make(chan error, 1)
				go func() {
					_, err := tc.send(ctx, client)
					done <- err
				}()
				// In-memory I/O finishes before Wait observes the goroutine blocked on Retry-After.
				synctest.Wait()
				require.Equal(1, attempts)
				require.Empty(done)
				cancel()
				synctest.Wait()
				require.Len(done, 1, "cancellation interrupts Retry-After before its timer fires")
				require.ErrorIs(<-done, context.Canceled)
				assert.Equal(t, 1, attempts, "cancellation prevents another request")
			})
		})
	}
}
