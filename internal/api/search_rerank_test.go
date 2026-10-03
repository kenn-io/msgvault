package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/hybrid"
	"go.kenn.io/msgvault/internal/vector/rerank"
)

// apiScriptedReranker stands in for the provider behind rerank.Reranker;
// the HTTP provider contract is covered in package rerank.
type apiScriptedReranker struct {
	scores map[string]float64
	err    error
	calls  int
}

func (s *apiScriptedReranker) Rerank(_ context.Context, request rerank.Request) (rerank.Result, error) {
	s.calls++
	if s.err != nil {
		return rerank.Result{}, s.err
	}
	scores := make([]float64, len(request.Candidates))
	for i, candidate := range request.Candidates {
		scores[i] = s.scores[candidate]
	}
	return rerank.Result{Scores: scores}, nil
}

type rerankSearchFixture struct {
	server   *Server
	backend  *fakeVectorBackend
	reranker *apiScriptedReranker
}

func newRerankSearchFixture(t *testing.T, withStage bool, rerankCfg vector.RerankConfig) *rerankSearchFixture {
	t.Helper()
	backend := &fakeVectorBackend{
		active: &vector.Generation{
			ID: 1, Model: "fake", Dimension: 4,
			Fingerprint: "fake:4", State: vector.GenerationActive,
		},
		searchHits: []vector.Hit{
			{MessageID: 41, Score: 0.9, Rank: 1},
			{MessageID: 42, Score: 0.8, Rank: 2},
			{MessageID: 43, Score: 0.7, Rank: 3},
		},
	}
	reranker := &apiScriptedReranker{scores: map[string]float64{
		"first hit": 0.1, "second hit": 0.4, "third hit": 0.95,
	}}
	texts := map[int64]string{41: "first hit", 42: "second hit", 43: "third hit"}
	cfg := hybrid.Config{ExpectedFingerprint: "fake:4", RRFK: 60, KPerSignal: 10}
	if withStage {
		cfg.Rerank = &hybrid.RerankStage{
			Reranker: reranker,
			Texts: func(_ context.Context, ids []int64) ([]string, error) {
				out := make([]string, len(ids))
				for i, id := range ids {
					out[i] = texts[id]
				}
				return out, nil
			},
			Model:      "cohere/rerank-4-pro",
			Candidates: 10,
		}
	}
	engine := hybrid.NewEngine(backend, nil, realEmbedder{dim: 4}, cfg)
	server := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store: &mockStore{messages: []APIMessage{
			{ID: 41, Subject: "first"}, {ID: 42, Subject: "second"}, {ID: 43, Subject: "third"},
		}},
		HybridEngine: engine,
		VectorCfg:    vector.Config{Rerank: rerankCfg},
		Backend:      backend,
		Logger:       testLogger(),
	})
	return &rerankSearchFixture{server: server, backend: backend, reranker: reranker}
}

type rerankSearchBody struct {
	Rerank *struct {
		Applied    bool   `json:"applied"`
		Model      string `json:"model"`
		Candidates int    `json:"candidates"`
		Fallback   string `json:"fallback"`
	} `json:"rerank"`
	Results []struct {
		ID    int64 `json:"id"`
		Score *struct {
			Rerank *float64 `json:"rerank"`
			Vector *float64 `json:"vector"`
		} `json:"score"`
	} `json:"results"`
}

func (f *rerankSearchFixture) search(t *testing.T, query string) (int, rerankSearchBody, ErrorResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	f.server.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/search?"+query, nil))
	var body rerankSearchBody
	var errResp ErrorResponse
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	} else {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &errResp), w.Body.String())
	}
	return w.Code, body, errResp
}

func resultIDs(body rerankSearchBody) []int64 {
	ids := make([]int64, len(body.Results))
	for i, result := range body.Results {
		ids[i] = result.ID
	}
	return ids
}

