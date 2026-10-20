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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/whoknows"
)

func whoKnowsGet(t *testing.T, srv *api.Server, query string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/people/who-knows?"+query, nil)
	request.RemoteAddr = "127.0.0.1:12345"
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
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

// EmbeddedMessageCountForIDs reports every message as embedded.
func (b *whoKnowsBackend) EmbeddedMessageCountForIDs(_ context.Context, _ vector.GenerationID, ids []int64) (int64, error) {
	return int64(len(ids)), nil
}

type whoKnowsEmbedder struct{ err error }

func (e whoKnowsEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0}, e.err
}

func newWhoKnowsVectorServer(t *testing.T, backend *whoKnowsBackend, embedder whoKnowsEmbedder, tweak ...func(*api.ServerOptions)) (*api.Server, *store.Store) {
	t.Helper()
	cfg := vector.Config{Enabled: true}
	cfg.Embed.Scope.MessageTypes = []string{"email"}
	backend.generation = vector.Generation{ID: 1, State: vector.GenerationActive, Fingerprint: cfg.GenerationFingerprint()}
	st := testutil.NewTestStore(t)
	engine := hybrid.NewEngine(backend, st.DB(), embedder, hybrid.Config{
		KPerSignal: 100, RRFK: 60, BuildScope: cfg.Embed.Scope.BuildScope(),
	})
	options := api.ServerOptions{
		Config: &config.Config{}, Store: st, HybridEngine: engine, Backend: backend,
		VectorCfg: cfg, Logger: slog.New(slog.DiscardHandler),
	}
	for _, change := range tweak {
		change(&options)
	}
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
	assert.JSONEq(`{"topic":"sailboat insurance","limits":["partial_index"],"people":[],"unsaved_contacts":[]}`, got.Body.String(),
		"the email-only index leaves chat unsearched")
	assert.Equal([]string{"email"}, backend.request.Filter.MessageTypes)
	require.NotNil(t, backend.request.Filter.After)
}

func TestWhoKnowsRanksAuthorsFromTheStore(t *testing.T) {
	require := require.New(t)
	backend := &whoKnowsBackend{}
	srv, st := newWhoKnowsVectorServer(t, backend, whoKnowsEmbedder{})
	source, err := st.GetOrCreateSource("gmail", "owner@example.net")
	require.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "thread", "Thread")
	require.NoError(err)
	ana, err := st.EnsureParticipant("ana@example.net", "Ana Lopez", "example.net")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(ana)
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{
		ConversationID: conversation, SourceID: source.ID, SourceMessageID: "m1", MessageType: "email",
		SenderID: sql.NullInt64{Int64: ana, Valid: true},
		SentAt:   sql.NullTime{Time: time.Now().AddDate(0, 0, -3), Valid: true},
		Subject:  sql.NullString{String: "kube-proxy rules", Valid: true},
	})
	require.NoError(err)
	// Only a min_score of 0.4 reaching the service keeps this hit.
	backend.hits = []vector.FusedHit{{MessageID: id, BM25Score: math.NaN(), VectorScore: .45}}

	got := whoKnowsGet(t, srv, "topic=kubernetes+networking&min_score=0.4")
	require.Equal(http.StatusOK, got.Code, got.Body.String())
	var body whoknows.Response
	require.NoError(json.Unmarshal(got.Body.Bytes(), &body))
	require.Len(body.People, 1)
	assert.Equal(t, person.ID, body.People[0].PersonID)
}

func TestWhoKnowsMapsSearchErrors(t *testing.T) {
	for _, test := range []struct {
		name     string
		backend  error
		embedder error
		status   int
		code     string
	}{
		{"index stale", vector.ErrIndexStale, nil, http.StatusServiceUnavailable, "index_stale"},
		{"embedding timeout", nil, fmt.Errorf("%w: %w", vector.ErrEmbeddingTimeout, context.DeadlineExceeded), http.StatusServiceUnavailable, "embedding_timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &whoKnowsBackend{err: test.backend}
			srv, _ := newWhoKnowsVectorServer(t, backend, whoKnowsEmbedder{err: test.embedder})
			got := whoKnowsGet(t, srv, "topic=cloud")
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

	got := whoKnowsGet(t, srv, "topic=cloud", apiprotocol.AgentTokenHeader, token.Secret)
	assert.Equal(t, http.StatusUnauthorized, got.Code, got.Body.String())
}

// The route reads the archive, so a sync holding the operation gate does not
// hold it up.
func TestWhoKnowsAnswersWhileASyncRuns(t *testing.T) {
	gate := api.NewSerialOperationGate()
	srv, _ := newWhoKnowsVectorServer(t, &whoKnowsBackend{}, whoKnowsEmbedder{}, func(o *api.ServerOptions) {
		o.OperationGate = gate
	})
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "sync")
	require.True(t, ok)
	t.Cleanup(release)
	got := whoKnowsGet(t, srv, "topic=cloud")
	assert.Equal(t, http.StatusOK, got.Code, got.Body.String())
}

// bareStore has no who-knows reads.
type bareStore struct{ api.MessageStore }

func TestWhoKnowsNeedsTheStoreReads(t *testing.T) {
	backend := &whoKnowsBackend{}
	srv, _ := newWhoKnowsVectorServer(t, backend, whoKnowsEmbedder{}, func(o *api.ServerOptions) {
		o.Store = bareStore{o.Store}
	})
	got := whoKnowsGet(t, srv, "topic=cloud")
	assert.Equal(t, http.StatusServiceUnavailable, got.Code)
	assert.Contains(t, got.Body.String(), "who_knows_unavailable")
}
