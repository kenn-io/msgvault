package query_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestEmptyDuckDBAccountViews(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	engine, err := query.NewDuckDBEngine("", "", nil)
	require.NoError(err)
	t.Cleanup(func() { _ = engine.Close() })
	for _, stmt := range []string{
		"SELECT id, source_id, account_address, account_path, account_attribution_basis FROM message_accounts",
		"SELECT source_id, group_key, address_key FROM account_identity_group_memberships",
	} {
		result, err := engine.QuerySQL(t.Context(), stmt)
		require.NoError(err)
		assert.Empty(result.Rows)
	}
}

func TestVirtualAccountFiltersAndTotals(t *testing.T) {
	f := storetest.New(t)
	st := f.Store
	require.NoError(t, st.AddAccountIdentity(f.Source.ID, f.Source.Identifier, "manual"))
	require.NoError(t, st.AddAccountIdentity(f.Source.ID, "work@example.org", "manual"))
	require.NoError(t, st.AddAccountIdentity(f.Source.ID, "mask@example.org", "fastmail-masked-email"))
	var dialect query.Dialect = query.SQLiteQueryDialect{}
	if st.IsPostgreSQL() {
		dialect = query.PostgreSQLQueryDialect{}
	}
	e := query.NewEngineWithDialect(st.DB(), dialect)
	sentLabel, err := st.EnsureLabel(f.Source.ID, "SENT", "Sent", "system")
	require.NoError(t, err)
	makeMessage := func(key, header string, sent bool) int64 {
		var labels []int64
		if sent {
			labels = []int64{sentLabel}
		}
		id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: key, MessageType: "email", SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}, IsFromMe: sent}, RawMIME: []byte(header + "\r\n\r\nbody"), LabelIDs: labels})
		require.NoError(t, err)
		return id
	}
	inbound := makeMessage("work-inbound", "From: sender@example.test\r\nX-Delivered-To: work@example.org", false)
	outbound := makeMessage("work-sent", "From: work@example.org\r\nTo: other@example.net", true)
	mask := makeMessage("mask", "X-Delivered-To: mask@example.org", false)
	ambiguous := makeMessage("ambiguous", "X-Delivered-To: mask@example.org, work@example.org", false)
	for _, tc := range []struct {
		q    string
		want []int64
	}{
		{"account:work@example.org", []int64{inbound, outbound}},
		{"received:work@example.org", []int64{inbound}},
		{"account:work@example.org account:fastmail-masked:" + f.Source.Identifier, []int64{inbound, outbound, mask}},
		{"account:unattributed", []int64{ambiguous}},
		{"received:unattributed", []int64{ambiguous}},
		{"account:fastmail-masked:" + f.Source.Identifier + " received:work@example.org", nil},
	} {
		t.Run(tc.q, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			q := search.Parse(tc.q)
			require.NoError(q.Err())
			got, err := e.Search(t.Context(), q, 100, 0)
			require.NoError(err)
			var ids []int64
			for _, m := range got {
				ids = append(ids, m.ID)
			}
			assert.ElementsMatch(tc.want, ids)
			count, err := e.SearchFastCount(t.Context(), q, query.MessageFilter{})
			require.NoError(err)
			assert.Equal(int64(len(tc.want)), count)
			api, _, err := st.SearchMessagesQueryContext(t.Context(), q, 0, 100)
			require.NoError(err)
			assert.Len(api, len(tc.want))
		})
	}
	assert := assert.New(t)
	require := require.New(t)
	filter := query.MessageFilter{SourceID: &f.Source.ID, AccountScopes: []search.AccountScope{{Addresses: []string{"work@example.org"}}}}
	rows, err := e.ListMessages(t.Context(), filter)
	require.NoError(err)
	assert.Len(rows, 2)
	stats, err := e.GetTotalStats(t.Context(), query.StatsOptions{Filter: &filter})
	require.NoError(err)
	assert.Equal(int64(2), stats.MessageCount)
	noMatch := query.MergeFilterIntoQuery(search.Parse("account:mask@example.org"), filter)
	rows, err = e.Search(t.Context(), noMatch, 100, 0)
	require.NoError(err)
	assert.Empty(rows)
	accounts, err := e.ListAccounts(t.Context())
	require.NoError(err)
	require.Len(accounts, 1)
	var total int64
	for _, v := range accounts[0].VirtualAccounts {
		total += v.MessageCount
	}
	assert.Equal(int64(4), total, "identities and unattributed must partition the source")
}

