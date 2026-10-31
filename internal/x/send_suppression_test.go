package x

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestXReplySuppressionAcrossSourcesBlocksNewAdmission(t *testing.T) {
	for _, native := range []string{"100", "102"} {
		for _, surface := range []string{"prepare", "admit", "send"} {
			t.Run(native+"/"+surface, func(t *testing.T) {
				f := newSendFixture(t)
				foreign, err := f.st.GetOrCreateSource("x", "2001")
				require.NoError(t, err)
				now := time.Now().UTC()
				_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: foreign.ID, OwnerID: "2001", PostID: native, AuthorID: "1002", ConversationID: "100", EditHistory: []string{native}, Text: "foreign public copy", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: now}})
				require.NoError(t, err)
				execution, err := f.st.AcquireSyncExecutionContext(t.Context(), foreign.ID)
				require.NoError(t, err)
				_, err = f.st.RemoveXPostContentContext(t.Context(), foreign.ID, native, "deleted")
				require.NoError(t, err)
				require.NoError(t, execution.Release())
				// Confirmed suppression has committed before deferred owner cleanup.
				// Every send surface must observe it even through another source alias.
				switch surface {
				case "prepare":
					_, err = f.sender().PrepareReply(t.Context(), f.request())
				case "admit":
					execution, lockErr := f.st.AcquireSyncExecutionContext(t.Context(), f.source.ID)
					require.NoError(t, lockErr)
					_, err = f.st.AdmitXReplyContext(t.Context(), store.XReplyAdmission{SourceID: f.source.ID, OwnerID: "1001", DraftID: f.draft.DraftID, Revision: f.draft.Revision, TargetPostID: "102", Cost: store.XRequestCost{Kind: store.XRequestCreate, OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read", "tweet.write"}, AllowSend: true, MonthlyBudgetMicros: f.policy.MonthlyBudgetMicros, CreateReservationMicros: f.policy.CreateReservationMicros, At: now}})
					require.NoError(t, execution.Release())
				case "send":
					_, err = f.sender().Send(t.Context(), f.request())
				}
				assert.ErrorIs(t, err, store.ErrChatDraftInvalidDestination)
				assert.Zero(t, f.posts.Load(), "committed suppression must prevent a new POST")
				_, receiptErr := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
				assert.ErrorIs(t, receiptErr, store.ErrXReplyReceiptNotFound)
				state, stateErr := f.st.LoadXControlContext(t.Context(), f.source.ID)
				require.NoError(t, stateErr)
				assert.Empty(t, state.Reservations, "refused suppressed target cannot reserve spending")
			})
		}
	}
}

func TestXAcceptedRepairRetainsForeignSuppressionAfterSourceRemoval(t *testing.T) {
	f := newSendFixture(t)
	undo := sendFailure(t, f.st, "message_bodies", "TRUE")
	result, err := f.sender().Send(t.Context(), f.request())
	require.Error(t, err)
	require.Equal(t, "sent_archive_pending", result.Status)
	require.Equal(t, "9001", result.RemoteID)
	undo()
	foreign, err := f.st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), foreign.ID)
	require.NoError(t, err)
	_, err = f.st.RemoveXPostContentContext(t.Context(), foreign.ID, "9001", "removed")
	require.NoError(t, err)
	require.NoError(t, execution.Release())
	sendReadOnly(t, f)
	result, err = f.sender().Send(t.Context(), f.request())
	require.NoError(t, err)
	assert.Equal(t, "sent", result.Status)
	require.NoError(t, f.st.RemoveSource(foreign.ID))
	// Repair must retain native-ID suppression in the sender's surviving source,
	// so removing the other account cannot reopen the accepted post to ingestion.
	target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
	require.NoError(t, err)
	assert.Equal(t, "removed", target.Availability)
	assert.Zero(t, target.MessageID)
	now := time.Now().UTC()
	_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source.ID, OwnerID: "1001", PostID: "9001", AuthorID: "1001", ConversationID: "100", ReplyToPostID: "102", EditHistory: []string{"9001"}, Text: "must stay removed", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1001", FetchedAt: now}})
	require.NoError(t, err)
	target, err = f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
	require.NoError(t, err)
	assert.Zero(t, target.MessageID)
	assert.Equal(t, "removed", target.Availability)
	assert.Equal(t, int32(1), f.posts.Load())
	assert.Zero(t, f.gets.Load())
}

func TestXAdoptionProtectedAuthorQueuesForeignKnownChains(t *testing.T) {
	f := newSendFixture(t)
	foreign, err := f.st.GetOrCreateSource("x", "2001")
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: foreign.ID, OwnerID: "2001", PostID: "500", AuthorID: "1001", ConversationID: "500", EditHistory: []string{"500"}, Text: "known owner copy", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1001", FetchedAt: now}})
	require.NoError(t, err)
	f.post = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"title":"Unknown synthetic outcome"}`)
	}
	result, err := f.sender().Send(t.Context(), f.request())
	require.Error(t, err)
	require.Equal(t, "remote_unknown", result.Status)
	f.get = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"9001","author_id":"1001","conversation_id":"100","text":"owner reply","created_at":"2026-10-31T12:00:00Z","referenced_tweets":[{"type":"replied_to","id":"102"}],"edit_history_tweet_ids":["9001"]}],"includes":{"users":[{"id":"1001","username":"example_owner","protected":true}]}}`)
	}
	pending, err := f.st.PendingXRemovalsContext(t.Context(), foreign.ID)
	require.NoError(t, err)
	require.Empty(t, pending)
	req := f.request()
	req.AdoptPost = "9001"
	prepared, err := f.sender().PrepareReply(t.Context(), req)
	require.NoError(t, err, "the real admitted lookup must reach adoption")
	assert.Equal(t, "adopt", prepared.Mode)
	pending, err = f.st.PendingXRemovalsContext(t.Context(), foreign.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, pending, "confirmed protected-author evidence must queue disjoint known copies")
	receipt, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
	require.NoError(t, err)
	assert.Equal(t, "remote_unknown", receipt.State)
	assert.Equal(t, int32(1), f.posts.Load())
	assert.Equal(t, int32(1), f.gets.Load())
}
