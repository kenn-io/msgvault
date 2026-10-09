package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSearchMetadataFindsMailByReceivingAccount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "work@example.org", "manual"))
	forwarded := f.NewMessage().WithSourceMessageID("forwarded").WithSubject("forwarded").Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(forwarded, []byte("X-Delivered-To: work@example.org\r\nTo: list@example.com\r\n\r\nbody")))
	f.NewMessage().WithSourceMessageID("other").WithSubject("other").Create(t, f.Store)

	engine := query.NewSQLiteEngine(f.Store.DB())
	if f.Store.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	}
	resp := runTool[paginatedSearchMessages](t, "search_metadata", newTestHandlers(engine).searchMetadata,
		map[string]any{"query": "received:work@example.org"})
	require.Len(resp.Data, 1)
	assert.Equal(forwarded, resp.Data[0].ID)
}

func TestAccountArgumentSelectsVirtualAccounts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "work@example.org", "manual"))
	forwarded := f.NewMessage().WithSourceMessageID("forwarded").WithSubject("invoice").Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(forwarded, []byte("X-Delivered-To: work@example.org\r\n\r\nbody")))
	conflict := f.NewMessage().WithSourceMessageID("conflict").WithSubject("invoice").Create(t, f.Store)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "other@example.org", "manual"))
	require.NoError(f.Store.UpsertMessageRaw(conflict, []byte("X-Original-To: work@example.org, other@example.org\r\n\r\nbody")))

	engine := query.NewSQLiteEngine(f.Store.DB())
	if f.Store.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	}
	h := newTestHandlers(engine)
	ids := func(account string) []int64 {
		resp := runTool[paginatedSearchMessages](t, "search_metadata", h.searchMetadata,
			map[string]any{"query": "subject:invoice", "account": account})
		out := make([]int64, 0, len(resp.Data))
		for _, row := range resp.Data {
			out = append(out, row.ID)
		}
		return out
	}
	assert.Equal([]int64{forwarded}, ids(store.VirtualIdentityKey(f.Source.ID, "work@example.org")))
	assert.Equal([]int64{conflict}, ids(store.VirtualUnattributedKey(f.Source.ID)))
	assert.Equal([]int64{forwarded}, ids("work@example.org"), "an exact address selects its account on every source")
	_, err := h.resolveAccount(t.Context(), "wrok@example.org")
	require.ErrorContains(err, "account not found", "a mistyped address keeps the old error")
	assert.ElementsMatch([]int64{forwarded, conflict}, ids(f.Source.Identifier), "a source identifier keeps its old meaning")

	stats := runTool[getStatsResponse](t, "get_stats", h.getStats, map[string]any{})
	require.Len(stats.Accounts, 1)
	keys := make([]string, 0, len(stats.Accounts[0].VirtualAccounts))
	for _, v := range stats.Accounts[0].VirtualAccounts {
		keys = append(keys, v.Key)
	}
	assert.Contains(keys, store.VirtualIdentityKey(f.Source.ID, "work@example.org"))
	assert.Contains(keys, store.VirtualUnattributedKey(f.Source.ID))
}

func TestForwardedAccountResolvesAddressesAndKeys(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	engine := &querytest.MockEngine{
		Accounts:        []query.AccountInfo{{ID: 1, Identifier: "alice@example.com"}},
		VirtualAccounts: map[int64][]store.VirtualAccount{1: {{SourceID: 1, AccountAddress: "work@example.org"}}},
	}
	h := &handlers{engine: engine}
	source := int64(1)
	for account, want := range map[string]struct {
		name   string
		scopes []search.AccountScope
	}{
		"alice@example.com": {name: "alice@example.com"},
		"work@example.org":  {scopes: []search.AccountScope{{Addresses: []string{"work@example.org"}}}},
		store.VirtualIdentityKey(1, "work@example.org"): {scopes: []search.AccountScope{{SourceID: &source, Addresses: []string{"work@example.org"}}}},
	} {
		name, scopes, err := h.resolveForwardedAccount(t.Context(), account)
		require.NoError(err, account)
		assert.Equal(want.name, name, account)
		assert.Equal(want.scopes, scopes, account)
	}
}

func TestAccountSelectionIntersectsQueryAccounts(t *testing.T) {
	assert := assert.New(t)
	selection := accountSelection{scope: &search.AccountScope{Addresses: []string{"work@example.org"}}}
	q := search.Parse("invoice account:work@example.org account:other@example.org")
	require.NoError(t, selection.applyToQuery(q))
	assert.Equal([]string{"work@example.org"}, q.AccountAddrs)
	assert.ErrorIs(selection.applyToQuery(search.Parse("invoice account:other@example.org")), errAccountSelectionConflict)
}

func TestUnattributedAccountNarrowsTextQueries(t *testing.T) {
	assert := assert.New(t)
	source := int64(1)
	var bodyQuery, fastQuery *search.Query
	engine := &querytest.MockEngine{
		Accounts: []query.AccountInfo{{ID: 1, Identifier: "alice@example.com", SourceType: "gmail"}},
		SearchMessageBodiesFunc: func(_ context.Context, q *search.Query, _, _ int) ([]query.MessageSummary, error) {
			bodyQuery = q
			return nil, nil
		},
		SearchFastFunc: func(_ context.Context, q *search.Query, _ query.MessageFilter, _, _ int) ([]query.MessageSummary, error) {
			fastQuery = q
			return []query.MessageSummary{{ID: 100, SourceID: 1, SourceMessageID: "m100"}}, nil
		},
	}
	h := &handlers{engine: engine, dataDir: t.TempDir()}
	key := store.VirtualUnattributedKey(1)

	runTool[paginatedSearchMessages](t, "search_message_bodies", h.searchMessageBodies, map[string]any{"query": "invoice", "account": key})
	runTool[stageDeletionResponse](t, "stage_deletion", h.stageDeletion, map[string]any{"query": "invoice", "account": key})

	want := []search.AccountScope{{SourceID: &source, Unattributed: true}}
	if assert.NotNil(bodyQuery) {
		assert.Equal(want, bodyQuery.AccountScopes)
	}
	if assert.NotNil(fastQuery) {
		assert.Equal(want, fastQuery.AccountScopes)
	}
}
