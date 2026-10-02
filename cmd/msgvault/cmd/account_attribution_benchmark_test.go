package cmd

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func BenchmarkAccountParquet150k(b *testing.B) {
	_, sourceID, path := storetest.NewAccountBenchmarkArchive(b, 150000)
	cache := filepath.Join(b.TempDir(), "analytics")
	_, err := buildCache(path, cache, true)
	require.NoError(b, err)
	engine, err := query.NewDuckDBEngine(cache, "", nil)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, engine.Close()) })
	for _, selector := range []string{"", "account:work@example.org", "received:work@example.org", "account:fastmail-masked:inbox@example.net"} {
		name := selector
		if name == "" {
			name = "source"
		}
		b.Run(name, func(b *testing.B) {
			parsed := search.Parse(selector)
			filter := query.MessageFilter{SourceID: &sourceID}
			count, err := engine.SearchFastCount(b.Context(), parsed, filter)
			require.NoError(b, err)
			require.Positive(b, count)
			b.ResetTimer()
			b.ReportMetric(float64(count), "matched")
			for range b.N {
				_, err := engine.SearchFastCount(b.Context(), parsed, filter)
				require.NoError(b, err)
			}
		})
		b.Run("list/"+name, func(b *testing.B) {
			parsed := search.Parse(selector)
			filter := query.MessageFilter{SourceID: &sourceID, AccountScopes: parsed.AccountScopes, Pagination: query.Pagination{Limit: 50}}
			b.ResetTimer()
			for range b.N {
				messages, err := engine.ListMessages(b.Context(), filter)
				require.NoError(b, err)
				require.Len(b, messages, 50)
			}
		})
	}
}
