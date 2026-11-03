package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCommentTriageSurvivesSourceMerge(t *testing.T) {
	for _, state := range []store.CommentTriageState{store.CommentTriageHandled, store.CommentTriageSkipped} {
		for _, destination := range []string{"empty", "same-state", "ordinary-name", "renamed-state", "incoming-ordinary"} {
			t.Run(string(state)+"/"+destination, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				st, incoming, incomingRoot := queueFixture(t)
				_, err := st.SetCommentTriageContext(t.Context(), incoming.MessageID, state, nil)
				require.NoError(err)
				into, err := st.GetOrCreateSource("facebook_comments", "app:9:page:101")
				require.NoError(err)
				conv, err := st.EnsureConversation(into.ID, "101_201", "Example destination post")
				require.NoError(err)
				root, err := st.UpsertMessage(&store.Message{SourceID: into.ID, ConversationID: conv, SourceMessageID: "101_201", MessageType: "facebook_comments"})
				require.NoError(err)
				existing, err := st.UpsertMessage(&store.Message{SourceID: into.ID, ConversationID: conv, SourceMessageID: "101_201_301", MessageType: "facebook_comments", ReplyToMessageID: sql.NullInt64{Int64: root, Valid: true}})
				require.NoError(err)
				name := "Handled"
				if state == store.CommentTriageSkipped {
					name = "Skipped"
				}
				var ordinaryIncoming int64
				switch destination {
				case "same-state", "renamed-state", "incoming-ordinary":
					_, err = st.SetCommentTriageContext(t.Context(), existing, state, nil)
					require.NoError(err)
					if destination == "renamed-state" {
						_, err = st.DB().Exec(st.Rebind("UPDATE labels SET name=? WHERE source_id=? AND source_label_id=?"), "Reviewed comments", into.ID, "msgvault:triage:"+string(state))
						require.NoError(err)
					}
					if destination == "incoming-ordinary" {
						_, err = st.DB().Exec(st.Rebind("UPDATE labels SET name=? WHERE source_id=? AND source_label_id=?"), "Reviewed comments", incoming.SourceID, "msgvault:triage:"+string(state))
						require.NoError(err)
						var incomingConv int64
						require.NoError(st.DB().QueryRow(st.Rebind("SELECT conversation_id FROM messages WHERE id=?"), incomingRoot).Scan(&incomingConv))
						ordinaryIncoming, err = st.UpsertMessage(&store.Message{SourceID: incoming.SourceID, ConversationID: incomingConv, SourceMessageID: "100_200_302", MessageType: "facebook_comments", ReplyToMessageID: sql.NullInt64{Int64: incomingRoot, Valid: true}})
						require.NoError(err)
						label, err := st.EnsureLabel(incoming.SourceID, "ordinary", name, "user")
						require.NoError(err)
						require.NoError(st.AddMessageLabels(ordinaryIncoming, []int64{label}))
					}
				case "ordinary-name":
					label, err := st.EnsureLabel(into.ID, "ordinary", name, "user")
					require.NoError(err)
					require.NoError(st.AddMessageLabels(existing, []int64{label}))
				}
				_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: incoming.SourceID, IntoSourceID: into.ID})
				require.NoError(err)
				got, err := st.CommentTriageContext(t.Context(), incoming.MessageID)
				require.NoError(err)
				assert.Equal(state, got)
				queue, err := st.ListCommentQueueContext(t.Context(), store.CommentQueueFilter{SourceID: into.ID, State: string(state)})
				require.NoError(err)
				ids := make([]int64, 0, len(queue))
				for _, item := range queue {
					ids = append(ids, item.MessageID)
				}
				assert.Contains(ids, incoming.MessageID)
				if destination == "ordinary-name" {
					got, err = st.CommentTriageContext(t.Context(), existing)
					require.NoError(err)
					assert.Equal(store.CommentTriageOpen, got)
				}
				if ordinaryIncoming != 0 {
					got, err = st.CommentTriageContext(t.Context(), ordinaryIncoming)
					require.NoError(err)
					assert.Equal(store.CommentTriageOpen, got)
				}
				open := store.CommentTriageOpen
				_, err = st.SetCommentTriageContext(t.Context(), incoming.MessageID, open, &open)
				require.ErrorIs(err, store.ErrCommentTriageConflict)
				_, err = st.SetCommentTriageContext(t.Context(), incoming.MessageID, open, &state)
				require.NoError(err)
				got, err = st.CommentTriageContext(t.Context(), incoming.MessageID)
				require.NoError(err)
				assert.Equal(open, got)
				_, err = st.SetCommentTriageContext(t.Context(), incoming.MessageID, state, &open)
				require.NoError(err)
				if destination == "ordinary-name" {
					got, err = st.CommentTriageContext(t.Context(), existing)
					require.NoError(err)
					assert.Equal(open, got, "triage changes must not adopt an ordinary label with the same display name")
				}
			})
		}
	}
}
