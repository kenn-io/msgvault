package daemonclient

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personcard"
)

func TestGetPersonCardForwardsOptionsAndPreservesEnvelope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	asOf := time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/people/email:alex@example.com/card", r.URL.Path)
		assert.Equal(t, "owner", r.Header.Get("X-Api-Key"))
		assert.Equal(t, "2026-10-17T15:00:00Z", r.URL.Query().Get("as_of"))
		assert.Equal(t, "4", r.URL.Query().Get("max_items"))
		assert.Equal(t, "8192", r.URL.Query().Get("max_bytes"))
		assert.Equal(t, "markdown", r.URL.Query().Get("format"))
		assert.Equal(t, "external", r.URL.Query().Get("affiliation"))
		w.Header().Set("Content-Type", "application/json")
		assertions.NoError(json.MarshalWrite(w, personcard.PersonCardResult{SchemaVersion: 1, Status: "ok", Ref: "email:alex@example.com", AsOf: asOf, CollectedAt: asOf, Format: personcard.FormatMarkdown, Content: new("card\n"), ContentBytes: 5, Gaps: []string{"open_loops_unavailable"}}))
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	got, err := client.GetPersonCard(t.Context(), "email:alex@example.com", personcard.Options{AsOf: asOf, MaxItems: 4, MaxBytes: 8192, Format: personcard.FormatMarkdown, Affiliation: "external"})
	requirements.NoError(err)
	assertions.Equal("ok", got.Status)
	assertions.Equal("email:alex@example.com", got.Ref)
	requirements.NotNil(got.Content)
	assertions.Equal("card\n", *got.Content)
	assertions.Equal([]string{"open_loops_unavailable"}, got.Gaps)
}

func TestGetPersonCardPreservesDaemonErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, err := w.Write([]byte(`{"error":"ref_not_permitted","message":"owner required"}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	_, err = client.GetPersonCard(t.Context(), "person:7", personcard.Options{})
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusForbidden, apiErr.Status)
}
