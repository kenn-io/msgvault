package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWhoKnowsParameterValidation(t *testing.T) {
	check := assert.New(t)
	srv := newWhoKnowsTestServer(t)
	for _, query := range []string{"q=topic&limit=0", "q=topic&evidence=11", "q=topic&candidates=49", "q=topic&half_life_days=3651", "q=topic&min_profile_score=2", "q=topic&min_score=NaN", "q=topic&tiebreak=random", "q=topic&after=2026-10-18&before=2026-10-17", "q=topic&include_owner=invalid"} {
		got := whoKnowsRequest(t, srv, "/api/v1/people/who-knows?"+query)
		check.Equal(http.StatusBadRequest, got.Code, query)
		check.Contains(got.Body.String(), "invalid_parameter")
	}
	got := whoKnowsRequest(t, srv, "/api/v1/people/who-knows")
	check.Equal(http.StatusBadRequest, got.Code)
	check.Contains(got.Body.String(), "missing_free_text")
	got = whoKnowsRequest(t, srv, "/api/v1/people/who-knows?q=topic")
	check.Equal(http.StatusServiceUnavailable, got.Code)
	check.Contains(got.Body.String(), "vector_not_enabled")
}

func TestWhoKnowsOpenAPI(t *testing.T) {
	check := assert.New(t)
	op := api.OpenAPIDocument().Paths["/api/v1/people/who-knows"]
	if check.NotNil(op) {
		check.Equal("findPeopleWhoKnow", op.Get.OperationID)
		check.Contains(op.Get.Responses, "200")
		for _, parameter := range op.Get.Parameters {
			if parameter.Name == "min_score" || parameter.Name == "min_profile_score" {
				check.Equal("double", parameter.Schema.Format)
			}
		}
	}
}

func newWhoKnowsTestServer(t *testing.T) *api.Server {
	t.Helper()
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}}, Store: testutil.NewTestStore(t), Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	return srv
}
func whoKnowsRequest(t *testing.T, srv *api.Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "127.0.0.1:12345"
	got := httptest.NewRecorder()
	srv.Router().ServeHTTP(got, r)
	return got
}

func TestWhoKnowsRejectsRestrictedAgent(t *testing.T) {
	must := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.net")
	must.NoError(err)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	body := fmt.Sprintf(`{"label":"reader","permissions":["search.read"],"source_ids":[%d]}`, source.ID)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent-tokens", strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("X-Api-Key", "synthetic-owner-key")
	request.Header.Set("Content-Type", "application/json")
	granted := httptest.NewRecorder()
	srv.Router().ServeHTTP(granted, request)
	must.Equal(http.StatusCreated, granted.Code, granted.Body.String())
	var token struct {
		Secret string `json:"secret"`
	}
	must.NoError(json.Unmarshal(granted.Body.Bytes(), &token))
	must.NotEmpty(token.Secret)
	request = httptest.NewRequest(http.MethodGet, "/api/v1/people/who-knows?q=cloud", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set(apiprotocol.AgentTokenHeader, token.Secret)
	got := httptest.NewRecorder()
	srv.Router().ServeHTTP(got, request)
	assert.Equal(t, http.StatusUnauthorized, got.Code, got.Body.String())
}

type whoKnowsVectorBackend struct {
	vector.Backend

	generation vector.Generation
	err        error
	request    vector.FusedRequest
}

func (b *whoKnowsVectorBackend) ActiveGeneration(context.Context) (vector.Generation, error) {
	return b.generation, b.err
}
func (b *whoKnowsVectorBackend) FusedSearch(_ context.Context, r vector.FusedRequest) ([]vector.FusedHit, vector.SearchMetadata, error) {
	b.request = r
	return []vector.FusedHit{}, vector.SearchMetadata{}, nil
}

type whoKnowsEmbedder struct{}

func (whoKnowsEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0}, nil
}

type whoKnowsDeadlineEmbedder struct{}

func (whoKnowsDeadlineEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return nil, context.DeadlineExceeded
}

type whoKnowsMessageOnly struct{ api.MessageStore }

