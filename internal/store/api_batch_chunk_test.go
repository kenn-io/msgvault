package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBatchGetRecipientsAndLabels_ChunkLargeIDSets is the regression for the
// SQLite bound-parameter ceiling in batchGetRecipients/batchGetLabels: one
// message id is one bound parameter in each function's IN-list, and eval's
// FTS mode over-fetches a ranked page well past batchQueryIDChunk at a large
// -n (see rankedFTS / eval.OverFetchPlan). A message on each side of the
// chunk boundary carries a recipient and a label, so a bug that silently
// dropped the second chunk, or mis-merged results across chunks, would leave
// one of the two empty instead of populated.
func TestBatchGetRecipientsAndLabels_ChunkLargeIDSets(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := openTestStore(t)
	ctx := context.Background()

	src, err := st.GetOrCreateSource("gmail", "batchchunk@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-batchchunk", "Thread BatchChunk")
	require.NoError(err, "EnsureConversation")
	aliceID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err, "EnsureParticipant alice")
	bobID, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err, "EnsureParticipant bob")

	const total = batchQueryIDChunk + 3
	sentAt := time.Date(2024, 9, 2, 12, 0, 0, 0, time.UTC)
	ids := make([]int64, total)
	for i := range total {
		id, err := st.UpsertMessage(&Message{
			ConversationID:  convID,
			SourceID:        src.ID,
			SourceMessageID: fmt.Sprintf("batchchunk-msg-%d", i),
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: sentAt, Valid: true},
			Subject:         sql.NullString{String: "batchchunk", Valid: true},
			SizeEstimate:    100,
		})
		require.NoError(err, "UpsertMessage %d", i)
		ids[i] = id
	}

	require.NoError(st.ReplaceMessageRecipients(ids[0], "to", []int64{aliceID}, []string{"Alice"}),
		"recipient on the first chunk's message")
	require.NoError(st.ReplaceMessageRecipients(ids[total-1], "to", []int64{bobID}, []string{"Bob"}),
		"recipient on the second chunk's message")

	firstLabelID, err := st.EnsureLabel(src.ID, "first-chunk", "first-chunk", "user")
	require.NoError(err, "EnsureLabel first-chunk")
	require.NoError(st.LinkMessageLabel(ids[0], firstLabelID))
	secondLabelID, err := st.EnsureLabel(src.ID, "second-chunk", "second-chunk", "user")
	require.NoError(err, "EnsureLabel second-chunk")
	require.NoError(st.LinkMessageLabel(ids[total-1], secondLabelID))

	recipients, err := st.batchGetRecipients(ctx, ids, "to")
	require.NoError(err, "batchGetRecipients")
	assert.Equal([]string{"Alice <alice@example.com>"}, recipients[ids[0]],
		"a message hydrated in the first recipient chunk must carry its recipient")
	assert.Equal([]string{"Bob <bob@example.com>"}, recipients[ids[total-1]],
		"a message hydrated in the second recipient chunk must carry its recipient too")

	labels, err := st.batchGetLabels(ctx, ids)
	require.NoError(err, "batchGetLabels")
	assert.Equal([]string{"first-chunk"}, labels[ids[0]],
		"a message hydrated in the first label chunk must carry its label")
	assert.Equal([]string{"second-chunk"}, labels[ids[total-1]],
		"a message hydrated in the second label chunk must carry its label too")
}
