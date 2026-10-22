package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/whoknows"
)

func whoKnowsGet(t *testing.T, srv *api.Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/people/who-knows?"+query, nil)
	request.RemoteAddr = "127.0.0.1:12345"
	got := httptest.NewRecorder()
	srv.Router().ServeHTTP(got, request)
	return got
}

func TestWhoKnowsRejectsInvalidRequests(t *testing.T) {
	srv, _ := newWhoKnowsVectorServer(t, &whoKnowsBackend{}, whoKnowsEmbedder{})
	for query, code := range map[string]string{
		"":                        "missing_free_text",
		"topic=x&limit=51":        "invalid_parameter",
		"topic=x&limit=many":      "invalid_limit",
		"topic=x&after=yesterday": "invalid_after",
	} {
		got := whoKnowsGet(t, srv, query)
		assert.Equal(t, http.StatusBadRequest, got.Code, query)
		assert.Contains(t, got.Body.String(), `"`+code+`"`, query)
	}
}

func TestWhoKnowsNeedsVectorSearch(t *testing.T) {
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: testutil.NewTestStore(t), Logger: slog.New(slog.DiscardHandler),
	})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	got := whoKnowsGet(t, srv, "topic=cloud")
	assert.Equal(t, http.StatusServiceUnavailable, got.Code)
	assert.Contains(t, got.Body.String(), "vector_not_enabled")
}

type whoKnowsBackend struct {
	vector.Backend

	generation vector.Generation
	hits       []vector.FusedHit
	request    vector.FusedRequest
}

func (b *whoKnowsBackend) ActiveGeneration(context.Context) (vector.Generation, error) {
	return b.generation, nil
}

func (b *whoKnowsBackend) FusedSearch(_ context.Context, r vector.FusedRequest) ([]vector.FusedHit, vector.SearchMetadata, error) {
	b.request = r
	return b.hits, vector.SearchMetadata{}, nil
}

type whoKnowsEmbedder struct{ err error }

func (e whoKnowsEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0}, e.err
}

func newWhoKnowsVectorServer(t *testing.T, backend *whoKnowsBackend, embedder whoKnowsEmbedder) (*api.Server, *store.Store) {
	t.Helper()
	cfg := vector.Config{Enabled: true}
	cfg.Embed.Scope.MessageTypes = []string{"email"}
	st := testutil.NewTestStore(t)
	options := api.ServerOptions{
		Config: &config.Config{}, Store: st, Backend: backend,
		VectorCfg: cfg, Logger: slog.New(slog.DiscardHandler),
	}
	backend.generation = vector.Generation{ID: 1, State: vector.GenerationActive, Fingerprint: options.VectorCfg.GenerationFingerprint()}
	options.HybridEngine = hybrid.NewEngine(backend, st.DB(), embedder, hybrid.Config{
		KPerSignal: 100, RRFK: 60, BuildScope: options.VectorCfg.Embed.Scope.BuildScope(),
	})
	srv := api.NewServerWithOptions(options)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	srv.SetEmbeddingStatus(func(context.Context, int64) (vector.EmbeddingStatus, error) {
		return vector.EmbeddingStatus{}, nil
	}, 0)
	return srv, st
}

func TestWhoKnowsReturnsEmptyLists(t *testing.T) {
	assert := assert.New(t)
	backend := &whoKnowsBackend{}
	srv, _ := newWhoKnowsVectorServer(t, backend, whoKnowsEmbedder{})
	got := whoKnowsGet(t, srv, "topic=sailboat+insurance&after=2026-01-01")
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	assert.Equal("no-store", got.Header().Get("Cache-Control"))
	var body whoknows.Response
	require.NoError(t, json.Unmarshal(got.Body.Bytes(), &body))
	assert.Equal("sailboat insurance", body.Topic)
	assert.Empty(body.People)
	assert.Empty(body.UnsavedContacts)
	assert.Equal([]string{"email"}, backend.request.Filter.MessageTypes)
	require.NotNil(t, backend.request.Filter.After)
}

func TestWhoKnowsPassesMinScoreToTheService(t *testing.T) {
	require := require.New(t)
	backend := &whoKnowsBackend{}
	srv, st := newWhoKnowsVectorServer(t, backend, whoKnowsEmbedder{})
	source, err := st.GetOrCreateSource("gmail", "owner@example.net")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "thread", "Thread")
	require.NoError(err)
	ana, err := st.EnsureParticipant("ana@example.net", "Ana Lopez", "example.net")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{
		ConversationID: conversation, SourceID: source.ID, SourceMessageID: "m1", MessageType: "email",
		SenderID: sql.NullInt64{Int64: ana, Valid: true},
	})
	require.NoError(err)
	// Only a min_score of 0.4 reaching the service keeps this hit.
	backend.hits = []vector.FusedHit{{MessageID: id, BM25Score: math.NaN(), VectorScore: .45}}

	got := whoKnowsGet(t, srv, "topic=kubernetes+networking&min_score=0.4")
	require.Equal(http.StatusOK, got.Code, got.Body.String())
	var body whoknows.Response
	require.NoError(json.Unmarshal(got.Body.Bytes(), &body))
	require.Len(body.UnsavedContacts, 1)
	assert.Equal(t, "ana@example.net", body.UnsavedContacts[0].EmailAddress)
}

// A wrapped context.DeadlineExceeded must stay an embedding timeout rather
// than become the generic query timeout.
func TestWhoKnowsMapsSearchErrors(t *testing.T) {
	timeout := fmt.Errorf("%w: %w", vector.ErrEmbeddingTimeout, context.DeadlineExceeded)
	srv, _ := newWhoKnowsVectorServer(t, &whoKnowsBackend{}, whoKnowsEmbedder{err: timeout})
	got := whoKnowsGet(t, srv, "topic=cloud")
	assert.Equal(t, http.StatusServiceUnavailable, got.Code, got.Body.String())
	assert.Contains(t, got.Body.String(), "embedding_timeout")
	assert.NotContains(t, got.Body.String(), "query_timeout")
}