func TestHandleSearchRerankReordersResults(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	f := newRerankSearchFixture(t, true, vector.RerankConfig{Enabled: true})

	status, body, _ := f.search(t, "q=invoice&mode=vector&rerank=true&explain=true&page_size=2")
	require.Equal(http.StatusOK, status)
	assert.Equal([]int64{43, 42}, resultIDs(body))
	require.NotNil(body.Rerank)
	assert.True(body.Rerank.Applied)
	assert.Equal("cohere/rerank-4-pro", body.Rerank.Model)
	assert.Equal(3, body.Rerank.Candidates)
	assert.Empty(body.Rerank.Fallback)
	require.NotNil(body.Results[0].Score)
	require.NotNil(body.Results[0].Score.Rerank)
	assert.InDelta(0.95, *body.Results[0].Score.Rerank, 1e-9)
	assert.NotNil(body.Results[0].Score.Vector, "retrieval signals stay visible")
	assert.GreaterOrEqual(f.backend.searchLimit, 10, "retrieval widens to the rerank window")
}

func TestHandleSearchRerankDefault(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	f := newRerankSearchFixture(t, true, vector.RerankConfig{Enabled: true, Default: true})

	status, body, _ := f.search(t, "q=invoice&mode=vector")
	require.Equal(http.StatusOK, status)
	require.NotNil(body.Rerank, "default=true reranks when the request is silent")
	assert.Equal([]int64{43, 42, 41}, resultIDs(body))

	status, body, _ = f.search(t, "q=invoice&mode=vector&rerank=false")
	require.Equal(http.StatusOK, status)
	assert.Nil(body.Rerank, "rerank=false overrides the default")
	assert.Equal([]int64{41, 42, 43}, resultIDs(body))
	assert.Equal(1, f.reranker.calls)
}

func TestHandleSearchRerankIsOptInByDefault(t *testing.T) {
	t.Parallel()
	f := newRerankSearchFixture(t, true, vector.RerankConfig{Enabled: true})

	status, body, _ := f.search(t, "q=invoice&mode=vector")
	require.Equal(t, http.StatusOK, status)
	assert.Nil(t, body.Rerank)
	assert.Equal(t, 0, f.reranker.calls, "no message text leaves the daemon without a request for it")
}

func TestHandleSearchRerankProviderFailureKeepsRetrievalOrder(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	f := newRerankSearchFixture(t, true, vector.RerankConfig{Enabled: true})
	f.reranker.err = &rerank.Error{Category: rerank.FailureProviderStatus, Status: 502}

	status, body, _ := f.search(t, "q=invoice&mode=vector&rerank=true&explain=true")
	require.Equal(http.StatusOK, status)
	assert.Equal([]int64{41, 42, 43}, resultIDs(body))
	require.NotNil(body.Rerank)
	assert.False(body.Rerank.Applied)
	assert.Equal(rerank.FailureProviderStatus, body.Rerank.Fallback)
	assert.Nil(body.Results[0].Score.Rerank)
}

func TestHandleSearchRerankRejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		withStage bool
		query     string
		status    int
		code      string
	}{
		{"no stage", false, "q=invoice&mode=hybrid&rerank=true", http.StatusServiceUnavailable, "rerank_unavailable"},
		{"fts mode", true, "q=invoice&mode=fts&rerank=true", http.StatusBadRequest, "rerank_unsupported_mode"},
		{"fts default mode", true, "q=invoice&rerank=true", http.StatusBadRequest, "rerank_unsupported_mode"},
		{"not a boolean", true, "q=invoice&mode=hybrid&rerank=maybe", http.StatusBadRequest, "invalid_rerank"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newRerankSearchFixture(t, tt.withStage, vector.RerankConfig{Enabled: tt.withStage})
			status, _, errResp := f.search(t, tt.query)
			assert.Equal(t, tt.status, status)
			assert.Equal(t, tt.code, errResp.Error)
			assert.Equal(t, 0, f.reranker.calls)
		})
	}
}

func TestHandleSearchRerankDefaultWithoutStageDoesNotFail(t *testing.T) {
	t.Parallel()
	// [vector.rerank] is on with default=true, but the stage failed to start
	// (for example, a missing credential). Searches keep working unranked.
	f := newRerankSearchFixture(t, false, vector.RerankConfig{Enabled: true, Default: true})

	status, body, _ := f.search(t, "q=invoice&mode=vector")
	require.Equal(t, http.StatusOK, status)
	assert.Nil(t, body.Rerank)
	assert.Equal(t, []int64{41, 42, 43}, resultIDs(body))
}
