package daemonclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCallsRetryBusyResponse(t *testing.T) {
	oldDelay := operationBusyRetryDelay
	operationBusyRetryDelay = time.Millisecond
	t.Cleanup(func() { operationBusyRetryDelay = oldDelay })
	for _, operation := range []string{"import", "list", "summary"} {
		t.Run(operation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				if requests == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":"operation_in_progress","message":"sync is running"}`))
					return
				}
				if operation == "import" {
					_, _ = w.Write([]byte(`{"message_id":42,"source_id":7}`))
				} else {
					_, _ = w.Write([]byte(`{"items":[]}`))
				}
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, APIKey: "example-key", AllowInsecure: true})
			require.NoError(err)
			t.Cleanup(func() { _ = client.Close() })
			var notices []string
			client.SetBusyNotifier(func(message string) { notices = append(notices, message) })
			switch operation {
			case "import":
				result, err := client.ImportCallObservation(t.Context(), []byte(`{"format":"signal_desktop","account":"example","record":{"callId":"call-1","timestamp":1790841600000}}`))
				require.NoError(err)
				assert.Equal(int64(42), result.MessageID)
			case "list":
				_, err = client.ListCallObservations(t.Context(), store.CallQuery{Limit: 1})
				require.NoError(err)
			case "summary":
				_, err = client.SummarizeCalls(t.Context(), store.CallQuery{Limit: 1})
				require.NoError(err)
			}
			assert.Equal(2, requests)
			assert.Contains(notices, "sync is running")
		})
	}
}

func TestImportCallObservationPreservesRawInteger(t *testing.T) {
	var captured []byte
	var readErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, readErr = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message_id":42,"source_id":7}`))
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, APIKey: "example-key", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	_, err = client.ImportCallObservation(t.Context(), []byte(`{"format":"normalized","account":"a","record":{"version":1,"provider":"signal","account":"a","record_id":"7","kind":"call","direction":"incoming","outcome":"answered","owner_participation":"joined","occurred_at":"2026-10-01T09:00:00Z","precision":"instant","raw":{"sequence":9007199254740993}}}`))
	require.NoError(t, err)
	require.NoError(t, readErr)
	assert.Contains(t, string(captured), `9007199254740993`)
}
