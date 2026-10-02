package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

func TestVirtualAccountMCPFiltersAndFallback(t *testing.T) {
	source := int64(7)
	scope := search.AccountScope{SourceID: &source, Addresses: []string{"work@example.org"}}
	account := store.VirtualIdentityKey(source, "work@example.org")
	for _, operation := range []string{"metadata", "body", "stats", "deletion-fallback", "deletion-filter"} {
		t.Run(operation, func(t *testing.T) {
			called := false
			engine := &querytest.MockEngine{Accounts: []query.AccountInfo{{ID: source, Identifier: "inbox@example.net"}}}
			engine.SearchFastFunc = func(_ context.Context, q *search.Query, f query.MessageFilter, _, _ int) ([]query.MessageSummary, error) {
				if operation == "metadata" {
					called = true
					assert.Equal(t, []search.AccountScope{scope}, f.AccountScopes)
					require.NotNil(t, f.SourceID)
					assert.Equal(t, source, *f.SourceID)
				}
				return nil, nil
			}
			engine.SearchMessageBodiesFunc = func(_ context.Context, q *search.Query, _, _ int) ([]query.MessageSummary, error) {
				called = true
				assert.Equal(t, []search.AccountScope{scope}, q.AccountScopes)
				return nil, nil
			}
			engine.SearchFunc = func(_ context.Context, q *search.Query, _, _ int) ([]query.MessageSummary, error) {
				called = true
				assert.Equal(t, []search.AccountScope{scope}, q.AccountScopes)
				return nil, nil
			}
			engine.GetTotalStatsFunc = func(_ context.Context, o query.StatsOptions) (*query.TotalStats, error) {
				called = true
				require.NotNil(t, o.Filter)
				assert.Equal(t, []search.AccountScope{scope}, o.Filter.AccountScopes)
				return &query.TotalStats{}, nil
			}
			engine.GetDeletionTargetsByFilterFunc = func(_ context.Context, f query.MessageFilter) ([]query.DeletionTarget, error) {
				called = true
				assert.Equal(t, []search.AccountScope{scope}, f.AccountScopes)
				return nil, nil
			}
			handlers := newTestHandlers(engine)
			args := map[string]any{"account": account, "query": "needle"}
			switch operation {
			case "metadata":
				callToolDirect(t, "search_metadata", handlers.searchMetadata, args)
			case "body":
				callToolDirect(t, "search_message_bodies", handlers.searchMessageBodies, args)
			case "stats":
				callToolDirect(t, "get_stats", handlers.getStats, args)
			case "deletion-fallback":
				callToolDirect(t, "stage_deletion", handlers.stageDeletion, args)
			case "deletion-filter":
				delete(args, "query")
				args["from"] = "sender@example.test"
				callToolDirect(t, "stage_deletion", handlers.stageDeletion, args)
			}
			assert.True(t, called, "the production handler must pass the scope to its engine")
		})
	}
}

type sourceAccountResolutionEngine struct {
	query.Engine

	sources      []query.AccountInfo
	catalogCalls int
}

func (e *sourceAccountResolutionEngine) ListSourceAccounts(context.Context) ([]query.AccountInfo, error) {
	return e.sources, nil
}

func (e *sourceAccountResolutionEngine) ListAccounts(context.Context) ([]query.AccountInfo, error) {
	e.catalogCalls++
	return nil, errors.New("virtual totals must not be loaded for source resolution")
}

func TestAccountResolutionSkipsVirtualTotalsForPhysicalAndPinnedKeys(t *testing.T) {
	for _, account := range []string{"inbox@example.net", store.VirtualIdentityKey(7, "work@example.org"), "unattributed:7"} {
		t.Run(account, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			e := &sourceAccountResolutionEngine{sources: []query.AccountInfo{{ID: 7, Identifier: "inbox@example.net"}}}
			h := newTestHandlers(e)
			source, _, err := h.resolveAccountScope(t.Context(), account)
			require.NoError(err)
			require.NotNil(source)
			assert.Equal(int64(7), *source)
			assert.Zero(e.catalogCalls)
		})
	}
}

func TestVirtualAccountSimilarSearcherReceivesPinnedScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := int64(7)
	var got SimilarSearchRequest
	h := newTestHandlers(&querytest.MockEngine{Accounts: []query.AccountInfo{{ID: source, Identifier: "inbox@example.net"}}})
	h.similarSearcher = similarSearcherFunc(func(_ context.Context, req SimilarSearchRequest) (*SimilarSearchResult, error) {
		got = req
		return &SimilarSearchResult{SeedMessageID: req.MessageID}, nil
	})
	callToolDirect(t, ToolFindSimilarMessages, h.findSimilarMessages, map[string]any{"message_id": float64(11), "account": store.VirtualIdentityKey(source, "work@example.org")})
	assert.Equal(int64(11), got.MessageID)
	assert.Empty(got.Account)
	require.Len(got.AccountScopes, 1)
	assert.Equal([]search.AccountScope{{SourceID: &source, Addresses: []string{"work@example.org"}}}, got.AccountScopes)
}

func TestSimilarAccountResolutionPreservesSourceAndPhysicalSemantics(t *testing.T) {
	for _, account := range []string{"work@example.org", "Inbox@Example.net", "primary inbox", " inbox@example.net "} {
		t.Run(account, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			source := int64(7)
			var got SimilarSearchRequest
			called := false
			engine := &querytest.MockEngine{Accounts: []query.AccountInfo{
				{ID: source, Identifier: "inbox@example.net", DisplayName: "Primary Inbox", VirtualAccounts: []store.VirtualAccount{{SourceID: source, AccountAddress: "work@example.org"}, {SourceID: source, AccountAddress: "inbox@example.net"}}},
				{ID: 9, Identifier: "other@example.net", VirtualAccounts: []store.VirtualAccount{{SourceID: 9, Group: "fastmail-masked:other@example.net"}}},
			}}
			h := newTestHandlers(engine)
			h.similarSearcher = similarSearcherFunc(func(_ context.Context, req SimilarSearchRequest) (*SimilarSearchResult, error) {
				called, got = true, req
				return &SimilarSearchResult{SeedMessageID: req.MessageID}, nil
			})
			callToolDirect(t, ToolFindSimilarMessages, h.findSimilarMessages, map[string]any{"message_id": float64(11), "account": account})
			require.True(called)
			assert.Equal(int64(11), got.MessageID)
			if account == "work@example.org" {
				assert.Empty(got.Account)
				assert.Equal([]search.AccountScope{{SourceID: &source, Addresses: []string{"work@example.org"}}}, got.AccountScopes)
			} else {
				assert.Equal(account, got.Account, "the daemon resolves the entire physical source")
				assert.Empty(got.AccountScopes, "physical scope must not narrow to messages attributed to its email")
			}
		})
	}
}
