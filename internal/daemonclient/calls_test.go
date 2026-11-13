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
	"go.kenn.io/msgvault/internal/store"
)

func TestCallClientImportPayloadAndQueryEncoding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var captured []byte
	var readErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/import/calls":
			assertions.Equal(http.MethodPost, r.Method)
			captured, readErr = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"message_id":42,"source_id":7}`))
		case "/api/v1/calls", "/api/v1/calls/summary":
			assertions.Equal(http.MethodGet, r.Method)
			query := r.URL.Query()
			want := map[string][]string{
				"person_id": {"23"}, "limit": {"3"},
				"after": {"2026-10-01T00:00:00Z"}, "before": {"2026-11-01T00:00:00Z"},
			}
			if r.URL.Path == "/api/v1/calls" {
				want["after_id"] = []string{"42"}
				_, _ = w.Write([]byte(`{"items":[{"message_id":42,"record_id":"one"}]}`))
			} else {
				want["offset"] = []string{"2"}
				_, _ = w.Write([]byte(`{"items":[{"person_id":23,"answered":2,"example_message_id":42}]}`))
			}
			assertions.Equal(want, map[string][]string(query))
		default:
			assertions.Fail("unexpected request path", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, APIKey: "example-key", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	const payload = ` {"format":"normalized", "unexpected_field":true,"account":"a","record":{"version":1,"provider":"signal","account":"a","record_id":"7","direction":"incoming","outcome":"answered","owner_participation":"joined","occurred_at":"2026-10-01T09:00:00Z","raw":{"sequence":9007199254740993}}} `
	result, err := client.ImportCallObservation(t.Context(), []byte(payload))
	requirements.NoError(err)
	requirements.NoError(readErr)
	assertions.Equal([]byte(payload), captured) //nolint:testifylint // Preserve the exact request bytes, including whitespace and unknown fields.
	assertions.Equal(int64(42), result.MessageID)

	after := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	before := after.AddDate(0, 1, 0)
	query := store.CallQuery{PersonID: 23, Limit: 3, AfterID: 42, After: &after, Before: &before}
	list, err := client.ListCallObservations(t.Context(), query)
	requirements.NoError(err)
	requirements.NotNil(list)
	requirements.Len(list.Items, 1)
	assertions.Equal(int64(42), list.Items[0].MessageID)
	query.AfterID, query.Offset = 0, 2
	summary, err := client.SummarizeCalls(t.Context(), query)
	requirements.NoError(err)
	requirements.NotNil(summary)
	requirements.Len(summary.Items, 1)
	assertions.Equal(int64(23), summary.Items[0].PersonID)
	assertions.Equal(int64(2), summary.Items[0].Answered)
}

const callObservationImportBody = `{"format":"normalized","account":"example/account","record":{"version":1,"provider":"zoom_phone","account":"example/account","direction":"incoming","outcome":"answered","owner_participation":"joined","occurred_at":"2024-05-03T12:00:00Z","duration_seconds":null,"record_id":"call-record-1","participants":[],"raw":{"call_id":"native-call-1"}}}`

func TestImportCallObservationRetriesBusyAndRateLimitedResponses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		var attempts []time.Time
		var bodies []string
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts = append(attempts, time.Now())
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			bodies = append(bodies, string(body))
			w.Header().Set("Content-Type", "application/json")
			switch len(attempts) {
			case 1:
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"operation_in_progress","message":"import busy"}`))
			case 2:
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate_limit_exceeded","message":"too many requests"}`))
			default:
				_, _ = w.Write([]byte(`{"message_id":7,"source_id":3}`))
			}
		}))
		client, err := New(Config{
			URL: "http://127.0.0.1", AllowInsecure: true,
			APIKey: "test-owner-key", HTTPClient: server.Client(),
		})
		require.NoError(err)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		result, err := client.ImportCallObservation(ctx, []byte(callObservationImportBody))
		require.NoError(err)
		assert.Equal(int64(7), result.MessageID)
		assert.Equal(int64(3), result.SourceID)
		require.Len(attempts, 3)
		assert.GreaterOrEqual(attempts[1].Sub(attempts[0]), operationBusyRetryDelay)
		assert.GreaterOrEqual(attempts[2].Sub(attempts[1]), time.Second)
		require.Len(bodies, 3)
		assert.Equal(bodies[0], bodies[1])
		assert.Equal(bodies[0], bodies[2])
	})
}

func TestImportCallObservationRateLimitRetryStopsWhenCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		attempts := 0
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts++
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate_limit_exceeded","message":"too many requests"}`))
		}))
		client, err := New(Config{
			URL: "http://127.0.0.1", AllowInsecure: true,
			APIKey: "test-owner-key", HTTPClient: server.Client(),
		})
		require.NoError(err)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := client.ImportCallObservation(ctx, []byte(callObservationImportBody))
			done <- err
		}()

		synctest.Wait()
		require.Equal(1, attempts)
		require.Empty(done)
		cancel()
		synctest.Wait()
		require.Len(done, 1)
		require.ErrorIs(<-done, context.Canceled)
		require.Equal(1, attempts)
	})
}

func TestListCallObservationsRetriesRateLimitedPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		var attempts []time.Time
		var queries []string
		server := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts = append(attempts, time.Now())
			queries = append(queries, r.URL.RawQuery)
			w.Header().Set("Content-Type", "application/json")
			if len(attempts) == 1 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate_limit_exceeded","message":"too many requests"}`))
				return
			}
			_, _ = w.Write([]byte(`{"items":[]}`))
		}))
		client, err := New(Config{
			URL: "http://127.0.0.1", AllowInsecure: true,
			APIKey: "test-owner-key", HTTPClient: server.Client(),
		})
		require.NoError(err)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		page, err := client.ListCallObservations(ctx, store.CallQuery{AfterID: 42, Limit: 20})
		require.NoError(err)
		assert.Empty(page.Items)
		require.Len(attempts, 2)
		assert.GreaterOrEqual(attempts[1].Sub(attempts[0]), time.Second)
		require.Len(queries, 2)
		assert.Equal(queries[0], queries[1])
	})
}
