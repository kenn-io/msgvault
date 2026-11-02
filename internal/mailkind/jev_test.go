package mailkind

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func jevFixture(t *testing.T) map[string]any {
	t.Helper()
	probabilities := map[string]float64{}
	for kind := range kindCriteria {
		probabilities[kind] = 0
	}
	probabilities["receipt"], probabilities["invoice"] = 0.9, 0.1
	return map[string]any{"model": ModelID, "answers": map[string]any{
		"transactional": map[string]any{"type": "noul", "noul": 0.95},
		"kind":          map[string]any{"type": "choice", "choice": "receipt", "confidence": 0.1, "probabilities": probabilities},
	}}
}

func encodeJevFixture(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}

func TestJevMailKindWire(t *testing.T) {
	response := encodeJevFixture(t, jevFixture(t))
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer synthetic-key", r.Header.Get("Authorization"))
		assert.Equal(t, http.MethodPost, r.Method)
		body, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request struct {
			State     Packet                    `json:"state"`
			Model     string                    `json:"model"`
			Questions map[string]map[string]any `json:"questions"`
		}
		if !assert.NoError(t, json.Unmarshal(body, &request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, ModelID, request.Model)
		assert.Equal(t, "masked fixture", request.State.Excerpt)
		assert.Len(t, request.Questions, 2)
		assert.Equal(t, "noul", request.Questions["transactional"]["type"])
		assert.Equal(t, "choice", request.Questions["kind"]["type"])
		assert.Len(t, request.Questions["kind"]["criteria"], 10)
		assert.NotContains(t, request.Questions["kind"], "choices")
		_, _ = w.Write(response)
	}))
	defer provider.Close()
	gateCalls := 0
	client := JevClient{Endpoint: provider.URL, Key: "synthetic-key", HTTPClient: provider.Client(),
		RequestGate: func(_ context.Context, dispatch func() error) (bool, error) { gateCalls++; return true, dispatch() }}
	result, err := client.Decide(t.Context(), Packet{SchemaVersion: PacketSchema, Excerpt: "masked fixture"})
	require.NoError(t, err)
	assert.Equal(t, "receipt", result.Kind)
	assert.InDelta(t, 0.9, result.Probability, 1e-12) // Choice probability, not the confidence score.
	assert.InDelta(t, 0.95, result.Transactional, 1e-12)
	assert.Equal(t, 1, result.Requests)
	assert.Equal(t, 1, gateCalls)
}

func fixtureObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	require.True(t, ok)
	return object
}

func fixtureProbabilities(t *testing.T, value map[string]any) map[string]float64 {
	t.Helper()
	probabilities, ok := fixtureObject(t, fixtureObject(t, value["answers"])["kind"])["probabilities"].(map[string]float64)
	require.True(t, ok)
	return probabilities
}

func TestJevMailKindRejectsMalformedAnswers(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, map[string]any){
		"model":            func(t *testing.T, v map[string]any) { t.Helper(); v["model"] = "other" },
		"missing question": func(t *testing.T, v map[string]any) { t.Helper(); delete(fixtureObject(t, v["answers"]), "kind") },
		"refusal": func(t *testing.T, v map[string]any) {
			t.Helper()
			fixtureObject(t, fixtureObject(t, v["answers"])["kind"])["type"] = "refusal"
		},
		"unknown choice": func(t *testing.T, v map[string]any) {
			t.Helper()
			fixtureObject(t, fixtureObject(t, v["answers"])["kind"])["choice"] = "arbitrary_label"
		},
		"wrong top": func(t *testing.T, v map[string]any) {
			t.Helper()
			fixtureObject(t, fixtureObject(t, v["answers"])["kind"])["choice"] = "invoice"
		},
		"missing probability": func(t *testing.T, v map[string]any) {
			t.Helper()
			delete(fixtureProbabilities(t, v), "other")
		},
		"invalid probability": func(t *testing.T, v map[string]any) {
			t.Helper()
			fixtureProbabilities(t, v)["receipt"] = 1.1
		},
		"incomplete distribution": func(t *testing.T, v map[string]any) {
			t.Helper()
			fixtureProbabilities(t, v)["receipt"] = 0.85
		},
		"missing predicate": func(t *testing.T, v map[string]any) {
			t.Helper()
			delete(fixtureObject(t, fixtureObject(t, v["answers"])["transactional"]), "noul")
		},
	} {
		t.Run(name, func(t *testing.T) {
			value := jevFixture(t)
			mutate(t, value)
			_, err := parseJev(encodeJevFixture(t, value))
			require.Error(t, err)
		})
	}
	for _, value := range []string{`{`, `{"model":"jev-1.13.0","model":"jev-1.13.0"}`, `{"answers":{"transactional":{"noul":NaN}}}`} {
		_, err := parseJev([]byte(value))
		require.Error(t, err)
	}
}

func TestJevMailKindRechecksConsentBeforeRetry(t *testing.T) {
	requests, gates := 0, 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests++; w.WriteHeader(http.StatusTooManyRequests) }))
	defer provider.Close()
	client := JevClient{Endpoint: provider.URL, Key: "synthetic-key", HTTPClient: provider.Client(),
		RequestGate: func(_ context.Context, dispatch func() error) (bool, error) {
			gates++
			if gates > 1 {
				return false, nil
			}
			return true, dispatch()
		}}
	result, err := client.Decide(t.Context(), Packet{SchemaVersion: PacketSchema})
	require.ErrorIs(t, err, ErrConsentRevoked)
	assert.Equal(t, 1, requests)
	assert.Equal(t, 1, result.Requests)
	assert.Equal(t, 2, gates)
	client.RequestGate = nil
	_, err = client.Decide(t.Context(), Packet{SchemaVersion: PacketSchema})
	require.Error(t, err)
	assert.Equal(t, 1, requests)
}

func TestJevMailKindRejectsRedirectAndOversize(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls++ }))
	defer target.Close()
	oversized := encodeJevFixture(t, jevFixture(t))
	oversized = append(oversized, bytes.Repeat([]byte(" "), (1<<20)+1-len(oversized))...)
	for _, redirect := range []bool{true, false} {
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if redirect {
				http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				return
			}
			_, _ = w.Write(oversized)
		}))
		client := JevClient{Endpoint: provider.URL, Key: "synthetic-key", HTTPClient: provider.Client(),
			RequestGate: func(_ context.Context, dispatch func() error) (bool, error) { return true, dispatch() }}
		_, err := client.Decide(t.Context(), Packet{SchemaVersion: PacketSchema})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "sensitive response")
		provider.Close()
	}
	assert.Zero(t, targetCalls)
}
