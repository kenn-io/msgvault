package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGCCommentActionRetainsUnresolvedTargetAndAncestors(t *testing.T) {
	for _, state := range []string{"claimed", "dispatched", "unknown", "succeeded", "failed"} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			src, err := st.GetOrCreateSource("facebook_comments", "app:9:page:100")
			require.NoError(err)
			conv, err := st.EnsureConversation(src.ID, "100_200", "Example post")
			require.NoError(err)
			var ids []int64
			for _, nativeID := range []string{"100_200", "100_201", "100_202", "100_203"} {
				id, upsertErr := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: nativeID, MessageType: "facebook_comments"})
				require.NoError(upsertErr)
				ids = append(ids, id)
				require.NoError(st.MarkMessageDeleted(src.ID, nativeID))
			}
			require.NoError(st.SetReplyTo(src.ID, "100_201", "100_200"))
			require.NoError(st.SetReplyTo(src.ID, "100_202", "100_201"))
			row := claimAction(t, st, store.MetaCommentActionClaim{ActionKey: "retained-action", SourceID: src.ID, SourceIdentifier: src.Identifier, MessageID: ids[2], TargetCommentID: "100_202", Action: "hide"})
			if state == "dispatched" {
				row, err = st.MarkMetaCommentActionDispatchedContext(t.Context(), row.ID, row.RowRevision)
				require.NoError(err)
			} else if state != "claimed" {
				row = finishAction(t, st, row, state)
			}
			plan, err := st.PlanGCContext(t.Context())
			require.NoError(err)
			unresolved := state == "claimed" || state == "dispatched" || state == "unknown"
			want := int64(4)
			if unresolved {
				want = 1
				assert.Equal([]int64{ids[3]}, plan.SourceDeletedIDs)
			}
			assert.Equal(want, plan.SourceDeleted)
			deleted, err := st.ExecuteGCContext(t.Context(), plan)
			require.NoError(err)
			assert.Equal(want, deleted)
			if unresolved {
				assert.Equal(ids[:3], remainingGCMessageIDs(t, st))
				target, readErr := st.MetaCommentTargetContext(t.Context(), ids[2])
				require.NoError(readErr)
				assert.Equal(ids[1], target.ParentMessageID)
				assert.Equal("100_201", target.ParentCommentID)
			} else {
				assert.Empty(remainingGCMessageIDs(t, st))
			}
			saved, err := st.GetMetaCommentActionByKeyContext(t.Context(), "retained-action")
			require.NoError(err)
			assert.Equal(row.ID, saved.ID)
			assert.Equal(unresolved, saved.MessageID.Valid)
		})
	}
}

func TestGCCommentRetainsDeletedAncestorsOfLiveComments(t *testing.T) {
	for _, provider := range []string{"facebook_comments", "instagram_comments", "gmail"} {
		t.Run(provider, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			identifier := "gc@example.com"
			switch provider {
			case "facebook_comments":
				identifier = "app:9:page:100"
			case "instagram_comments":
				identifier = "app:9:page:100:instagram:700"
			}
			src, err := st.GetOrCreateSource(provider, identifier)
			require.NoError(err)
			conv, err := st.EnsureConversation(src.ID, "synthetic-gc-thread", "Example thread")
			require.NoError(err)
			messageType := provider
			if provider == "gmail" {
				messageType = "email"
			}
			var ids []int64
			for _, nativeID := range []string{"parent", "middle", "survivor", "unrelated"} {
				id, upsertErr := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: nativeID, MessageType: messageType})
				require.NoError(upsertErr)
				ids = append(ids, id)
			}
			require.NoError(st.SetReplyTo(src.ID, "middle", "parent"))
			require.NoError(st.SetReplyTo(src.ID, "survivor", "middle"))
			for _, nativeID := range []string{"parent", "middle", "unrelated"} {
				require.NoError(st.MarkMessageDeleted(src.ID, nativeID))
			}
			plan, err := st.PlanGCContext(t.Context())
			require.NoError(err)
			want := int64(1)
			if provider == "gmail" {
				want = 3
			}
			assert.Equal(want, plan.SourceDeleted)
			deleted, err := st.ExecuteGCContext(t.Context(), plan)
			require.NoError(err)
			assert.Equal(want, deleted)
			if provider != "gmail" {
				assert.Equal(ids[:3], remainingGCMessageIDs(t, st))
				target, targetErr := st.MetaCommentTargetContext(t.Context(), ids[2])
				require.NoError(targetErr)
				assert.Equal(ids[1], target.ParentMessageID)
				assert.Equal("middle", target.ParentCommentID)
			} else {
				assert.Equal(ids[2:3], remainingGCMessageIDs(t, st))
			}
			require.NoError(st.MarkMessageDeleted(src.ID, "survivor"))
			plan, err = st.PlanGCContext(t.Context())
			require.NoError(err)
			_, err = st.ExecuteGCContext(t.Context(), plan)
			require.NoError(err)
			assert.Empty(remainingGCMessageIDs(t, st), "ancestor retention ends when no live descendant needs it")
		})
	}
}
