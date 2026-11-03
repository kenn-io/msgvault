package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCommentTriageDoesNotAdoptOrdinaryLabels(t *testing.T) {
	for _, state := range []store.CommentTriageState{store.CommentTriageHandled, store.CommentTriageSkipped} {
		for _, collisions := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/%d", state, collisions), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st, target, root := queueFixture(t)
				var conv int64
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_id FROM messages WHERE id=?`), root).Scan(&conv))
				other, err := st.UpsertMessage(&store.Message{SourceID: target.SourceID, ConversationID: conv, SourceMessageID: "100_200_301", MessageType: "facebook_comments", ReplyToMessageID: sql.NullInt64{Int64: root, Valid: true}})
				require.NoError(err)
				baseName := "Handled"
				if state == store.CommentTriageSkipped {
					baseName = "Skipped"
				}
				var labels []int64
				for i := range collisions {
					name := baseName
					if i > 0 {
						name = fmt.Sprintf("%s (comment triage %d)", baseName, i)
					}
					label, createErr := st.EnsureLabel(target.SourceID, fmt.Sprintf("ordinary-%d", i), name, "user")
					require.NoError(createErr)
					require.NoError(st.AddMessageLabels(other, []int64{label}))
					labels = append(labels, label)
				}
				for range 2 {
					_, err = st.SetCommentTriageContext(t.Context(), target.MessageID, state, nil)
					require.NoError(err)
				}
				got, err := st.CommentTriageContext(t.Context(), other)
				require.NoError(err)
				assert.Equal(store.CommentTriageOpen, got, "triage must affect only the selected comment")
				queue, err := st.ListCommentQueueContext(t.Context(), store.CommentQueueFilter{SourceID: target.SourceID, State: string(state)})
				require.NoError(err)
				require.Len(queue, 1)
				assert.Equal(target.MessageID, queue[0].MessageID)
				for i, label := range labels {
					var key string
					require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_label_id FROM labels WHERE id=?`), label).Scan(&key))
					assert.Equal(fmt.Sprintf("ordinary-%d", i), key)
				}
				var reserved int
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM labels WHERE source_id=? AND source_label_id=?`), target.SourceID, "msgvault:triage:"+string(state)).Scan(&reserved))
				assert.Equal(1, reserved, "repeated triage reuses the reserved label")
			})
		}
	}
}
