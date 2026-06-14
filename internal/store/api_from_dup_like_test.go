package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
)

// TestSearchMessagesLike_MultipleFromRows_NoDuplication (D2) covers the LIKE
// fallback path — searchMessagesLike and the forced no-FTS branch of
// searchMessagesQueryImpl — which share the same from-side LEFT JOIN as the
// FTS path. A message with multiple recipient_type='from' rows must NOT be
// duplicated by these queries either. Internal-package test (SQLite) so it can
// drive the unexported fallback directly; the join fix is dialect-agnostic and
// the cross-backend coverage lives in TestStoreAPI_MultipleFromRows_NoDuplication.
func TestSearchMessagesLike_MultipleFromRows_NoDuplication(t *testing.T) {
	st := openTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "fromduplike@example.com")
	requirepkg.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-fromduplike", "Thread FromDupLike")
	requirepkg.NoError(t, err, "EnsureConversation")

	aliceID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	requirepkg.NoError(t, err, "EnsureParticipant alice")
	bobID, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	requirepkg.NoError(t, err, "EnsureParticipant bob")

	const n = 3
	const tag = "fromdupliketag"
	sameTime := time.Date(2024, 9, 2, 12, 0, 0, 0, time.UTC)

	wantIDs := make(map[int64]struct{}, n)
	ids := make([]int64, 0, n)
	for i := range n {
		id, err := st.UpsertMessage(&Message{
			ConversationID:  convID,
			SourceID:        src.ID,
			SourceMessageID: fmt.Sprintf("fromduplike-msg-%d", i),
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: sameTime, Valid: true},
			Subject:         sql.NullString{String: tag + " " + strconv.Itoa(i), Valid: true},
			Snippet:         sql.NullString{String: tag + " snippet", Valid: true},
			SizeEstimate:    100,
		})
		requirepkg.NoError(t, err, "UpsertMessage %d", i)
		wantIDs[id] = struct{}{}
		ids = append(ids, id)
		requirepkg.NoError(t,
			st.ReplaceMessageRecipients(id, "from", []int64{aliceID}, []string{"Alice"}),
			"single from %d", i)
	}
	// Last message gets TWO distinct 'from' rows — duplicated on the old join.
	dupID := ids[n-1]
	requirepkg.NoError(t,
		st.ReplaceMessageRecipients(dupID, "from",
			[]int64{aliceID, bobID}, []string{"Alice", "Bob"}),
		"two-from")

	occ := func(msgs []APIMessage, id int64) int {
		c := 0
		for _, m := range msgs {
			if m.ID == id {
				c++
			}
		}
		return c
	}
	assertDistinct := func(t *testing.T, msgs []APIMessage, total int64, label string) {
		t.Helper()
		seen := make(map[int64]struct{}, len(msgs))
		for _, m := range msgs {
			_, dup := seen[m.ID]
			assertpkg.Falsef(t, dup, "%s: id %d duplicated", label, m.ID)
			seen[m.ID] = struct{}{}
		}
		assertpkg.Equalf(t, 1, occ(msgs, dupID), "%s: multi-from id %d must appear once", label, dupID)
		assertpkg.Lenf(t, msgs, n, "%s: page must hold every distinct id once", label)
		for id := range wantIDs {
			_, ok := seen[id]
			assertpkg.Truef(t, ok, "%s: distinct id %d missing", label, id)
		}
		assertpkg.Equalf(t, int64(n), total, "%s: total must equal distinct count", label)
	}

	t.Run("searchMessagesLike", func(t *testing.T) {
		msgs, total, err := st.searchMessagesLike(tag, 0, n)
		requirepkg.NoError(t, err, "searchMessagesLike")
		assertDistinct(t, msgs, total, "searchMessagesLike")
	})

	t.Run("searchMessagesQueryImpl no-FTS branch", func(t *testing.T) {
		msgs, total, err := st.searchMessagesQueryImpl(
			&search.Query{TextTerms: []string{tag}}, 0, n, false)
		requirepkg.NoError(t, err, "searchMessagesQueryImpl ftsAvailable=false")
		assertDistinct(t, msgs, total, "searchMessagesQueryImpl no-FTS")
	})
}
