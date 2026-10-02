package store_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func BenchmarkAccountScopes150k(b *testing.B) {
	st, sourceID, _ := storetest.NewAccountBenchmarkArchive(b, 150000)
	engine := query.NewEngineWithDialect(st.DB(), query.SQLiteQueryDialect{})
	for _, text := range []string{"", "account:work@example.org", "received:work@example.org", "account:fastmail-masked:inbox@example.net", "report", "report account:work@example.org"} {
		name := text
		if name == "" {
			name = "source"
		}
		b.Run(name, func(b *testing.B) {
			parsed := search.Parse(text)
			require.NoError(b, parsed.Err())
			count, err := engine.SearchFastCount(b.Context(), parsed, query.MessageFilter{SourceID: &sourceID})
			require.NoError(b, err)
			require.Positive(b, count)
			b.ResetTimer()
			b.ReportMetric(float64(count), "matched")
			for range b.N {
				_, err := engine.SearchFastCount(b.Context(), parsed, query.MessageFilter{SourceID: &sourceID})
				require.NoError(b, err)
			}
		})
		if text == "report" || text == "report account:work@example.org" {
			continue
		}
		b.Run("list/"+name, func(b *testing.B) {
			parsed := search.Parse(text)
			filter := query.MessageFilter{SourceID: &sourceID, AccountScopes: parsed.AccountScopes, Pagination: query.Pagination{Limit: 50}}
			b.ResetTimer()
			for range b.N {
				messages, err := engine.ListMessages(b.Context(), filter)
				require.NoError(b, err)
				require.Len(b, messages, 50)
			}
		})
	}
	b.Run("virtual_account_catalog", func(b *testing.B) {
		for range b.N {
			accounts, err := st.ListVirtualAccountsContext(b.Context())
			require.NoError(b, err)
			require.Len(b, accounts[sourceID], 4, "sink, work, collapsed masks, and unattributed")
			var total int64
			for _, account := range accounts[sourceID] {
				total += account.MessageCount
			}
			require.Equal(b, int64(150000), total)
		}
	})
	b.Run("confirm_new_mask", func(b *testing.B) {
		b.ResetTimer()
		for range b.N {
			b.StopTimer()
			_, err := st.RemoveAccountIdentity(sourceID, "newmask@example.org")
			require.NoError(b, err)
			b.StartTimer()
			require.NoError(b, st.AddAccountIdentity(sourceID, "newmask@example.org", "fastmail-masked-email"))
		}
		b.StopTimer()
		visited, err := st.RecomputeAccountAttributionForIdentitiesContext(b.Context(), sourceID, []string{"newmask@example.org"})
		require.NoError(b, err)
		require.Equal(b, int64(1), visited)
		b.ReportMetric(float64(visited), "visited")
	})
}

func BenchmarkAccountBackfill1000(b *testing.B) {
	st, sourceID, _ := storetest.NewAccountBenchmarkArchive(b, 1000)
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		_, err := st.DB().Exec("DELETE FROM account_attribution_repair_progress")
		require.NoError(b, err)
		_, err = st.DB().Exec("UPDATE messages SET account_address=NULL,account_path=NULL,account_attribution_basis='not-derived'")
		require.NoError(b, err)
		b.StartTimer()
		p, err := st.BackfillAccountAttributionContext(b.Context(), sourceID, 100, nil)
		require.NoError(b, err)
		require.True(b, p.Completed)
		require.Equal(b, int64(1000), p.Scanned)
	}
}
