package query

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkParticipantFilter times the views a person drill-down opens on the
// generated archive in buildBenchData: the entry listing, the file listing,
// the participant groups, and the selection totals, each scoped to one
// participant. A participant filter must find that person's sender,
// recipient, and roster rows without assembling participant lists for every
// message in the archive. Each sub-benchmark also reports min-memory-MB.
func BenchmarkParticipantFilter(b *testing.B) {
	engine := buildBenchData(b)
	ctx := b.Context()
	person := ExploreRequest{Context: Context{ParticipantIDs: []int64{1}}}

	b.Run("entries", func(b *testing.B) {
		benchmarkWithMinimumMemory(b, engine, func() error {
			request := person
			request.Page = PageSpec{Limit: 50}
			result, err := engine.Explore(ctx, request)
			if err != nil {
				return err
			}
			// Participant 1 appears in four of the 100 chat conversations.
			require.Len(b, result.Rows, 4)
			return nil
		})
	})

	b.Run("files", func(b *testing.B) {
		benchmarkWithMinimumMemory(b, engine, func() error {
			result, err := engine.SearchFiles(ctx, FileSearchRequest{Explore: person, Page: PageSpec{Limit: 50}})
			if err != nil {
				return err
			}
			require.Len(b, result.Files, 50)
			return nil
		})
	})

	b.Run("participant_groups", func(b *testing.B) {
		benchmarkWithMinimumMemory(b, engine, func() error {
			result, err := engine.ExploreGroups(ctx, ExploreGroupRequest{
				Explore: person, Dimension: "participant", Page: PageSpec{Limit: 50},
			})
			if err != nil {
				return err
			}
			require.NotEmpty(b, result.Rows)
			return nil
		})
	})

	b.Run("selection_stats", func(b *testing.B) {
		benchmarkWithMinimumMemory(b, engine, func() error {
			result, err := engine.ExploreSelectionStats(ctx, ExploreSelectionRequest{Explore: person})
			if err != nil {
				return err
			}
			require.Positive(b, result.Count)
			return nil
		})
	})
}
