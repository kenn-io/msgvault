package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestXDraftRequiresReviewedTarget(t *testing.T) {
	st, source := xPostFixture(t)
	p := xPostInput(source, "102", "100", "reviewed parent", "100", "102")
	saved, err := st.PersistXPostContext(t.Context(), p)
	require.NoError(t, err)
	target, err := st.ResolveXPostContext(t.Context(), source, "100")
	require.NoError(t, err)
	draft, err := st.CreateChatDraftContext(t.Context(), target.ConversationID, saved.MessageID, "reviewed reply", allowChatDraft)
	require.NoError(t, err)
	assert.Equal(t, "102", draft.ReplyToSourceMessageID)
	assert.Equal(t, "100", draft.SourceConversationID)
	_, err = st.CreateChatDraftContext(t.Context(), target.ConversationID, 0, "no parent", allowChatDraft)
	require.ErrorIs(t, err, store.ErrChatDraftInvalidDestination)
}

func xReplyFixture(t *testing.T) (*store.Store, store.ChatDraft, store.XReplyAdmission) {
	t.Helper()
	st, source := xPostFixture(t)
	p := xPostInput(source, "102", "100", "reviewed parent", "100", "102")
	saved, err := st.PersistXPostContext(t.Context(), p)
	require.NoError(t, err)
	target, err := st.ResolveXPostContext(t.Context(), source, "100")
	require.NoError(t, err)
	d, err := st.CreateChatDraftContext(t.Context(), target.ConversationID, saved.MessageID, "reviewed reply", allowChatDraft)
	require.NoError(t, err)
	generation := int64(1)
	_, err = st.UpdateXControlContext(t.Context(), source, 0, store.XControlPatch{OwnerID: "1001", Generation: &generation})
	require.NoError(t, err)
	cost := store.XRequestCost{Kind: store.XRequestCreate, OwnerID: "1001", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read", "tweet.write"}, AllowSend: true, MonthlyBudgetMicros: 5_000_000, CreateReservationMicros: 200_000, At: xPostTime}
	return st, d, store.XReplyAdmission{SourceID: source, OwnerID: "1001", DraftID: d.DraftID, Revision: d.Revision, TargetPostID: d.ReplyToSourceMessageID, Cost: cost}
}

func TestXDraftAdmissionFreezesAndRetainsReceipt(t *testing.T) {
	st, d, input := xReplyFixture(t)
	admitted, err := st.AdmitXReplyContext(t.Context(), input)
	require.NoError(t, err)
	require.True(t, admitted.Created)
	a := admitted.Attempt
	assert.Equal(t, "admitted", a.State)
	assert.Equal(t, "102", a.TargetPostID)
	assert.NotContains(t, a.BodyDigest, d.Body)
	assert.Len(t, a.BodyDigest, 64)
	assert.Equal(t, "94b0a8e85213be7ec46b6aeae003393b842e2f10fcd934d758ab1e382c16ccfb", a.BodyDigest) // SHA256 of the exact reviewed text.
	assert.Equal(t, int64(200_000), a.ReservedMicros)
	state, err := st.LoadXControlContext(t.Context(), d.SourceID)
	require.NoError(t, err)
	require.Len(t, state.Reservations, 1)
	assert.Equal(t, a.ReservationID, state.Reservations[0].ID)
	again, err := st.AdmitXReplyContext(t.Context(), input)
	require.NoError(t, err)
	assert.False(t, again.Created)
	assert.Equal(t, a.AttemptID, again.Attempt.AttemptID)
	_, err = st.UpdateChatDraftContext(t.Context(), d.DraftID, 1, "changed")
	require.ErrorIs(t, err, store.ErrXReplyFrozen)
	require.ErrorIs(t, st.DeleteChatDraftContext(t.Context(), d.DraftID, 1), store.ErrXReplyFrozen)
	require.NoError(t, st.RecordXReplyAcceptedContext(t.Context(), a.XAttemptKey, "9001"))
	require.NoError(t, st.RecordXReplyAcceptedContext(t.Context(), a.XAttemptKey, "9001"))
	require.ErrorIs(t, st.RecordXReplyAcceptedContext(t.Context(), a.XAttemptKey, "9002"), store.ErrXAttemptState)
	require.ErrorIs(t, st.RejectXReplyContext(t.Context(), a.XAttemptKey, "provider_rejected"), store.ErrXAttemptState)
	done, err := st.FinishXReplyContext(t.Context(), a.XAttemptKey)
	require.NoError(t, err)
	assert.Equal(t, "sent", done.State)
	assert.Equal(t, "9001", done.RemoteID)
	_, err = st.GetChatDraftContext(t.Context(), d.DraftID)
	require.ErrorIs(t, err, store.ErrChatDraftNotFound)
	receipt, err := st.GetXReplyReceiptContext(t.Context(), d.DraftID)
	require.NoError(t, err)
	assert.Equal(t, done, receipt)
	done2, err := st.FinishXReplyContext(t.Context(), a.XAttemptKey)
	require.NoError(t, err)
	assert.Equal(t, done, done2)
	state, err = st.LoadXControlContext(t.Context(), d.SourceID)
	require.NoError(t, err)
	assert.Zero(t, state.SpendBuckets[0].ReservedMicros)
	assert.Equal(t, int64(200_000), state.SpendBuckets[0].SpentMicros)
	target, err := st.ResolveXPostContext(t.Context(), d.SourceID, "9001")
	require.NoError(t, err)
	body, err := st.GetMessageBodyText(target.MessageID)
	require.NoError(t, err)
	assert.Equal(t, d.Body, body)
	// Receipts are independent of drafts, but source removal owns their lifetime.
	_, err = st.DB().Exec(st.Rebind("DELETE FROM sources WHERE id=?"), d.SourceID)
	require.NoError(t, err)
	_, err = st.GetXReplyReceiptContext(t.Context(), d.DraftID)
	require.ErrorIs(t, err, store.ErrXReplyReceiptNotFound)
}

func TestXDraftChangedAndInvalidAdmissionRollBack(t *testing.T) {
	for _, mode := range []string{"revision", "target", "owner", "budget", "unverified", "consolidated"} {
		t.Run(mode, func(t *testing.T) {
			st, d, input := xReplyFixture(t)
			expected := store.ErrXTargetChanged
			switch mode {
			case "revision":
				input.Revision++
				expected = store.ErrChatDraftRevisionConflict
			case "target":
				_, err := st.PersistXPostContext(t.Context(), xPostInput(d.SourceID, "103", "100", "edited", "100", "102", "103"))
				require.NoError(t, err)
			case "owner":
				input.OwnerID = "2001"
				expected = store.ErrXBudgetIdentity
			case "budget":
				input.Cost.MonthlyBudgetMicros = 199_999
				expected = store.ErrXBudgetExceeded
			case "unverified":
				_, err := st.DB().Exec(st.Rebind("UPDATE x_post_versions SET availability='unverified' WHERE source_id=?"), d.SourceID)
				require.NoError(t, err)
				expected = store.ErrChatDraftInvalidDestination
			case "consolidated":
				_, err := st.DB().Exec(st.Rebind("UPDATE conversations SET source_conversation_id='99' WHERE id=?"), d.ConversationID)
				require.NoError(t, err)
			}
			_, err := st.AdmitXReplyContext(t.Context(), input)
			require.ErrorIs(t, err, expected)
			state, err := st.LoadXControlContext(t.Context(), d.SourceID)
			require.NoError(t, err)
			assert.Empty(t, state.Reservations)
			_, err = st.GetXReplyReceiptContext(t.Context(), d.DraftID)
			require.ErrorIs(t, err, store.ErrXReplyReceiptNotFound)
			_, err = st.UpdateChatDraftContext(t.Context(), d.DraftID, d.Revision, "still editable")
			require.NoError(t, err)
		})
	}
}

func TestXDraftRejectedHistoryAndStaleAttemptKeys(t *testing.T) {
	st, d, input := xReplyFixture(t)
	first, err := st.AdmitXReplyContext(t.Context(), input)
	require.NoError(t, err)
	require.NoError(t, st.RejectXReplyContext(t.Context(), first.Attempt.XAttemptKey, "provider_rejected"))
	edited, err := st.UpdateChatDraftContext(t.Context(), d.DraftID, d.Revision, "new review")
	require.NoError(t, err)
	input.Revision = edited.Revision
	second, err := st.AdmitXReplyContext(t.Context(), input)
	require.NoError(t, err)
	require.True(t, second.Created)
	assert.NotEqual(t, first.Attempt.AttemptID, second.Attempt.AttemptID)
	require.ErrorIs(t, st.RecordXReplyAcceptedContext(t.Context(), first.Attempt.XAttemptKey, "9001"), store.ErrXAttemptState)
	require.NoError(t, st.RejectXReplyContext(t.Context(), first.Attempt.XAttemptKey, "provider_rejected"))
	wrong := second.Attempt.XAttemptKey
	wrong.SourceID++
	require.ErrorIs(t, st.MarkXReplyUnknownContext(t.Context(), wrong), store.ErrXAttemptState)
	require.NoError(t, st.MarkXReplyUnknownContext(t.Context(), second.Attempt.XAttemptKey))
	require.ErrorIs(t, st.RejectXReplyContext(t.Context(), second.Attempt.XAttemptKey, "provider_rejected"), store.ErrXAttemptState)
	again, err := st.AdmitXReplyContext(t.Context(), input)
	require.NoError(t, err)
	assert.False(t, again.Created)
	assert.Equal(t, "remote_unknown", again.Attempt.State)
	state, err := st.LoadXControlContext(t.Context(), d.SourceID)
	require.NoError(t, err)
	assert.Equal(t, int64(400_000), state.SpendBuckets[0].ReservedMicros, "no unproved rejection refund")
}
