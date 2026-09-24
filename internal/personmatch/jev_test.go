package personmatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJevScoresMinimalPacketAndValidatesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/systemone", r.URL.Path)
		assert.Equal(t, "Bearer fixture-key", r.Header.Get("Authorization"))
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			assert.NoError(t, err)
			http.Error(w, "bad test request", http.StatusBadRequest)
			return
		}
		assert.Equal(t, "jev-1.13.0", body["model"])
		state, ok := body["state"].(map[string]any)
		if !assert.True(t, ok) {
			http.Error(w, "bad test state", http.StatusBadRequest)
			return
		}
		assert.NotContains(t, state, "notes")
		assert.NotContains(t, state, "messages")
		assert.NotContains(t, state, "full_contact_book")
		left, ok := state["left"].(map[string]any)
		if !assert.True(t, ok) {
			http.Error(w, "bad test endpoint", http.StatusBadRequest)
			return
		}
		assert.NotContains(t, left, "id")
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":0.81}}}`))
	}))
	defer server.Close()
	client := JevClient{Endpoint: server.URL + "/v1/systemone", ModelID: "jev-1.13.0", Key: "fixture-key", HTTPClient: server.Client()}
	judgment, err := client.Score(t.Context(), PairPacket{Left: PairEndpoint{Kind: "participant"}, Right: PairEndpoint{Kind: "participant"}})
	require.NoError(t, err)
	assert.InDelta(t, 0.81, judgment.Probability, 1e-9)
}

func TestJevDoesNotFollowRedirects(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	redirectRequests := 0
	redirectTargetRequests := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectTargetRequests++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":0.99}}}`))
	}))
	defer target.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectRequests++
		http.Redirect(w, &http.Request{}, target.URL, http.StatusFound)
	}))
	defer provider.Close()

	client := JevClient{
		Endpoint: provider.URL + "/v1/systemone", ModelID: "jev-1.13.0", Key: "fixture-key",
		HTTPClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }},
	}
	_, err := client.Score(t.Context(), PairPacket{Left: PairEndpoint{Email: "private@example.invalid"}})
	must.ErrorContains(err, "jev returned HTTP 302")
	checks.Equal(1, redirectRequests)
	checks.Zero(redirectTargetRequests, "Jev must not send credentials or identity data to a redirect target")
}

func TestJevRetriesOverloadOnlyWithinBound(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":0.80}}}`))
	}))
	defer server.Close()
	client := JevClient{Endpoint: server.URL + "/v1/systemone", ModelID: "jev-1.13.0", Key: "fixture-key", HTTPClient: server.Client()}
	judgment, err := client.Score(t.Context(), PairPacket{})
	require.NoError(t, err)
	assert.Equal(t, 3, attempts)
	assert.InDelta(t, 0.80, judgment.Probability, 1e-9)
}

func TestJevHonorsCanceledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()
	client := JevClient{Endpoint: server.URL + "/v1/systemone", ModelID: "jev-1.13.0", Key: "fixture-key", HTTPClient: server.Client()}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := client.Score(ctx, PairPacket{})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestJevRejectsMalformedOrMismatchedResponse(t *testing.T) {
	for _, tc := range []struct{ name, response string }{
		{"wrong model", `{"model":"jev-latest","answers":{"same_person":{"type":"noul","noul":0.95}}}`},
		{"wrong answer", `{"model":"jev-1.13.0","answers":{"same_person":{"type":"choice","noul":0.95}}}`},
		{"missing probability", `{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul"}}}`},
		{"out of range", `{"model":"jev-1.13.0","answers":{"same_person":{"type":"noul","noul":1.1}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tc.response)) }))
			defer server.Close()
			client := JevClient{Endpoint: server.URL + "/v1/systemone", ModelID: "jev-1.13.0", Key: "fixture-key", HTTPClient: server.Client()}
			_, err := client.Score(t.Context(), PairPacket{})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "fixture-key")
		})
	}
}
