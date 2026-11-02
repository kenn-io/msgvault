package query

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/duckdbutil"
)

// BenchmarkExploreLargeArchive is the scheduled/manual reference gate for the
// generated mixed archive in buildBenchData: 100,004 messages, 104 logical
// entries, and 20,000 attachments. Warm first-page and grouped-view operations
// have a 500 ms reference budget; shared CI compiles and executes a bounded
// smoke iteration instead of enforcing wall-clock timing. Listing and grouping
// sub-benchmarks also report min-memory-MB, the smallest DuckDB memory_limit
// under which the operation succeeds with four query threads.
//
// Broad search deliberately transfers at most 10,000 ranked message IDs from
// the authoritative search engine into DuckDB. EntryRow has no message-ID list,
// so 100,000 raw chat fragments project to bounded logical conversation rows
// without an unbounded cross-engine message-ID transfer.
func BenchmarkExploreLargeArchive(b *testing.B) {
	engine := buildBenchData(b)
	ctx := context.Background()

	b.Run("first_page", func(b *testing.B) {
		benchmarkWithMinimumMemory(b, engine, func() error {
			result, err := engine.Explore(ctx, ExploreRequest{Page: PageSpec{Limit: 50}})
			if err != nil {
				return err
			}
			require.Len(b, result.Rows, 50)
			require.Equal(b, int64(104), result.TotalCount)
			return nil
		})
	})

	b.Run("page_past_end", func(b *testing.B) {
		benchmarkWithMinimumMemory(b, engine, func() error {
			result, err := engine.Explore(ctx, ExploreRequest{Page: PageSpec{Limit: 50, Offset: 500}})
			if err != nil {
				return err
			}
			require.Empty(b, result.Rows)
			require.Equal(b, int64(104), result.TotalCount)
			return nil
		})
	})

	b.Run("domain_filter", func(b *testing.B) {
		benchmarkWithMinimumMemory(b, engine, func() error {
			result, err := engine.Explore(ctx, ExploreRequest{
				Context: Context{Domains: []string{"acme.com"}},
				Page:    PageSpec{Limit: 50},
			})
			if err != nil {
				return err
			}
			require.Len(b, result.Rows, 8)
			require.Equal(b, int64(8), result.TotalCount)
			return nil
		})
	})

	for _, tc := range []struct {
		dimension string
		count     int64
	}{
		{"participant", 500},
		{"domain", 50},
	} {
		b.Run(tc.dimension+"_groups", func(b *testing.B) {
			benchmarkWithMinimumMemory(b, engine, func() error {
				result, err := engine.ExploreGroups(ctx, ExploreGroupRequest{
					Dimension: tc.dimension, Page: PageSpec{Limit: 50},
				})
				if err != nil {
					return err
				}
				require.Len(b, result.Rows, 50)
				require.Equal(b, tc.count, result.TotalCount)
				return nil
			})
		})
	}

	b.Run("files_first_page", func(b *testing.B) {
		benchmarkWithMinimumMemory(b, engine, func() error {
			result, err := engine.SearchFiles(ctx, FileSearchRequest{
				Page: PageSpec{Limit: 500},
			})
			if err != nil {
				return err
			}
			require.Len(b, result.Files, 500)
			require.Equal(b, int64(20_000), result.TotalCount)
			return nil
		})
	})

	b.Run("nested_group", func(b *testing.B) {
		for b.Loop() {
			result, err := engine.ExploreGroups(ctx, ExploreGroupRequest{
				Explore:   ExploreRequest{Grouping: []GroupSpec{{Dimension: "source"}}},
				Dimension: "domain", Sort: SortSpec{Field: "count", Direction: "desc"},
				Page: PageSpec{Limit: 50},
			})
			require.NoError(b, err)
			require.LessOrEqual(b, len(result.Rows), 50)
		}
	})

	b.Run("person_files", func(b *testing.B) {
		for b.Loop() {
			result, err := engine.ExploreFiles(ctx, ExploreFilesRequest{
				Explore: ExploreRequest{Context: Context{ParticipantIDs: []int64{1}}},
				Page:    PageSpec{Limit: 50},
			})
			require.NoError(b, err)
			require.LessOrEqual(b, len(result.Files), 50)
		}
	})

	b.Run("broad_chat_search_projection", func(b *testing.B) {
		require.Equal(b, 10_000, MaxExploreCandidateMessageIDs, "the benchmark must track the fixed production transfer contract")
		candidateIDs := make([]int64, MaxExploreCandidateMessageIDs)
		for index := range candidateIDs {
			candidateIDs[index] = int64(index + 1)
		}
		require.Len(b, candidateIDs, MaxExploreCandidateMessageIDs)
		_, leaksArchiveSizedIDs := reflect.TypeFor[EntryRow]().FieldByName("MatchedMessageIDs")
		require.False(b, leaksArchiveSizedIDs)

		for b.Loop() {
			result, err := engine.Explore(ctx, ExploreRequest{
				Search: SearchSpec{Mode: SearchFullText, Query: "synthetic", CandidateMessageIDs: candidateIDs,
					LexicalIndexRevision: fmt.Sprintf("benchmark:%d", MaxExploreCandidateMessageIDs)},
				Page: PageSpec{Limit: 50},
			})
			require.NoError(b, err)
			require.LessOrEqual(b, len(result.Rows), 50)
		}
	})
}

// benchmarkWithMinimumMemory times run, then reports its min-memory-MB.
func benchmarkWithMinimumMemory(b *testing.B, engine *DuckDBEngine, run func() error) {
	b.Helper()
	for b.Loop() {
		require.NoError(b, run())
	}
	b.ReportMetric(minimumMemoryLimitMB(b, engine, run), "min-memory-MB")
}

// minimumMemoryLimitMB bisects the smallest DuckDB memory_limit, in decimal
// megabytes, under which run succeeds with four query threads. That is the
// memory DuckDB cannot evict or spill at the query's peak; thread stacks and
// the Go heap are outside it. The peak varies by a few megabytes between runs,
// so compare the metric across -count samples. Errors other than
// out-of-memory fail the run. The engine's own
// limit and thread count are restored afterwards.
func minimumMemoryLimitMB(b *testing.B, engine *DuckDBEngine, run func() error) float64 {
	b.Helper()
	var memoryLimit, threads string
	require.NoError(b, engine.db.QueryRow(
		"SELECT current_setting('memory_limit'), current_setting('threads')::VARCHAR",
	).Scan(&memoryLimit, &threads))
	defer func() {
		_, err := engine.db.Exec("SET memory_limit = '" + memoryLimit + "'")
		require.NoError(b, err)
		_, err = engine.db.Exec("SET threads = " + threads)
		require.NoError(b, err)
	}()
	_, err := engine.db.Exec("SET threads = 4")
	require.NoError(b, err)

	low, high := 1, 1024
	for low < high {
		mid := (low + high) / 2
		_, err = engine.db.Exec(fmt.Sprintf("SET memory_limit = '%dMB'", mid))
		require.NoError(b, err)
		err = run()
		if err == nil {
			high = mid
			continue
		}
		require.True(b, duckdbutil.IsOutOfMemory(err), "unexpected error at memory_limit=%dMB: %v", mid, err)
		low = mid + 1
	}
	require.Less(b, high, 1024, "run did not fit the largest probed memory_limit")
	return float64(high)
}
