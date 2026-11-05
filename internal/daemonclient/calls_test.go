package daemonclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportCallObservationPreservesRawInteger(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	oldDelay := operationBusyRetryDelay
	operationBusyRetryDelay = time.Millisecond
	t.Cleanup(func() { operationBusyRetryDelay = oldDelay })
	requests := 0
	var captured []byte
	var readErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, readErr = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"operation_in_progress","message":"sync is running"}`))
			return
		}
		_, _ = w.Write([]byte(`{"message_id":42,"source_id":7}`))
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, APIKey: "example-key", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	var notices []string
	client.SetBusyNotifier(func(message string) { notices = append(notices, message) })
	result, err := client.ImportCallObservation(t.Context(), []byte(`{"format":"normalized","account":"a","record":{"version":1,"provider":"signal","account":"a","record_id":"7","kind":"call","direction":"incoming","outcome":"answered","owner_participation":"joined","occurred_at":"2026-10-01T09:00:00Z","precision":"instant","raw":{"sequence":9007199254740993}}}`))
	requirements.NoError(err)
	requirements.NoError(readErr)
	assertions.Contains(string(captured), `9007199254740993`)
	assertions.Equal(int64(42), result.MessageID)
	assertions.Equal(2, requests)
	assertions.Contains(notices, "sync is running")
}
