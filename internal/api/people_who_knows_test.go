package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

func whoKnowsPost(t *testing.T, srv *api.Server, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/people/who-knows", strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	got := httptest.NewRecorder()
	srv.Router().ServeHTTP(got, request)
	return got
}

func TestWhoKnowsRejectsInvalidRequests(t *testing.T) {
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: testutil.NewTestStore(t), Logger: slog.New(slog.DiscardHandler),
	})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	for body, code := range map[string]string{
		`{"topic":"   "}`:                                          "missing_free_text",
		`{"topic":"x","limit":0}`:                                  "invalid_parameter",
		`{"topic":"x","limit":51}`:                                 "invalid_parameter",
		`{"topic":"x","evidence":11}`:                              "invalid_parameter",
		`{"topic":"x","min_score":0}`:                              "invalid_parameter",
		`{"topic":"x","min_score":1.5}`:                            "invalid_parameter",
		`{"topic":"x","after":"yesterday"}`:                        "invalid_parameter",
		`{"topic":"x","after":"2026-02-01","before":"2026-01-01"}`: "invalid_parameter",
		`{"topic":"x","unknown":1}`:                                "bad_request",
	} {
		got := whoKnowsPost(t, srv, body)
		assert.Equal(t, http.StatusBadRequest, got.Code, body)
		assert.Contains(t, got.Body.String(), code, body)
	}
}

func TestWhoKnowsNeedsVectorSearch(t *testing.T) {
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: testutil.NewTestStore(t), Logger: slog.New(slog.DiscardHandler),
	})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	got := whoKnowsPost(t, srv, `{"topic":"cloud"}`)
	assert.Equal(t, http.StatusServiceUnavailable, got.Code)
	assert.Contains(t, got.Body.String(), "vector_not_enabled")
}

type whoKnowsBackend struct {
	vector.Backend

	generation vector.Generation
	hits       []vector.FusedHit
	err        error
	request    vector.FusedRequest
}

func (b *whoKnowsBackend) ActiveGeneration(context.Context) (vector.Generation, error) {
	return b.generation, nil
}

func (b *whoKnowsBackend) FusedSearch(_ context.Context, r vector.FusedRequest) ([]vector.FusedHit, vector.SearchMetadata, error) {
	b.request = r
	return b.hits, vector.SearchMetadata{}, b.err
}

type whoKnowsEmbedder struct{ err error }

func (e whoKnowsEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0}, e.err
}

func newWhoKnowsVectorServer(t *testing.T, backend *whoKnowsBackend, embedder whoKnowsEmbedder) *api.Server {
	t.Helper()
	cfg := vector.Config{Enabled: true}
	cfg.Embed.Scope.MessageTypes = []string{"email"}
	backend.generation = vector.Generation{ID: 1, State: vector.GenerationActive, Fingerprint: cfg.GenerationFingerprint()}
	st := testutil.NewTestStore(t)
	engine := hybrid.NewEngine(backend, st.DB(), embedder, hybrid.Config{
		KPerSignal: 100, RRFK: 60, BuildScope: cfg.Embed.Scope.BuildScope(),
	})
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: st, HybridEngine: engine, Backend: backend,
		VectorCfg: cfg, Logger: slog.New(slog.DiscardHandler),
	})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	srv.SetEmbeddingStatus(func(context.Context, int64) (vector.EmbeddingStatus, error) {
		return vector.EmbeddingStatus{}, nil
	}, 0)
	return srv
}

func TestWhoKnowsReturnsEmptyListsWithReason(t *testing.T) {
	assert := assert.New(t)
	backend := &whoKnowsBackend{}
	srv := newWhoKnowsVectorServer(t, backend, whoKnowsEmbedder{})
	got := whoKnowsPost(t, srv, `{"topic":"sailboat insurance","after":"2026-01-01"}`)
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	assert.Equal("no-store", got.Header().Get("Cache-Control"))
	assert.JSONEq(`{"topic":"sailboat insurance","reason":"no_relevant_evidence","people":[],"unsaved_contacts":[]}`, got.Body.String())
	assert.Equal([]string{"email"}, backend.request.Filter.MessageTypes)
	require.NotNil(t, backend.request.Filter.After)
}

func TestWhoKnowsMapsSearchErrors(t *testing.T) {
	for _, test := range []struct {
		name     string
		backend  error
		embedder error
		status   int
		code     string
	}{
		{"index scope mismatch", vector.ErrIndexScopeMismatch, nil, http.StatusBadRequest, "index_scope_mismatch"},
		{"index stale", vector.ErrIndexStale, nil, http.StatusServiceUnavailable, "index_stale"},
		{"index building", vector.ErrIndexBuilding, nil, http.StatusServiceUnavailable, "index_building"},
		{"embedding timeout", nil, fmt.Errorf("%w: %w", vector.ErrEmbeddingTimeout, context.DeadlineExceeded), http.StatusServiceUnavailable, "embedding_timeout"},
		{"unexpected failure", errors.New("synthetic failure"), nil, http.StatusInternalServerError, "internal_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &whoKnowsBackend{err: test.backend}
			srv := newWhoKnowsVectorServer(t, backend, whoKnowsEmbedder{err: test.embedder})
			got := whoKnowsPost(t, srv, `{"topic":"cloud"}`)
			assert.Equal(t, test.status, got.Code, got.Body.String())
			assert.Contains(t, got.Body.String(), test.code)
			assert.NotContains(t, got.Body.String(), "query_timeout")
		})
	}
}

func TestWhoKnowsRejectsRestrictedAgent(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.net")
	require.NoError(err)
	srv := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}},
		Store:  st, Logger: slog.New(slog.DiscardHandler),
	})
	t.Cleanup(func() { require.NoError(srv.Shutdown(context.Background())) })
	issue := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens",
		strings.NewReader(fmt.Sprintf(`{"label":"reader","permissions":["search.read"],"source_ids":[%d]}`, source.ID)))
	issue.RemoteAddr = "127.0.0.1:12345"
	issue.Header.Set("X-Api-Key", "synthetic-owner-key")
	issue.Header.Set("Content-Type", "application/json")
	granted := httptest.NewRecorder()
	srv.Router().ServeHTTP(granted, issue)
	require.Equal(http.StatusCreated, granted.Code, granted.Body.String())
	var token struct {
		Secret string `json:"secret"`
	}
	require.NoError(json.Unmarshal(granted.Body.Bytes(), &token))

	got := whoKnowsPost(t, srv, `{"topic":"cloud"}`, apiprotocol.AgentTokenHeader, token.Secret)
	assert.Equal(t, http.StatusUnauthorized, got.Code, got.Body.String())
}

func TestWhoKnowsOpenAPI(t *testing.T) {
	assert := assert.New(t)
	path := api.OpenAPIDocument().Paths["/api/v1/people/who-knows"]
	require.NotNil(t, path)
	require.NotNil(t, path.Post)
	assert.Equal("findPeopleWhoKnow", path.Post.OperationID)
	assert.Nil(path.Get)
	assert.Contains(path.Post.Responses, "200")
	assert.Contains(path.Post.Responses, "503")
}
