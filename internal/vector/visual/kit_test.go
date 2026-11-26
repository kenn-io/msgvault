package visual

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestKitProviderInputs(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embed", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		requests = append(requests, body)
		_, err := w.Write([]byte(`{"data":[{"index":0,"embedding":[3,4]}]}`))
		require.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	p, err := NewKitProvider("kit", server.URL+"/v1", "gemini-embedding-2", 2, "", server.Client(), "generation")
	require.NoError(t, err)
	doc, _, err := AssembleDocument(AssembleRequest{
		Owner: Owner{MessageID: 1, BlobHash: testHash}, Media: &MediaInput{MIMEType: "image/png", BlobHash: testHash, Bytes: []byte{1, 2, 3}},
		Context: MessageContext{Body: "private context"}, Policy: ContextPolicy{ExcludeContext: true},
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
		{"unsupported", 415, `{"error":{"code":"unsupported_content"}}`, ErrProviderRejected},
		{"bad input", 400, ``, ErrProviderRejected},
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

func TestKitConfigurationFailureCanRecover(t *testing.T) {
	f, _, work := workerFixture(t, 2)
	ready := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !ready {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"model_not_found"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[3,4]}]}`))
	}))
	t.Cleanup(server.Close)
	provider, err := NewKitProvider("kit", server.URL+"/v1", "model", 2, "", server.Client(), "generation")
	require.NoError(t, err)
	for i := range work {
		work[i].Document.Parts = []InputPart{{Media: work[i].Document.Parts[len(work[i].Document.Parts)-1].Media}}
	}
	worker := newTestVisualWorker(t, f, provider, newFakeVisualBackend())
	worker.config.MaxBatchItems = 1
	result, err := worker.Run(t.Context(), work)
	require.Error(t, err)
	assert.Zero(t, result.Terminal)
	ready = true
	for i := range work {
		claim, ok, err := f.Store.ClaimVisualWork(t.Context(), store.VisualClaimRequest{GenerationID: work[i].Claim.GenerationID, Owner: work[i].Candidate.Owner, ProposedRevision: work[i].Document.Revision, LeaseOwner: "retry", Now: time.Now().UTC(), LeaseDuration: time.Minute, SourceFence: work[i].Claim.SourceFence})
		require.NoError(t, err)
		require.True(t, ok)
		work[i].Claim = claim
	}
	result, err = worker.Run(t.Context(), work)
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.Published)
}
