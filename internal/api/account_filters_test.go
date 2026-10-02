package api

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

func TestAccountFiltersValidationAndTotals(t *testing.T) {
	for _, param := range []string{"account_addresses=bad", "account_addresses=work%40example.org&account_unattributed=true", "account_groups=not-a-group", "account_unattributed=yes", "account_scopes=%5B%7B%22unknown%22%3Atrue%7D%5D"} {
		t.Run(param, func(t *testing.T) {
			assert := assert.New(t)
			_, err := parseMessageFilter(httptest.NewRequest(http.MethodGet, "/?"+param, nil))
			assert.Error(err)
		})
	}
	assert := assert.New(t)
	require := require.New(t)
	var captured query.StatsOptions
	engine := &querytest.MockEngine{GetTotalStatsFunc: func(_ context.Context, o query.StatsOptions) (*query.TotalStats, error) {
		captured = o
		return &query.TotalStats{MessageCount: 3}, nil
	}}
	srv := newTestServerWithEngine(t, engine)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/stats/total?account_addresses=Work%40example.org&source_id=7", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	require.Equal(http.StatusOK, w.Code, w.Body.String())
	require.NotNil(captured.Filter)
	require.Len(captured.Filter.AccountScopes, 1)
	assert.Equal([]string{"work@example.org"}, captured.Filter.AccountScopes[0].Addresses)
	require.NotNil(captured.SourceID)
	assert.Equal(int64(7), *captured.SourceID)
}

func TestAccountScopeTransportIsAnIntersection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	encoded := `[{"addresses":["work@example.org"]},{"groups":["fastmail-masked:inbox@example.net"]}]`
	f, err := parseMessageFilter(httptest.NewRequest(http.MethodGet, "/?account_scopes="+url.QueryEscape(encoded), nil))
	require.NoError(err)
	assert.Equal([]search.AccountScope{{Addresses: []string{"work@example.org"}}, {Groups: []string{"fastmail-masked:inbox@example.net"}}}, f.AccountScopes)
}

func TestAccountScopeSearchFTSRejectsStructuredTransport(t *testing.T) {
	for _, mode := range []string{"", "&mode=fts"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv, _ := newTestServerWithMockStore(t)
			encoded := `[{"addresses":["work@example.org"]}]`
			r := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=report"+mode+"&account_scopes="+url.QueryEscape(encoded), nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			require.Equal(http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(w.Body.String(), `"error":"unsupported_filter_mode"`)
			assert.Contains(w.Body.String(), "account_scopes")
		})
	}
}

func TestAccountScopeExploreVirtualGroupIsSourcePinned(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := int64(7)
	key := "group:7:" + base64.RawURLEncoding.EncodeToString([]byte("fastmail-masked:inbox@example.net"))
	ctx, err := exploreContext([]ExploreFilter{{Dimension: "source", Values: []string{"9"}}, {Dimension: "account", Values: []string{key}}})
	require.NoError(err)
	assert.Equal([]int64{9}, ctx.SourceIDs)
	assert.Equal([]search.AccountScope{{SourceID: &source, Groups: []string{"fastmail-masked:inbox@example.net"}}}, ctx.AccountScopes)
	parsed := search.Parse("received:work@example.org")
	assert.True(applyLexicalFilterPushdown(parsed, ctx))
	assert.Len(parsed.AccountScopes, 2, "query and picker filters intersect before candidate retrieval")
}

type sourceMetadataOnlyStore struct {
	*mockStore

	countCalls atomic.Int32
}

func (s *sourceMetadataOnlyStore) ListSources(string) ([]*store.Source, error) {
	return []*store.Source{{ID: 7, Identifier: "inbox@example.net", SourceType: "imap"}}, nil
}

func (s *sourceMetadataOnlyStore) ListSourcesContext(context.Context, string) ([]*store.Source, error) {
	return s.ListSources("")
}

func (s *sourceMetadataOnlyStore) CountMessagesForSource(int64) (int64, error) {
	s.countCalls.Add(1)
	return 0, errors.New("source metadata must not count messages")
}

func (s *sourceMetadataOnlyStore) ListVirtualAccountsContext(context.Context) (map[int64][]store.VirtualAccount, error) {
	s.countCalls.Add(1)
	return nil, errors.New("source metadata must not aggregate virtual totals")
}

func TestSourceAccountEndpointSkipsArchiveTotals(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := &sourceMetadataOnlyStore{mockStore: &mockStore{}}
	srv := NewServer(&config.Config{}, st, nil, testLogger())
	request := httptest.NewRequest(http.MethodGet, "/api/v1/cli/source-accounts", nil)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var got cliSourceAccountsResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &got))
	require.Len(got.Accounts, 1)
	assert.Equal(int64(7), got.Accounts[0].ID)
	assert.Equal("inbox@example.net", got.Accounts[0].Email)
	assert.Equal("imap", got.Accounts[0].Type)
	assert.Zero(st.countCalls.Load())
	assert.NotContains(response.Body.String(), "message_count")
	assert.NotContains(response.Body.String(), "virtual_accounts")
}

func (s *sourceMetadataOnlyStore) CountMessagesBySourceContext(context.Context) (map[int64]store.SourceMessageCounts, error) {
	s.countCalls.Add(1)
	return nil, errors.New("source metadata must not count the archive")
}

func TestSimilarHTTPAccountScopesReachVectorFilter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := int64(7)
	scopes := []search.AccountScope{{SourceID: &source, Addresses: []string{"work@example.org"}}}
	cfg := vector.Config{Embeddings: vector.EmbeddingsConfig{Model: "fake", Dimension: 4}}
	backend := &fakeVectorBackend{active: &vector.Generation{ID: 1, Model: "fake", Dimension: 4, Fingerprint: cfg.GenerationFingerprint(), State: vector.GenerationActive}, loadVec: []float32{1, 0, 0, 0}}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, VectorCfg: cfg, Backend: backend, Logger: testLogger()})
	encoded, err := json.Marshal(scopes)
	require.NoError(err)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/search/similar?message_id=11&account_scopes="+url.QueryEscape(string(encoded)), nil)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Equal(scopes, backend.searchFilter.AccountScopes)
	bad := httptest.NewRecorder()
	srv.Router().ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/api/v1/search/similar?message_id=11&account_scopes=invalid", nil))
	assert.Equal(http.StatusBadRequest, bad.Code)
}