func TestWhoKnowsErrorsAndResponse(t *testing.T) {
	st := testutil.NewTestStore(t)
	cfg := vector.Config{Enabled: true}
	cfg.Embed.Scope.MessageTypes = []string{"email"}
	for _, test := range []struct {
		err    error
		code   string
		status int
	}{{vector.ErrNotEnabled, "vector_not_enabled", 503}, {vector.ErrIndexStale, "index_stale", 503}, {vector.ErrIndexBuilding, "index_building", 503}, {vector.ErrIndexScopeMismatch, "index_scope_mismatch", 400}, {errors.New("synthetic failure"), "semantic_search_unavailable", 503}, {nil, "", 200}} {
		t.Run(test.code, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)
			backend := &whoKnowsVectorBackend{generation: vector.Generation{ID: 1, State: vector.GenerationActive, Fingerprint: cfg.GenerationFingerprint()}, err: test.err}
			engine := hybrid.NewEngine(backend, st.DB(), whoKnowsEmbedder{}, hybrid.Config{KPerSignal: 100, RRFK: 60, BuildScope: cfg.Embed.Scope.BuildScope()})
			server := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: st, HybridEngine: engine, Backend: backend, VectorCfg: cfg, Logger: slog.New(slog.DiscardHandler)})
			t.Cleanup(func() { require.NoError(t, server.Shutdown(context.Background())) })
			server.SetEmbeddingStatus(func(context.Context, int64) (vector.EmbeddingStatus, error) { return vector.EmbeddingStatus{}, nil }, 0)
			got := whoKnowsRequest(t, server, "/api/v1/people/who-knows?q=cloud")
			must.Equal(test.status, got.Code, got.Body.String())
			if test.err != nil {
				check.Contains(got.Body.String(), test.code)
			} else {
				check.Equal("no-store", got.Header().Get("Cache-Control"))
				var body map[string]any
				must.NoError(json.Unmarshal(got.Body.Bytes(), &body))
				for _, key := range []string{"topic", "params", "people", "unsaved_candidates", "coverage", "retrieval", "tiebreak", "excluded", "limits_applied"} {
					check.Contains(body, key)
				}
				check.Equal([]string{"email"}, backend.request.Filter.MessageTypes)
			}
		})
	}
	backend := &whoKnowsVectorBackend{generation: vector.Generation{ID: 1, State: vector.GenerationActive, Fingerprint: cfg.GenerationFingerprint()}}
	engine := hybrid.NewEngine(backend, st.DB(), whoKnowsEmbedder{}, hybrid.Config{})
	server := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Store: whoKnowsMessageOnly{MessageStore: st}, HybridEngine: engine, Backend: backend, VectorCfg: cfg, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, server.Shutdown(context.Background())) })
	got := whoKnowsRequest(t, server, "/api/v1/people/who-knows?q=cloud")
	assert.Equal(t, 503, got.Code)
	assert.Contains(t, got.Body.String(), "person_scope_unavailable")
}

func TestWhoKnowsEmbeddingTimeoutKeepsSpecificErrorCode(t *testing.T) {
	st := testutil.NewTestStore(t)
	cfg := vector.Config{Enabled: true}
	cfg.Embed.Scope.MessageTypes = []string{"email"}
	backend := &whoKnowsVectorBackend{
		generation: vector.Generation{ID: 1, State: vector.GenerationActive, Fingerprint: cfg.GenerationFingerprint()},
	}
	engine := hybrid.NewEngine(backend, st.DB(), whoKnowsDeadlineEmbedder{}, hybrid.Config{
		KPerSignal: 100,
		RRFK:       60,
		BuildScope: cfg.Embed.Scope.BuildScope(),
	})
	server := api.NewServerWithOptions(api.ServerOptions{
		Config:       &config.Config{},
		Store:        st,
		HybridEngine: engine,
		Backend:      backend,
		VectorCfg:    cfg,
		Logger:       slog.New(slog.DiscardHandler),
	})
	t.Cleanup(func() { require.NoError(t, server.Shutdown(context.Background())) })
	server.SetEmbeddingStatus(func(context.Context, int64) (vector.EmbeddingStatus, error) {
		return vector.EmbeddingStatus{}, nil
	}, 0)

	got := whoKnowsRequest(t, server, "/api/v1/people/who-knows?q=cloud")
	assert.Equal(t, http.StatusServiceUnavailable, got.Code, got.Body.String())
	assert.Contains(t, got.Body.String(), "embedding_timeout")
	assert.NotContains(t, got.Body.String(), "query_timeout")
}

func TestWhoKnowsExcerptSchemaAllowsNull(t *testing.T) {
	schema := api.OpenAPIDocument().Components.Schemas.Map()["WhoKnowsEvidence"]
	require.NotNil(t, schema)
	assert.Len(t, schema.Properties["excerpt"].OneOf, 2)
}
