package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type collectionTestEngine struct {
	query.Engine
	collections []query.CollectionScope
}

func (e *collectionTestEngine) ListCollectionScopes(context.Context) ([]query.CollectionScope, error) {
	return e.collections, nil
}

func TestCollectionScopesFilterRealMessageReads(t *testing.T) {
	f := storetest.New(t)
	other, err := f.Store.GetOrCreateSource("imap", "other@example.test")
	require.NoError(t, err)
	first := f.NewMessage().WithSubject("collection fixture").Create(t, f.Store)
	second := f.NewMessage().WithSourceMessageID("other").WithSubject("collection fixture").Create(t, f.Store)
	_, err = f.Store.DB().ExecContext(t.Context(), "UPDATE messages SET source_id = ? WHERE id = ?", other.ID, second)
	require.NoError(t, err)
	e := &collectionTestEngine{Engine: query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL()), collections: []query.CollectionScope{{Name: "one", SourceIDs: []int64{f.Source.ID}}, {Name: "empty", SourceIDs: []int64{}}}}
	h := newTestHandlers(e)
	h.collectionScopes = true
	for _, tc := range []struct {
		name string
		want []int64
	}{{"one", []int64{first}}, {"empty", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			got := runTool[listMessagesResponse](t, ToolListMessages, h.listMessages, map[string]any{"collection": tc.name})
			ids := make([]int64, 0, len(got.Data))
			for _, m := range got.Data {
				ids = append(ids, m.ID)
			}
			assert.ElementsMatch(t, tc.want, ids)
			found := runTool[paginatedSearchMessages](t, ToolSearchMetadata, h.searchMetadata, map[string]any{"query": "subject:collection", "collection": tc.name})
			ids = ids[:0]
			for _, m := range found.Data {
				ids = append(ids, m.ID)
			}
			assert.ElementsMatch(t, tc.want, ids)
		})
	}
	_, err = h.resolveReadSelection(t.Context(), "account", map[string]any{"collection": "one"})
	require.ErrorContains(t, err, "mutually exclusive")
	_, err = h.resolveReadSelection(t.Context(), "", map[string]any{"collection": "missing"})
	require.ErrorContains(t, err, "collection not found")
}

func TestCollectionCatalogAvailability(t *testing.T) {
	e := &collectionTestEngine{Engine: query.NewSQLiteEngine(nil), collections: []query.CollectionScope{{Name: "empty", SourceIDs: []int64{}}}}
	searcher := hybridSearcherFunc(func(context.Context, HybridSearchRequest) (*HybridSearchResult, error) { return nil, nil })
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
			tools := toolsByName(t, rawListTools(t, tc.opts, false))
			if tc.available {
				assert.Contains(t, tools, ToolListMessageCollections)
			} else {
				assert.NotContains(t, tools, ToolListMessageCollections)
			}
			names := []string{ToolAggregate, ToolGetMessage, ToolListMessages, ToolSearchMessageBodies, ToolSearchMessages, ToolSearchMetadata}
			if !tc.delegated {
				names = append(names, ToolSemanticSearchMessages)
			}
			for _, name := range names {
				require.Contains(t, tools, name)
				if tc.available {
					assert.Contains(t, toolPropertyNames(t, tools[name]), "collection", name)
				} else {
					assert.NotContains(t, toolPropertyNames(t, tools[name]), "collection", name)
				}
			}
		})
	}
}

func TestSemanticCollectionUsesOneResolvedSource(t *testing.T) {
	e := &collectionTestEngine{Engine: &querytest.MockEngine{}, collections: []query.CollectionScope{{Name: "one", SourceIDs: []int64{42}}, {Name: "empty", SourceIDs: []int64{}}, {Name: "many", SourceIDs: []int64{42, 43}}}}
	h := newTestHandlers(e)
	h.collectionScopes = true
	calls := 0
	h.hybridSearcher = hybridSearcherFunc(func(_ context.Context, req HybridSearchRequest) (*HybridSearchResult, error) {
		calls++
		require.NotNil(t, req.SourceID)
		assert.Equal(t, int64(42), *req.SourceID)
		return &HybridSearchResult{}, nil
	})
	runTool[searchMessageBodiesResponse](t, ToolSemanticSearchMessages, h.semanticSearchMessages, map[string]any{"query": "fixture", "collection": "one"})
	runTool[searchMessageBodiesResponse](t, ToolSemanticSearchMessages, h.semanticSearchMessages, map[string]any{"query": "fixture", "collection": "empty"})
	assert.Equal(t, 1, calls)
	result, err := h.semanticSearchMessages(t.Context(), toolRequest{arguments: map[string]any{"query": "fixture", "collection": "many"}})
	require.NoError(t, err)
	assert.True(t, result.isError)
	assert.Equal(t, 1, calls)
	result, err = h.semanticSearchMessages(t.Context(), toolRequest{arguments: map[string]any{"query": "fixture", "collection": ""}})
	require.NoError(t, err)
	assert.True(t, result.isError)
	assert.Equal(t, 1, calls)
	result, err = h.semanticSearchMessages(t.Context(), toolRequest{arguments: map[string]any{"query": "subject:fixture", "collection": "empty"}})
	require.NoError(t, err)
	assert.True(t, result.isError)
	assert.Contains(t, resultText(t, result), "missing_free_text")
	assert.Equal(t, 1, calls)
}
