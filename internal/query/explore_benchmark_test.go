package query

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkExploreLargeArchive is the scheduled/manual reference gate for the
// generated mixed archive in buildBenchData. Warm first-page and grouped-view
// operations have a 500 ms reference budget; shared CI compiles and executes a
// bounded smoke iteration instead of enforcing wall-clock timing.
//
// Broad search deliberately transfers at most 10,000 ranked message IDs from
// the authoritative search engine into DuckDB. EntryRow has no message-ID list,
// so 100,000 raw chat fragments project to bounded logical conversation rows
// without an unbounded cross-engine message-ID transfer.
func BenchmarkExploreLargeArchive(b *testing.B) {
	engine := buildBenchData(b)
	ctx := context.Background()

	b.Run("first_page", func(b *testing.B) {
		for b.Loop() {
			result, err := engine.Explore(ctx, ExploreRequest{Page: PageSpec{Limit: 50}})
			require.NoError(b, err)
			require.LessOrEqual(b, len(result.Rows), 50)
		}
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
