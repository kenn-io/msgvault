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