func TestVirtualAccountGroupsCollapseMasks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	var confirmations []store.IdentityConfirmation
	for i := range 1001 {
		confirmations = append(confirmations, store.IdentityConfirmation{Identifier: fmt.Sprintf("mask%d@example.org", i), Signals: []string{"provider-alias", "fastmail-masked-email"}})
	}
	_, err := f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, confirmations)
	require.NoError(err)
	got, err := f.Store.ListVirtualAccountsContext(t.Context())
	require.NoError(err)
	require.Len(got[f.Source.ID], 2, "one group plus unattributed, regardless of mask count")
	assert.Equal("fastmail-masked:"+f.Source.Identifier, got[f.Source.ID][1].Group)
}

func TestVirtualAccountCatalogEmailSourceTypes(t *testing.T) {
	f := storetest.New(t)
	for _, kind := range []string{"gmail", "imap", "o365", "msmail", "mbox", "hey", "apple-mail", "pst", "eml", "maildir", "gcal"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			source, err := f.Store.GetOrCreateSource(kind, kind+"@example.org")
			require.NoError(err)
			require.NoError(f.Store.AddAccountIdentity(source.ID, source.Identifier, "manual"))
			accounts, err := f.Store.ListVirtualAccountsContext(t.Context())
			require.NoError(err)
			assert.Len(accounts[source.ID], 2, "confirmed address and unattributed bucket")
		})
	}
}

func TestVirtualAccountGroupUsesAddressIndex(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("SQLite query plan; PostgreSQL exercises the same functional filter")
	}
	parsed := search.Parse("account:fastmail-masked:" + f.Source.Identifier)
	conditions, args := search.AppendAccountConditions(nil, nil, parsed.AccountScopes, "m", "account_identity_group_memberships")
	rows, err := f.Store.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN SELECT m.id FROM messages m WHERE "+strings.Join(conditions, " AND "), args...)
	require.NoError(err)
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(rows.Scan(&id, &parent, &unused, &detail))
		plan.WriteString(detail + "\n")
	}
	require.NoError(rows.Err())
	defer func() { require.NoError(rows.Close()) }()
	assert.Contains(plan.String(), "idx_account_identities_address_key (source_id=? AND address_key=?)", "each masked membership lookup must use both indexed keys")
}

func TestSourceAccountListingDoesNotReadVirtualCatalog(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	var dialect query.Dialect = query.SQLiteQueryDialect{}
	if f.Store.IsPostgreSQL() {
		dialect = query.PostgreSQLQueryDialect{}
	}
	e := query.NewEngineWithDialect(f.Store.DB(), dialect)
	// A sources-only lookup remains usable without the virtual projection.
	_, err := f.Store.DB().Exec(`DROP VIEW account_identity_group_memberships`)
	require.NoError(err)
	accounts, err := e.ListSourceAccounts(t.Context())
	require.NoError(err)
	require.Len(accounts, 1)
	assert.Equal(f.Source.ID, accounts[0].ID)
	assert.Equal(f.Source.Identifier, accounts[0].Identifier)
	assert.Empty(accounts[0].VirtualAccounts)
	_, err = e.ListAccounts(t.Context())
	require.Error(err, "negative control: full catalog actually needs the virtual projection")
}

