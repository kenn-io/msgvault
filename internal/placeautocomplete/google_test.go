package placeautocomplete

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutocompleteAndExplicitSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls []string
	var mu sync.Mutex
	longID := strings.Repeat("a", 300)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		assert.Equal("synthetic-key", r.Header.Get("X-Goog-Api-Key"))
		switch r.URL.Path {
		case "/places:autocomplete":
			assert.Equal(http.MethodPost, r.Method)
			assert.Equal("suggestions.placePrediction.placeId,suggestions.placePrediction.text.text", r.Header.Get("X-Goog-FieldMask"))
			var body map[string]any
			if !assert.NoError(json.UnmarshalRead(r.Body, &body)) {
				return
			}
			assert.Equal("session-1", body["sessionToken"])
			assert.Equal("12 Example", body["input"])
			_, _ = fmt.Fprintf(w, `{"suggestions":[{"placePrediction":{"placeId":"%s","text":{"text":"12 Example Street"}}},{"placePrediction":{"placeId":"candidate-2","text":{"text":"Other Street"}}}]}`, longID)
		case "/places/" + longID:
			assert.Equal(http.MethodGet, r.Method)
			assert.Equal("session-1", r.URL.Query().Get("sessionToken"))
			assert.Equal("formattedAddress,postalAddress,addressComponents", r.Header.Get("X-Goog-FieldMask"))
			_, _ = w.Write([]byte(`{"formattedAddress":"12 Example Street","postalAddress":{"regionCode":"CA"},"addressComponents":[]}`))
		default:
			assert.Fail("unexpected request path", "%s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := Client{Key: "synthetic-key", HTTP: server.Client(), baseURL: server.URL}
	got, err := client.Autocomplete(t.Context(), Request{Input: "12 Example", SessionToken: "session-1", Limit: 1})
	require.NoError(err)
	require.Len(got.Candidates, 1)
	assert.Equal(longID, got.Candidates[0].PlaceID)
	assert.Equal("session-1", got.SessionToken)
	mu.Lock()
	assert.Equal([]string{"/places:autocomplete"}, calls)
	mu.Unlock()
	details, err := client.Select(t.Context(), got.Candidates[0].PlaceID, got.SessionToken, "", "")
	require.NoError(err)
	assert.Equal("12 Example Street", details.FormattedAddress)
	mu.Lock()
	assert.Equal([]string{"/places:autocomplete", "/places/" + longID}, calls)
	mu.Unlock()
}

func TestInvalidInputDoesNotSendRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte(`{}`)) }))
	defer server.Close()
	client := Client{Key: "synthetic-key", HTTP: server.Client(), baseURL: server.URL}
	for _, tc := range []struct {
		input   Request
		message string
	}{
		{Request{Input: "", SessionToken: "s", Limit: 5}, "input must contain 1 to 256 characters"},
		{Request{Input: "partial", SessionToken: "bad token", Limit: 5}, "session token must contain 1 to 36 letters, digits, underscores or hyphens"},
		{Request{Input: "partial", SessionToken: "s", Limit: 6}, "limit must be between 1 and 5"},
		{Request{Input: "partial", SessionToken: "s", Limit: 5, Bias: &Bias{Latitude: 91, RadiusMeters: 10}}, "location bias requires valid latitude, longitude and radius between 0 and 50000 metres"},
	} {
		_, err := client.Autocomplete(t.Context(), tc.input)
		require.EqualError(err, tc.message)
	}
	_, err := client.Select(t.Context(), "../outside", "s", "", "")
	require.EqualError(err, "invalid place ID")
	assert.Zero(calls.Load())
}

func TestCanceledRequestReturnsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := Client{Key: "synthetic-key", HTTP: server.Client(), baseURL: server.URL}
	_, err := client.Autocomplete(ctx, Request{Input: "partial", SessionToken: "s", Limit: 5})
	require.ErrorIs(t, err, context.Canceled)
}

func TestProviderErrorsDoNotExposeBodyOrKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "synthetic-secret", http.StatusBadRequest) }))
	defer server.Close()
	client := Client{Key: "synthetic-secret", HTTP: server.Client(), baseURL: server.URL}
	_, err := client.Autocomplete(t.Context(), Request{Input: "partial", SessionToken: "s", Limit: 5})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "synthetic-secret")
	assert.True(t, strings.Contains(err.Error(), "400"))
}
