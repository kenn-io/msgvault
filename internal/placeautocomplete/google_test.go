package placeautocomplete

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutocompleteAndExplicitSelection(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		assert.Equal(t, "synthetic-key", r.Header.Get("X-Goog-Api-Key"))
		switch r.URL.Path {
		case "/places:autocomplete":
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "suggestions.placePrediction.placeId,suggestions.placePrediction.text.text", r.Header.Get("X-Goog-FieldMask"))
			var body map[string]any
			require.NoError(t, json.UnmarshalRead(r.Body, &body))
			assert.Equal(t, "session-1", body["sessionToken"])
			assert.Equal(t, "12 Example", body["input"])
			_, _ = w.Write([]byte(`{"suggestions":[{"placePrediction":{"placeId":"candidate-1","text":{"text":"12 Example Street"}}},{"placePrediction":{"placeId":"candidate-2","text":{"text":"Other Street"}}}]}`))
		case "/places/candidate-1":
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "session-1", r.URL.Query().Get("sessionToken"))
			assert.Equal(t, "formattedAddress,postalAddress,addressComponents", r.Header.Get("X-Goog-FieldMask"))
			_, _ = w.Write([]byte(`{"formattedAddress":"12 Example Street","postalAddress":{"regionCode":"CA"},"addressComponents":[]}`))
		default:
			assert.Fail(t, "unexpected request path", "%s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := Client{Key: "synthetic-key", HTTP: server.Client(), baseURL: server.URL}
	got, err := client.Autocomplete(t.Context(), Request{Input: "12 Example", SessionToken: "session-1", Limit: 1})
	require.NoError(t, err)
	require.Len(t, got.Candidates, 1)
	assert.Equal(t, "candidate-1", got.Candidates[0].PlaceID)
	assert.Equal(t, "session-1", got.SessionToken)
	assert.Equal(t, []string{"/places:autocomplete"}, calls)
	details, err := client.Select(t.Context(), got.Candidates[0].PlaceID, got.SessionToken, "", "")
	require.NoError(t, err)
	assert.Equal(t, "12 Example Street", details.FormattedAddress)
	assert.Equal(t, []string{"/places:autocomplete", "/places/candidate-1"}, calls)
}

func TestInvalidInputDoesNotSendRequest(t *testing.T) {
	client := Client{Key: "synthetic-key", baseURL: "http://127.0.0.1:1"}
	for _, input := range []Request{{Input: "", SessionToken: "s", Limit: 5}, {Input: "partial", SessionToken: "bad token", Limit: 5}, {Input: "partial", SessionToken: "s", Limit: 6}, {Input: "partial", SessionToken: "s", Limit: 5, Bias: &Bias{Latitude: 91, RadiusMeters: 10}}} {
		_, err := client.Autocomplete(t.Context(), input)
		require.Error(t, err)
	}
	_, err := client.Select(t.Context(), "../outside", "s", "", "")
	require.Error(t, err)
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
