package store_test

import (
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestStoreAPI_MultipleFromRows_NoDuplication (D2) is the regression guard for
// the paginated/aggregate query paths whose from-side LEFT JOIN multiplied a
// row when a message had MORE THAN ONE recipient_type='from' record. RFC-5322
// allows multiple From mailboxes, and sync's buildRecipientSet emits one
// message_recipients row per distinct from-participant, so this is reachable.
//
// On the OLD join:
//
//	LEFT JOIN message_recipients mr ON mr.message_id = m.id
//	    AND mr.recipient_type = 'from'
//
// a message with two distinct 'from' rows appeared TWICE in the result page
// (displacing another message and disagreeing with the distinct COUNT(*)). The
// fix joins on the single chosen from-row id so at most one mr row matches.
//
// Runs on whichever backend testutil.NewTestStore selects; a postgres:// DSN in
// MSGVAULT_TEST_DB exercises the PG path as well as SQLite.
func TestStoreAPI_MultipleFromRows_NoDuplication(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "fromdup@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-fromdup", "Thread FromDup")
	require.NoError(err, "EnsureConversation")

	aliceID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err, "EnsureParticipant alice")
	bobID, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err, "EnsureParticipant bob")
	carolID, err := st.EnsureParticipant("carol@example.com", "Carol", "example.com")
	require.NoError(err, "EnsureParticipant carol")

	// All messages share one sent_at so ORDER BY sent_at DESC, id DESC is fully
	// deterministic and any row multiplication provably displaces a sibling page
	// row. subjectTag is shared so the search paths select exactly this set.
	const n = 4
	const subjectTag = "fromdupsubject"
	sameTime := time.Date(2024, 9, 1, 12, 0, 0, 0, time.UTC)

	wantIDs := make(map[int64]struct{}, n)
	ids := make([]int64, 0, n)
	for i := range n {
		mid, err := st.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        src.ID,
			SourceMessageID: fmt.Sprintf("fromdup-msg-%d", i),
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: sameTime, Valid: true},
			Subject:         sql.NullString{String: subjectTag + " " + strconv.Itoa(i), Valid: true},
			Snippet:         sql.NullString{String: subjectTag + " snippet", Valid: true},
			SizeEstimate:    1000,
		})
		require.NoError(err, "UpsertMessage %d", i)
		wantIDs[mid] = struct{}{}
		ids = append(ids, mid)
	}

	// Give every message a single 'from' row...
	for _, mid := range ids {
		require.NoError(
			st.ReplaceMessageRecipients(mid, "from", []int64{aliceID}, []string{"Alice"}),
			"ReplaceMessageRecipients single from")
	}
	// ...except the LAST one, which gets THREE distinct 'from' rows. On the old
	// join this message would appear three times, displacing two other rows from
	// any page large enough to start at it.
	dupID := ids[n-1]
	require.NoError(
		st.ReplaceMessageRecipients(dupID, "from",
			[]int64{aliceID, bobID, carolID},
			[]string{"Alice", "Bob", "Carol"}),
		"ReplaceMessageRecipients three-from")

	// countOccurrences returns how many times id appears in msgs.
	countOccurrences := func(msgs []store.APIMessage, id int64) int {
		c := 0
		for _, m := range msgs {
			if m.ID == id {
				c++
			}
		}
		return c
	}

	// assertDistinctPage asserts the page has no duplicate ids, the dup message
	// appears exactly once, and (when the page is large enough) every seeded id
	// is present exactly once. This is the assertion that FAILS on the old join.
	assertDistinctPage := func(t *testing.T, msgs []store.APIMessage, total int64, label string) {
		t.Helper()
		seen := make(map[int64]struct{}, len(msgs))
		for _, m := range msgs {
			_, dup := seen[m.ID]
			assert.Falsef(t, dup, "%s: id %d appeared more than once (row multiplication)", label, m.ID)
			seen[m.ID] = struct{}{}
		}
		assert.Equalf(t, 1, countOccurrences(msgs, dupID),
			"%s: multi-from message %d must appear exactly once", label, dupID)
		// A full page (limit >= n) must contain every distinct id exactly once;
		// on the old join the duplicate displaced a sibling so one id went missing.
		assert.Lenf(t, msgs, n, "%s: full page must hold every distinct message once", label)
		for id := range wantIDs {
			_, ok := seen[id]
			assert.Truef(t, ok, "%s: distinct id %d missing (displaced by a duplicate)", label, id)
		}
		assert.Equalf(t, int64(n), total, "%s: total must equal distinct message count", label)
	}

	t.Run("ListMessages", func(t *testing.T) {
		msgs, total, err := st.ListMessages(0, n)
		require.NoError(err, "ListMessages")
		assertDistinctPage(t, msgs, total, "ListMessages")
	})

	t.Run("SearchMessagesQuery", func(t *testing.T) {
		msgs, total, err := st.SearchMessagesQuery(
			&search.Query{SubjectTerms: []string{subjectTag}}, 0, n)
		require.NoError(err, "SearchMessagesQuery")
		assertDistinctPage(t, msgs, total, "SearchMessagesQuery")
	})

	t.Run("GetMessage returns the multi-from message once with a sender", func(t *testing.T) {
		m, err := st.GetMessage(dupID)
		require.NoError(err, "GetMessage")
		assert.Equal(t, dupID, m.ID, "GetMessage id")
		assert.NotEmpty(t, m.From, "GetMessage must resolve a sender from the single joined from-row")
	})

	t.Run("GetMessagesSummariesByIDs returns each id exactly once", func(t *testing.T) {
		msgs, err := st.GetMessagesSummariesByIDs(ids)
		require.NoError(err, "GetMessagesSummariesByIDs")
		assert.Len(t, msgs, n, "summaries must hold every distinct id once")
		assert.Equal(t, 1, countOccurrences(msgs, dupID),
			"multi-from message must appear exactly once in summaries")
	})
}
