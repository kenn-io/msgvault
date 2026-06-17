package query_test

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestPaginationStability_IdenticalSentAt is the regression guard for Q2:
// before the fix, ListMessages and Search ordered only by sent_at, so rows
// sharing an identical timestamp had an undefined relative order and the
// same id could appear on two adjacent pages (or be skipped entirely) under
// LIMIT/OFFSET. The m.id DESC tiebreaker makes pagination deterministic.
//
// Runs on whichever backend testutil.NewTestStore selects; setting
// MSGVAULT_TEST_DB to a postgres:// DSN exercises the PG path too.
const paginationStabilityN = 3
const paginationStabilitySubjectTag = "pagestable"

// seedPaginationStabilityFixture inserts paginationStabilityN messages that all
// share an identical sent_at — the ambiguous case that makes ordering by
// sent_at alone non-deterministic — and returns the source plus the set of
// inserted message IDs. Extracted from the test body so the assertion-heavy
// setup keeps its require checks without tripping the testify-helper linter.
func seedPaginationStabilityFixture(t *testing.T, st *store.Store) (*store.Source, map[int64]struct{}) {
	t.Helper()
	require := require.New(t)
	src, err := st.GetOrCreateSource("gmail", "pagination@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-page", "Thread Page")
	require.NoError(err, "EnsureConversation")
	aliceID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err, "EnsureParticipant")

	sameTime := time.Date(2024, 8, 1, 12, 0, 0, 0, time.UTC)
	wantIDs := make(map[int64]struct{}, paginationStabilityN)
	for i := range paginationStabilityN {
		mid, err := st.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        src.ID,
			SourceMessageID: fmt.Sprintf("page-msg-%d", i),
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: sameTime, Valid: true},
			Subject:         sql.NullString{String: paginationStabilitySubjectTag + " " + strconv.Itoa(i), Valid: true},
			Snippet:         sql.NullString{String: "snippet", Valid: true},
			SizeEstimate:    1000,
		})
		require.NoError(err, "UpsertMessage %d", i)
		require.NoError(
			st.ReplaceMessageRecipients(mid, "from", []int64{aliceID}, []string{"Alice"}),
			"ReplaceMessageRecipients %d", i)
		wantIDs[mid] = struct{}{}
	}
	return src, wantIDs
}

func TestPaginationStability_IdenticalSentAt(t *testing.T) {
	st := testutil.NewTestStore(t)
	src, wantIDs := seedPaginationStabilityFixture(t, st)
	const n = paginationStabilityN
	const subjectTag = paginationStabilitySubjectTag

	eng := query.NewEngine(st.DB(), st.IsPostgreSQL())
	ctx := context.Background()

	// (1) ListMessages — assert determinism for every sort field. The
	// tiebreaker must apply regardless of the primary sort column.
	for _, sort := range []struct {
		name  string
		field query.MessageSortField
	}{
		{"by_date", query.MessageSortByDate},
		{"by_size", query.MessageSortBySize},
		{"by_subject", query.MessageSortBySubject},
	} {
		t.Run("list_"+sort.name, func(t *testing.T) {
			seen := pageOneByOne(t, n, func(offset int) (int64, bool) {
				msgs, err := eng.ListMessages(ctx, query.MessageFilter{
					SourceID: &src.ID,
					Sorting: query.MessageSorting{
						Field:     sort.field,
						Direction: query.SortDesc,
					},
					Pagination: query.Pagination{Limit: 1, Offset: offset},
				})
				require.NoError(t, err, "ListMessages offset=%d", offset)
				if len(msgs) == 0 {
					return 0, false
				}
				return msgs[0].ID, true
			})
			assertDisjointAndComplete(t, wantIDs, seen)
		})
	}

	// (2) Search — exercises executeSearchQuery's ORDER BY. A subject term
	// shared by all three rows selects exactly them without needing FTS.
	t.Run("search", func(t *testing.T) {
		seen := pageOneByOne(t, n, func(offset int) (int64, bool) {
			results, err := eng.Search(ctx,
				&search.Query{SubjectTerms: []string{subjectTag}}, 1, offset)
			require.NoError(t, err, "Search offset=%d", offset)
			if len(results) == 0 {
				return 0, false
			}
			return results[0].ID, true
		})
		assertDisjointAndComplete(t, wantIDs, seen)
	})
}

// pageOneByOne walks offsets 0..n-1 with limit=1, collecting the single id
// returned at each offset. It fails the test if any page is empty (a skip).
func pageOneByOne(t *testing.T, n int, fetch func(offset int) (int64, bool)) []int64 {
	t.Helper()
	seen := make([]int64, 0, n)
	for offset := range n {
		id, ok := fetch(offset)
		require.True(t, ok, "page at offset=%d returned no row (pagination skipped a row)", offset)
		seen = append(seen, id)
	}
	return seen
}

// assertDisjointAndComplete verifies the paged ids contain no duplicates and
// exactly cover the expected set — no dupes across pages, no skips.
func assertDisjointAndComplete(t *testing.T, want map[int64]struct{}, got []int64) {
	t.Helper()
	gotSet := make(map[int64]struct{}, len(got))
	for _, id := range got {
		_, dup := gotSet[id]
		assert.False(t, dup, "id %d appeared on more than one page (unstable pagination)", id)
		gotSet[id] = struct{}{}
	}
	assert.Len(t, got, len(want), "paged ids should cover every row exactly once")
	for id := range want {
		_, ok := gotSet[id]
		assert.True(t, ok, "expected id %d missing from paged results (row skipped)", id)
	}
}
