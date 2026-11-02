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
				_testAssert := assert.New(t)
				_testRequire := require.New(t)

				f := newSendFixture(t)
				foreign, err := f.st.GetOrCreateSource("x", "2001")
				_testRequire.NoError(err)
				now := time.Now().UTC()
				_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: foreign.ID, OwnerID: "2001", PostID: native, AuthorID: "1002", ConversationID: "100", EditHistory: []string{native}, Text: "foreign public copy", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1002", FetchedAt: now}})
				_testRequire.NoError(err)
				execution, err := f.st.AcquireSyncExecutionContext(t.Context(), foreign.ID)
				_testRequire.NoError(err)
				_, err = f.st.RemoveXPostContentContext(t.Context(), foreign.ID, native, "deleted")
				_testRequire.NoError(err)
				_testRequire.NoError(execution.Release())
				// Confirmed suppression has committed before deferred owner cleanup.
				// Every send surface must observe it even through another source alias.
				switch surface {
				case "prepare":
					_, err = f.sender().PrepareReply(t.Context(), f.request())
				case "admit":
					execution, lockErr := f.st.AcquireSyncExecutionContext(t.Context(), f.source.ID)
					_testRequire.NoError(lockErr)
					_, err = f.st.AdmitXReplyContext(t.Context(), store.XReplyAdmission{SourceID: f.source.ID, OwnerID: "1001", DraftID: f.draft.DraftID, Revision: f.draft.Revision, TargetPostID: "102", Cost: store.XRequestCost{Kind: store.XRequestCreate, OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read", "tweet.write"}, AllowSend: true, MonthlyBudgetMicros: f.policy.MonthlyBudgetMicros, CreateReservationMicros: f.policy.CreateReservationMicros, At: now}})
					_testRequire.NoError(execution.Release())
				case "send":
					_, err = f.sender().Send(t.Context(), f.request())
				}
				_testRequire.ErrorIs(err, store.ErrChatDraftInvalidDestination)
				_testAssert.Zero(f.posts.Load(), "committed suppression must prevent a new POST")
				_, receiptErr := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
				_testRequire.ErrorIs(receiptErr, store.ErrXReplyReceiptNotFound)
				state, stateErr := f.st.LoadXControlContext(t.Context(), f.source.ID)
				_testRequire.NoError(stateErr)
				_testAssert.Empty(state.Reservations, "refused suppressed target cannot reserve spending")
			})
		}
	}
}

func TestXAcceptedRepairRetainsForeignSuppressionAfterSourceRemoval(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newSendFixture(t)
	undo := sendFailure(t, f.st, "message_bodies", "TRUE")
	result, err := f.sender().Send(t.Context(), f.request())
	_testRequire.Error(err)
	_testRequire.Equal("sent_archive_pending", result.Status)
	_testRequire.Equal("9001", result.RemoteID)
	undo()
	foreign, err := f.st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), foreign.ID)
	_testRequire.NoError(err)
	_, err = f.st.RemoveXPostContentContext(t.Context(), foreign.ID, "9001", "removed")
	_testRequire.NoError(err)
	_testRequire.NoError(execution.Release())
	sendReadOnly(t, f)
	result, err = f.sender().Send(t.Context(), f.request())
	_testRequire.NoError(err)
	_testAssert.Equal("sent", result.Status)
	_testRequire.NoError(f.st.RemoveSource(foreign.ID))
	// Repair must retain native-ID suppression in the sender's surviving source,
	// so removing the other account cannot reopen the accepted post to ingestion.
	target, err := f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
	_testRequire.NoError(err)
	_testAssert.Equal("removed", target.Availability)
	_testAssert.Zero(target.MessageID)
	now := time.Now().UTC()
	_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: f.source.ID, OwnerID: "1001", PostID: "9001", AuthorID: "1001", ConversationID: "100", ReplyToPostID: "102", EditHistory: []string{"9001"}, Text: "must stay removed", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1001", FetchedAt: now}})
	_testRequire.NoError(err)
	target, err = f.st.ResolveXPostContext(t.Context(), f.source.ID, "9001")
	_testRequire.NoError(err)
	_testAssert.Zero(target.MessageID)
	_testAssert.Equal("removed", target.Availability)
	_testAssert.Equal(int32(1), f.posts.Load())
	_testAssert.Zero(f.gets.Load())
}

func TestXAdoptionProtectedAuthorQueuesForeignKnownChains(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newSendFixture(t)
	foreign, err := f.st.GetOrCreateSource("x", "2001")
	_testRequire.NoError(err)
	now := time.Now().UTC()
	_, err = f.st.PersistXPostContext(t.Context(), store.XPersistPostInput{SourceID: foreign.ID, OwnerID: "2001", PostID: "500", AuthorID: "1001", ConversationID: "500", EditHistory: []string{"500"}, Text: "known owner copy", CreatedAt: now, FetchedAt: now, Availability: "public", Author: &store.XAuthorInput{UserID: "1001", FetchedAt: now}})
	_testRequire.NoError(err)
	f.post = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, `{"title":"Unknown synthetic outcome"}`)
	}
	result, err := f.sender().Send(t.Context(), f.request())
	_testRequire.Error(err)
	_testRequire.Equal("remote_unknown", result.Status)
	f.get = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[{"id":"9001","author_id":"1001","conversation_id":"100","text":"owner reply","created_at":"2026-10-31T12:00:00Z","referenced_tweets":[{"type":"replied_to","id":"102"}],"edit_history_tweet_ids":["9001"]}],"includes":{"users":[{"id":"1001","username":"example_owner","protected":true}]}}`)
	}
	pending, err := f.st.PendingXRemovalsContext(t.Context(), foreign.ID)
	_testRequire.NoError(err)
	_testRequire.Empty(pending)
	req := f.request()
	req.AdoptPost = "9001"
	prepared, err := f.sender().PrepareReply(t.Context(), req)
	_testRequire.NoError(err, "the real admitted lookup must reach adoption")
	_testAssert.Equal("adopt", prepared.Mode)
	pending, err = f.st.PendingXRemovalsContext(t.Context(), foreign.ID)
	_testRequire.NoError(err)
	_testAssert.NotEmpty(pending, "confirmed protected-author evidence must queue disjoint known copies")
	receipt, err := f.st.GetXReplyReceiptContext(t.Context(), f.draft.DraftID)
	_testRequire.NoError(err)
	_testAssert.Equal("remote_unknown", receipt.State)
	_testAssert.Equal(int32(1), f.posts.Load())
	_testAssert.Equal(int32(1), f.gets.Load())
}
