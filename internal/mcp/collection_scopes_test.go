package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type collectionTestEngine struct {
	query.Engine

	collections []query.CollectionScope
	listCalls   int
}

func (e *collectionTestEngine) ListCollectionScopes(context.Context) ([]query.CollectionScope, error) {
	e.listCalls++
	return e.collections, nil
}

func (e *collectionTestEngine) GetCollectionScope(_ context.Context, name string) (query.CollectionScope, error) {
	for _, scope := range e.collections {
		if scope.Name == name {
			return scope, nil
		}
	}
	return query.CollectionScope{}, store.ErrCollectionNotFound
}

func TestCollectionScopesFilterRealMessageReads(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	other, err := f.Store.GetOrCreateSource("imap", "other@example.test")
	require.NoError(err)
	first := f.NewMessage().WithSubject("collection fixture").Create(t, f.Store)
	convID, err := f.Store.EnsureConversation(t.Context(), other.ID, "other-thread", "Other Thread")
	require.NoError(err)
	storetest.NewMessage(other.ID, convID).WithSourceMessageID("other").WithSubject("collection fixture").Create(t, f.Store)
	e := &collectionTestEngine{Engine: query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL()), collections: []query.CollectionScope{{Name: "one", SourceIDs: []int64{f.Source.ID}}, {Name: "empty", SourceIDs: []int64{}}}}
	h := newTestHandlers(e)
	for _, tc := range []struct {
		name string
		want []int64
	}{{"one", []int64{first}}, {"empty", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			got := runTool[listMessagesResponse](t, ToolListMessages, h.listMessages, map[string]any{"collection": tc.name})
			ids := make([]int64, 0, len(got.Data))
			for _, m := range got.Data {
				ids = append(ids, m.ID)
			}
			assert.ElementsMatch(tc.want, ids)
			found := runTool[paginatedSearchMessages](t, ToolSearchMetadata, h.searchMetadata, map[string]any{"query": "subject:collection", "collection": tc.name})
			ids = ids[:0]
			for _, m := range found.Data {
				ids = append(ids, m.ID)
			}
			assert.ElementsMatch(tc.want, ids)
		})
	}
	_, err = h.resolveReadSelection(t.Context(), "account", map[string]any{"collection": "one"})
	require.ErrorContains(err, "mutually exclusive")
	_, err = h.resolveReadSelection(t.Context(), "", map[string]any{"collection": ""})
	require.ErrorContains(err, "collection name is required")
	_, err = h.resolveReadSelection(t.Context(), "", map[string]any{"collection": "missing"})
	require.ErrorContains(err, "collection not found")
	assertions.Zero(e.listCalls)
}

func TestCollectionCatalogAvailability(t *testing.T) {
	e := &collectionTestEngine{Engine: query.NewSQLiteEngine(nil), collections: []query.CollectionScope{{Name: "empty", SourceIDs: []int64{}}}}
	searcher := hybridSearcherFunc(func(context.Context, HybridSearchRequest) (*HybridSearchResult, error) {
		return &HybridSearchResult{}, nil
	})
	for _, tc := range []struct {
		name      string
		opts      ServeOptions
		available bool
		delegated bool
	}{
		{"owner", ServeOptions{Engine: e, CollectionScopesAvailable: true, HybridSearcher: searcher}, true, false},
		{"older daemon", ServeOptions{Engine: e, HybridSearcher: searcher}, false, false},
		{"no lister", ServeOptions{Engine: &querytest.MockEngine{}, CollectionScopesAvailable: true, HybridSearcher: searcher}, false, false},
		{"delegated", ServeOptions{Engine: e, CollectionScopesAvailable: true, HybridSearcher: searcher, DelegatedOnly: true, GrantPermissions: []string{string(agentgrant.PermissionSearchRead), string(agentgrant.PermissionMessageRead)}}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			tools := toolsByName(t, rawListTools(t, tc.opts, false))
			if tc.available {
				assert.Contains(tools, ToolListMessageCollections)
			} else {
				assert.NotContains(tools, ToolListMessageCollections)
			}
			names := []string{ToolAggregate, ToolGetMessage, ToolListMessages, ToolSearchMessageBodies, ToolSearchMessages, ToolSearchMetadata}
			if !tc.delegated {
				names = append(names, ToolSemanticSearchMessages)
			}
			for _, name := range names {
				require.Contains(tools, name)
				if tc.available {
					assert.Contains(toolPropertyNames(t, tools[name]), "collection", name)
				} else {
					assert.NotContains(toolPropertyNames(t, tools[name]), "collection", name)
				}
			}
		})
	}
}

func TestSemanticCollectionUsesAllSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	e := &collectionTestEngine{Engine: &querytest.MockEngine{}, collections: []query.CollectionScope{{Name: "one", SourceIDs: []int64{42}}, {Name: "empty", SourceIDs: []int64{}}, {Name: "many", SourceIDs: []int64{42, 43}}}}
	h := newTestHandlers(e)
	calls := 0
	h.hybridSearcher = hybridSearcherFunc(func(_ context.Context, req HybridSearchRequest) (*HybridSearchResult, error) {
		calls++
		assert.Equal("many", req.Collection)
		return &HybridSearchResult{}, nil
	})
	runTool[searchMessageBodiesResponse](t, ToolSemanticSearchMessages, h.semanticSearchMessages, map[string]any{"query": "fixture", "collection": "many"})
	runTool[searchMessageBodiesResponse](t, ToolSearchMessages, h.searchMessages, map[string]any{"query": "fixture", "mode": "hybrid", "collection": "many"})
	runTool[searchMessageBodiesResponse](t, ToolSemanticSearchMessages, h.semanticSearchMessages, map[string]any{"query": "fixture", "collection": "empty"})
	assert.Equal(2, calls)
	result, err := h.semanticSearchMessages(t.Context(), toolRequest{arguments: map[string]any{"query": "subject:fixture", "collection": "empty"}})
	require.NoError(err)
	assert.True(result.isError)
	assert.Contains(resultText(t, result), "missing_free_text")
	assert.Equal(2, calls)
}
