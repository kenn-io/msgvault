package store_test

import (
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestStoreAPI_PaginationStability_IdenticalSentAt is the C3 regression guard
// for the store-API query paths that the original Q2 fix missed:
//   - Store.ListMessages           (api.go ORDER BY ... DESC, m.id DESC)
//   - Store.SearchMessagesQuery    (api.go orderBy var + ", m.id DESC")
//
// Before the tiebreaker, rows sharing an identical sent_at had an undefined
// relative order, so LIMIT/OFFSET pagination could drop or duplicate the same
// id across adjacent pages. The m.id DESC tiebreaker makes paging deterministic.
//
// Runs on whichever backend testutil.NewTestStore selects; a postgres:// DSN in
// MSGVAULT_TEST_DB exercises the PG path (the engine the Q2 fix did not touch).
func TestStoreAPI_PaginationStability_IdenticalSentAt(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "page@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-page", "Thread Page")
	require.NoError(err, "EnsureConversation")
	aliceID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err, "EnsureParticipant")

	// Several messages sharing one sent_at: the ambiguous case that makes
	// ordering by sent_at alone non-deterministic under LIMIT/OFFSET.
	const n = 5
	sameTime := time.Date(2024, 8, 1, 12, 0, 0, 0, time.UTC)
	const subjectTag = "pagestableapi"
	wantIDs := make(map[int64]struct{}, n)
	for i := range n {
		mid, err := st.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        src.ID,
			SourceMessageID: fmt.Sprintf("api-page-msg-%d", i),
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: sameTime, Valid: true},
			Subject:         sql.NullString{String: subjectTag + " " + strconv.Itoa(i), Valid: true},
			Snippet:         sql.NullString{String: "snippet", Valid: true},
			SizeEstimate:    1000,
		})
		require.NoError(err, "UpsertMessage %d", i)
		require.NoError(
			st.ReplaceMessageRecipients(mid, "from", []int64{aliceID}, []string{"Alice"}),
			"ReplaceMessageRecipients %d", i)
		wantIDs[mid] = struct{}{}
	}

	// (1) Store.ListMessages — paginate one row at a time over identical sent_at.
	t.Run("ListMessages", func(t *testing.T) {
		seen := pageStoreOneByOne(t, n, func(offset int) (int64, bool) {
			msgs, _, err := st.ListMessages(offset, 1)
			requirepkg.NoError(t, err, "ListMessages offset=%d", offset)
			if len(msgs) == 0 {
				return 0, false
			}
			return msgs[0].ID, true
		})
		assertStorePagesDisjointComplete(t, wantIDs, seen)
	})

	// (2) Store.SearchMessagesQuery with a SubjectTerms filter shared by all
	// rows — selects exactly the n rows without depending on FTS availability,
	// exercising the orderBy var the C3 tiebreaker was appended to.
	t.Run("SearchMessagesQuery", func(t *testing.T) {
		seen := pageStoreOneByOne(t, n, func(offset int) (int64, bool) {
			msgs, _, err := st.SearchMessagesQuery(
				&search.Query{SubjectTerms: []string{subjectTag}}, offset, 1)
			requirepkg.NoError(t, err, "SearchMessagesQuery offset=%d", offset)
			if len(msgs) == 0 {
				return 0, false
			}
			return msgs[0].ID, true
		})
		assertStorePagesDisjointComplete(t, wantIDs, seen)
	})
}

// pageStoreOneByOne walks offsets 0..n-1 with limit=1, collecting the single id
// returned at each offset. It fails the test if any page is empty (a skip).
func pageStoreOneByOne(t *testing.T, n int, fetch func(offset int) (int64, bool)) []int64 {
	t.Helper()
	seen := make([]int64, 0, n)
	for offset := range n {
		id, ok := fetch(offset)
		requirepkg.True(t, ok, "page at offset=%d returned no row (pagination skipped a row)", offset)
		seen = append(seen, id)
	}
	return seen
}

// assertStorePagesDisjointComplete verifies the paged ids contain no duplicates
// and exactly cover the expected set — no dupes across pages, no skips.
func assertStorePagesDisjointComplete(t *testing.T, want map[int64]struct{}, got []int64) {
	t.Helper()
	gotSet := make(map[int64]struct{}, len(got))
	for _, id := range got {
		_, dup := gotSet[id]
		assertpkg.False(t, dup, "id %d appeared on more than one page (unstable pagination)", id)
		gotSet[id] = struct{}{}
	}
	assertpkg.Len(t, got, len(want), "paged ids should cover every row exactly once")
	for id := range want {
		_, ok := gotSet[id]
		assertpkg.True(t, ok, "expected id %d missing from paged results (row skipped)", id)
	}
}