func TestAccountScopesIncludeCalendarStatsAndAggregates(t *testing.T) {
	runCase := func(t *testing.T, e *query.SQLiteEngine, selector string) {
		t.Helper()
		require := require.New(t)
		assert := assert.New(t)
		scopes := search.Parse(selector).AccountScopes
		want := int64(2)
		if strings.HasPrefix(selector, "received:") {
			want = 1
		}
		stats, err := e.GetTotalStats(t.Context(), query.StatsOptions{Filter: &query.MessageFilter{AccountScopes: scopes}})
		require.NoError(err)
		assert.Equal(want, stats.MessageCount, "structured scope supplies the account message types")
		stats, err = e.GetTotalStats(t.Context(), query.StatsOptions{SearchQuery: selector})
		require.NoError(err)
		assert.Equal(want, stats.MessageCount, "operator scope supplies the account message types")
		aggregates, err := e.Aggregate(t.Context(), query.ViewTime, query.AggregateOptions{AccountScopes: scopes})
		require.NoError(err)
		var total int64
		for _, row := range aggregates {
			total += row.Count
		}
		assert.Equal(want, total)
		aggregates, err = e.SubAggregate(t.Context(), query.MessageFilter{AccountScopes: scopes}, query.ViewTime, query.AggregateOptions{})
		require.NoError(err)
		total = 0
		for _, row := range aggregates {
			total += row.Count
		}
		assert.Equal(want, total)
	}
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	require.NoError(st.AddAccountIdentity(f.Source.ID, "work@example.org", "manual"))
	when := sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	_, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "inbound", MessageType: "email", SentAt: when}, RawMIME: []byte("X-Delivered-To: work@example.org\r\n\r\nbody")})
	require.NoError(err)
	calendar, err := st.GetOrCreateSource("gcal", "calendar-login@example.net")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(calendar.ID, `{"calendar_id":"work@example.org"}`))
	require.NoError(st.AddAccountIdentity(calendar.ID, "work@example.org", "manual"))
	conv, err := st.EnsureConversation(calendar.ID, "event", "Synthetic calendar")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: calendar.ID, ConversationID: conv, SourceMessageID: "event", MessageType: "calendar_event", SentAt: when})
	require.NoError(err)
	var dialect query.Dialect = query.SQLiteQueryDialect{}
	if st.IsPostgreSQL() {
		dialect = query.PostgreSQLQueryDialect{}
	}
	e := query.NewEngineWithDialect(st.DB(), dialect)
	for _, selector := range []string{"account:work@example.org", "received:work@example.org"} {
		t.Run(selector, func(t *testing.T) { runCase(t, e, selector) })
	}
	stats, err := e.GetTotalStats(t.Context(), query.StatsOptions{})
	require.NoError(err)
	assert.Equal(int64(1), stats.MessageCount, "unscoped analytics preserve the email default")
}

func TestStructuredAccountScopesNormalizeWhitespace(t *testing.T) {
	setup := require.New(t)
	f := storetest.New(t)
	st := f.Store
	setup.NoError(st.AddAccountIdentity(f.Source.ID, "work@example.org", "manual"))
	setup.NoError(st.AddAccountIdentity(f.Source.ID, "mask@example.org", "fastmail-masked-email"))
	workID := f.CreateMessage("work")
	maskID := f.CreateMessage("mask")
	setup.NoError(st.UpsertMessageRaw(workID, []byte("X-Delivered-To: work@example.org\r\n\r\nbody")))
	setup.NoError(st.UpsertMessageRaw(maskID, []byte("X-Delivered-To: mask@example.org\r\n\r\nbody")))
	var dialect query.Dialect = query.SQLiteQueryDialect{}
	if st.IsPostgreSQL() {
		dialect = query.PostgreSQLQueryDialect{}
	}
	e := query.NewEngineWithDialect(st.DB(), dialect)
	for _, tc := range []struct {
		name  string
		scope search.AccountScope
		want  int64
	}{
		{"account", search.AccountScope{Addresses: []string{" \tWORK@EXAMPLE.ORG \n"}}, workID},
		{"received", search.AccountScope{Addresses: []string{" \tWORK@EXAMPLE.ORG \n"}, Inbound: true}, workID},
		{"group", search.AccountScope{Groups: []string{" \t" + strings.ToUpper("fastmail-masked:"+f.Source.Identifier) + " \n"}}, maskID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			tc.scope.SourceID = &f.Source.ID
			scopes := []search.AccountScope{tc.scope}
			require.NoError(search.ValidateAccountScopes(scopes))
			q := search.Parse(strings.Join(search.FormatAccountScopes(scopes), " "))
			require.NoError(q.Err())
			rows, err := e.Search(t.Context(), q, 100, 0)
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal(tc.want, rows[0].ID)
			count, err := e.SearchFastCount(t.Context(), q, query.MessageFilter{})
			require.NoError(err)
			assert.Equal(int64(1), count)
			api, _, err := st.SearchMessagesQueryContext(t.Context(), q, 0, 100)
			require.NoError(err)
			require.Len(api, 1)
			assert.Equal(tc.want, api[0].ID)
		})
	}
}
