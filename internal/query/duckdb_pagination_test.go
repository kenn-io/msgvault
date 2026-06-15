package query

import (
	"context"
	"testing"
	"time"

	_ "github.com/marcboeker/go-duckdb"
	requirepkg "github.com/stretchr/testify/require"
)

// TestDuckDBPaginationStability_IdenticalSortKey is the C3 regression for the
// DuckDB query engine, whose pageable ORDER BYs the original Q2 fix did not
// touch. It exercises two paths over a Parquet dataset whose rows deliberately
// share their primary sort key:
//
//   - DuckDBEngine.ListMessages    — messages with an identical sent_at; the
//     pagination-determining order is the filtered_msgs CTE (now ", msg.id DESC")
//   - DuckDBEngine.ListConversations — conversations whose latest message shares
//     one last_message_at; outer ORDER BY now carries ", conv.id DESC"
//
// Without the PK tiebreaker, LIMIT/OFFSET paging over equal-key rows can drop or
// duplicate the same id across adjacent pages. Paging one row at a time and
// asserting the union is disjoint-and-complete pins the deterministic order.
func TestDuckDBPaginationStability_IdenticalSortKey(t *testing.T) {
	b := NewTestDataBuilder(t)
	src := b.AddSource("me@gmail.com")
	alice := b.AddParticipant("alice@example.com", "example.com", "Alice")

	sameTime := time.Date(2024, 8, 1, 12, 0, 0, 0, time.UTC)
	const n = 5

	// Five messages, each in its OWN conversation, all sharing sent_at. This
	// gives us n messages with an identical sent_at (for ListMessages) AND n
	// conversations whose last_message_at is identical (for ListConversations).
	wantMsgIDs := make(map[int64]struct{}, n)
	wantConvIDs := make(map[int64]struct{}, n)
	for i := range n {
		convID := int64(100 + i)
		mid := b.AddMessage(MessageOpt{
			MessageType:    "sms",
			SourceID:       src,
			ConversationID: convID,
			SentAt:         sameTime,
		})
		b.AddFrom(mid, alice, "Alice")
		wantMsgIDs[mid] = struct{}{}
		wantConvIDs[convID] = struct{}{}
	}

	engine := b.BuildEngine()
	ctx := context.Background()

	t.Run("ListMessages", func(t *testing.T) {
		seen := pageDuckOneByOne(t, n, func(offset int) (int64, bool) {
			msgs, err := engine.ListMessages(ctx, MessageFilter{
				SourceID: &src,
				Sorting: MessageSorting{
					Field:     MessageSortByDate,
					Direction: SortDesc,
				},
				Pagination: Pagination{Limit: 1, Offset: offset},
			})
			requirepkg.NoError(t, err, "ListMessages offset=%d", offset)
			if len(msgs) == 0 {
				return 0, false
			}
			return msgs[0].ID, true
		})
		assertDuckPagesDisjointComplete(t, wantMsgIDs, seen)
	})

	t.Run("ListConversations", func(t *testing.T) {
		seen := pageDuckOneByOne(t, n, func(offset int) (int64, bool) {
			rows, err := engine.ListConversations(ctx, TextFilter{
				SortField:     TextSortByLastMessage,
				SortDirection: SortDesc,
				Pagination:    Pagination{Limit: 1, Offset: offset},
			})
			requirepkg.NoError(t, err, "ListConversations offset=%d", offset)
			if len(rows) == 0 {
				return 0, false
			}
			return rows[0].ConversationID, true
		})
		assertDuckPagesDisjointComplete(t, wantConvIDs, seen)
	})
}

func pageDuckOneByOne(t *testing.T, n int, fetch func(offset int) (int64, bool)) []int64 {
	t.Helper()
	seen := make([]int64, 0, n)
	for offset := range n {
		id, ok := fetch(offset)
		requirepkg.Truef(t, ok, "page at offset=%d returned no row (pagination skipped a row)", offset)
		seen = append(seen, id)
	}
	return seen
}

func assertDuckPagesDisjointComplete(t *testing.T, want map[int64]struct{}, got []int64) {
	t.Helper()
	gotSet := make(map[int64]struct{}, len(got))
	for _, id := range got {
		_, dup := gotSet[id]
		requirepkg.Falsef(t, dup, "id %d appeared on more than one page (unstable pagination)", id)
		gotSet[id] = struct{}{}
	}
	requirepkg.Len(t, got, len(want), "paged ids should cover every row exactly once")
	for id := range want {
		_, ok := gotSet[id]
		requirepkg.Truef(t, ok, "expected id %d missing from paged results (row skipped)", id)
	}
}
