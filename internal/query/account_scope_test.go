package query_test

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAccountScopesOnLiveEngine(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	for _, address := range []string{"work@example.org", "other@example.org"} {
		require.NoError(st.AddAccountIdentity(f.Source.ID, address, "manual"))
	}
	persist := func(key, raw string) int64 {
		id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message: &store.Message{
				SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: key, MessageType: "email",
				SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
			},
			RawMIME: []byte(raw),
		})
		require.NoError(err)
		return id
	}
	work := persist("work", "X-Delivered-To: work@example.org\r\n\r\nbody")
	other := persist("other", "X-Delivered-To: other@example.org\r\n\r\nbody")
	conflict := persist("conflict", "X-Original-To: work@example.org, other@example.org\r\n\r\nbody")
	pending := persist("pending", "X-Delivered-To: work@example.org\r\n\r\nbody")
	// Setup only: a row an older msgvault archived before attribution existed.
	_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET account_address = NULL, account_path = NULL WHERE id = ?`), pending)
	require.NoError(err)

	engine := query.NewSQLiteEngine(st.DB())
	if st.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(st.DB(), query.PostgreSQLQueryDialect{})
	}
	source := f.Source.ID
	listed := func(scopes ...search.AccountScope) []int64 {
		rows, err := engine.ListMessages(t.Context(), query.MessageFilter{AccountScopes: scopes, Pagination: query.Pagination{Limit: 100}})
		require.NoError(err)
		ids := make([]int64, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		slices.Sort(ids)
		searched, err := engine.SearchFastCount(t.Context(), &search.Query{}, query.MessageFilter{AccountScopes: scopes})
		require.NoError(err)
		assert.Equal(int64(len(ids)), searched, "SearchFastCount agrees with ListMessages")
		return ids
	}

	assert.Equal([]int64{work}, listed(search.AccountScope{SourceID: &source, Addresses: []string{"work@example.org"}}))
	assert.Equal([]int64{conflict, pending}, listed(search.AccountScope{SourceID: &source, Unattributed: true}),
		"unattributed includes rows still pending repair")
	assert.Equal([]int64{conflict}, listed(search.AccountScope{Unattributed: true, Inbound: true}),
		"received-only unattributed excludes pending rows")
	assert.Equal([]int64{work, other}, listed(search.AccountScope{Addresses: []string{"work@example.org", "other@example.org"}}))
	assert.Equal([]int64{work}, listed(
		search.AccountScope{Addresses: []string{"work@example.org", "other@example.org"}},
		search.AccountScope{Addresses: []string{"work@example.org"}},
	), "separate scopes intersect")

	rows, err := engine.Aggregate(t.Context(), query.ViewTime, query.AggregateOptions{
		AccountScopes: []search.AccountScope{{Unattributed: true}}, Limit: 10,
	})
	require.NoError(err)
	var total int64
	for _, row := range rows {
		total += row.Count
	}
	assert.Equal(int64(2), total, "aggregates honor account scopes")

	catalog, err := st.ListVirtualAccountsContext(t.Context())
	require.NoError(err)
	byKey := map[string]store.VirtualAccount{}
	for _, v := range catalog[source] {
		byKey[v.Key] = v
	}
	assert.Equal(int64(1), byKey[store.VirtualIdentityKey(source, "work@example.org")].MessageCount)
	assert.Equal(int64(1), byKey[store.VirtualIdentityKey(source, "other@example.org")].MessageCount)
	unattributed := byKey[store.VirtualUnattributedKey(source)]
	assert.True(unattributed.Unattributed)
	assert.Equal(int64(2), unattributed.MessageCount)
	assert.Equal(int64(1), unattributed.PendingCount)

	lister, ok := any(engine).(query.VirtualAccountLister)
	require.True(ok)
	viaEngine, err := lister.ListVirtualAccounts(t.Context())
	require.NoError(err)
	assert.Equal(catalog, viaEngine)
}
