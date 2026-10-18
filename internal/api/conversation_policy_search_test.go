package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
)

func TestConversationPolicyIncludeColdNeverReranksCold(t *testing.T) {
	srv, _ := newTestServerWithMockStore(t)
	for _, mode := range []string{"vector", "hybrid"} {
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=needle%20include:cold&mode="+mode, nil))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "lexical")
	}
}

func TestHandleSearchIncludeColdUsesStructuredLexicalSearch(t *testing.T) {
	srv, st := newTestServerWithMockStore(t)
	assert := assert.New(t)
	require := require.New(t)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/api/v1/search?q=needle%20include:cold&mode=fts", nil,
	))

	require.Equal(http.StatusOK, response.Code, response.Body.String())
	assert.Zero(st.searchMessagesCalls.Load(), "include:cold must not enter plain-text search")
	assert.Equal(int32(1), st.searchMessagesQueryCalls.Load(), "include:cold must use structured lexical search")
	require.NotNil(st.searchMessagesQueryLast)
	assert.True(st.searchMessagesQueryLast.IncludeCold)
	assert.Equal([]string{"needle"}, st.searchMessagesQueryLast.TextTerms)
}

func TestConversationPolicyIncludeColdExploreDefinition(t *testing.T) {
	for _, mode := range []string{"semantic", "hybrid"} {
		_, failure := validateExploreSearchDefinition(ExploreHTTPRequest{Query: "needle include:cold", SearchMode: mode}, query.Context{})
		require.NotNil(t, failure)
		assert.Contains(t, failure.message, "lexical")
	}
	assert.NotEqual(t, exploreCanonicalQueryHash("needle", "full_text"), exploreCanonicalQueryHash("needle include:cold", "full_text"))
}

func TestConversationPolicyIncludeColdDeletionSelectionRefused(t *testing.T) {
	calls := 0
	engine := &querytest.MockEngine{
		GetDeletionTargetsBySearchFunc: func(context.Context, *search.Query, query.MessageFilter, query.DeletionSearchMode) ([]query.DeletionTarget, error) {
			calls++
			return nil, errors.New("selector should not run")
		},
		GetDeletionTargetsByAggregateSearchFunc: func(context.Context, string, query.MessageFilter, query.ViewType, string) ([]query.DeletionTarget, error) {
			calls++
			return nil, errors.New("selector should not run")
		},
	}
	srv := newTestServerWithEngine(t, engine)
	for _, mode := range []string{"fast", "deep", "aggregate"} {
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/messages/gmail-ids?q=needle%20include:cold&search_mode="+mode+"&view_type=senders&aggregate_key=sender@example.test", nil))
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "lexical")
	}
	assert.Zero(t, calls)
}
