package visual

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKitProviderInputs(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embed", r.URL.Path)
		var body map[string]any
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
			return
		}
		requests = append(requests, body)
		_, err := w.Write([]byte(`{"data":[{"index":0,"embedding":[3,4]}]}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	p, err := NewKitProvider("kit", server.URL+"/v1", "gemini-embedding-2", 2, "", server.Client(), "generation")
	require.NoError(t, err)
	doc, _, err := AssembleDocument(AssembleRequest{
		Owner: Owner{MessageID: 1, BlobHash: testHash}, Media: &MediaInput{MIMEType: "image/png", BlobHash: testHash, Bytes: []byte{1, 2, 3}},
	})
	require.NoError(t, err)
	result, err := p.EmbedDocuments(t.Context(), []DocumentInput{doc})
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, int64(1), result[0].Owner.MessageID)
	_, _, err = p.EmbedQuery(t.Context(), QueryInput{Text: "beach"})
	require.NoError(t, err)
	_, _, err = p.EmbedQuery(t.Context(), QueryInput{Image: &MediaInput{MIMEType: "image/png", Bytes: []byte{4}}})
	require.NoError(t, err)
	require.Len(t, requests, 3)
	assert.Equal(t, []any{map[string]any{"role": "document", "media_type": "image/png", "data": "AQID"}}, requests[0]["inputs"])
	assert.Equal(t, []any{map[string]any{"role": "query", "text": "task: search result | query: beach"}}, requests[1]["inputs"])
	assert.Equal(t, []any{map[string]any{"role": "query", "media_type": "image/png", "data": "BA=="}}, requests[2]["inputs"])
	assert.Equal(t, "generation", p.PolicyFingerprint())
}

func TestKitProviderErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"credentials", 401, ``, ErrProviderUnauthorized},
		{"model", 400, `{"error":{"code":"model_not_found"}}`, ErrProviderConfiguration},
		{"forbidden model", 403, `{"error":{"code":"model_not_found"}}`, ErrProviderConfiguration},
		{"unsupported", 415, `{"error":{"code":"unsupported_content"}}`, ErrProviderConfiguration},
		{"unknown input", 400, ``, ErrProviderRetryable},
		{"input too long", 400, `{"error":{"code":"input_too_long"}}`, ErrProviderRejected},
		{"content policy", 400, `{"error":{"code":"content_policy_violation"}}`, ErrProviderRejected},
		{"rate limit", 429, ``, ErrProviderRetryable},
		{"bad vector", 200, `{"data":[{"index":0,"embedding":[1]}]}`, ErrProviderMalformed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(server.Close)
			p, err := NewKitProvider("kit", server.URL+"/v1", "model", 2, "", server.Client(), "generation")
			require.NoError(t, err)
			_, _, err = p.EmbedQuery(t.Context(), QueryInput{Text: "beach"})
			require.ErrorIs(t, err, tt.want)
			if tt.status == 429 {
				delay, ok := ProviderRetryAfter(err)
				assert.True(t, ok)
				assert.Equal(t, 3*time.Second, delay)
			}
		})
	}
}
