package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestXDraftRequiresReviewedTarget(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, source := xPostFixture(t)
	p := xPostInput(source, "102", "100", "reviewed parent", "100", "102")
	saved, err := st.PersistXPostContext(t.Context(), p)
	_testRequire.NoError(err)
	target, err := st.ResolveXPostContext(t.Context(), source, "100")
	_testRequire.NoError(err)
	draft, err := st.CreateChatDraftContext(t.Context(), target.ConversationID, saved.MessageID, "reviewed reply", allowChatDraft)
	_testRequire.NoError(err)
	_testAssert.Equal("102", draft.ReplyToSourceMessageID)
	_testAssert.Equal("100", draft.SourceConversationID)
	_, err = st.CreateChatDraftContext(t.Context(), target.ConversationID, 0, "no parent", allowChatDraft)
	_testRequire.ErrorIs(err, store.ErrChatDraftInvalidDestination)
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
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, d, input := xReplyFixture(t)
	admitted, err := st.AdmitXReplyContext(t.Context(), input)
	_testRequire.NoError(err)
	_testRequire.True(admitted.Created)
	a := admitted.Attempt
	_testAssert.Equal("admitted", a.State)
	_testAssert.Equal("102", a.TargetPostID)
	_testAssert.NotContains(a.BodyDigest, d.Body)
	_testAssert.Len(a.BodyDigest, 64)
	_testAssert.Equal("94b0a8e85213be7ec46b6aeae003393b842e2f10fcd934d758ab1e382c16ccfb", a.BodyDigest)
	_testAssert.

		// SHA256 of the exact reviewed text.
		Equal(int64(200_000), a.ReservedMicros)
	state, err := st.LoadXControlContext(t.Context(), d.SourceID)
	_testRequire.NoError(err)
	_testRequire.Len(state.Reservations, 1)
	_testAssert.Equal(a.ReservationID, state.Reservations[0].ID)
	again, err := st.AdmitXReplyContext(t.Context(), input)
	_testRequire.NoError(err)
	_testAssert.False(again.Created)
	_testAssert.Equal(a.AttemptID, again.Attempt.AttemptID)
	_, err = st.UpdateChatDraftContext(t.Context(), d.DraftID, 1, "changed")
	_testRequire.ErrorIs(err, store.ErrXReplyFrozen)
	_testRequire.ErrorIs(st.DeleteChatDraftContext(t.Context(), d.DraftID, 1), store.ErrXReplyFrozen)
	_testRequire.NoError(st.RecordXReplyAcceptedContext(t.Context(), a.XAttemptKey, "9001"))
	_testRequire.NoError(st.RecordXReplyAcceptedContext(t.Context(), a.XAttemptKey, "9001"))
	_testRequire.ErrorIs(st.RecordXReplyAcceptedContext(t.Context(), a.XAttemptKey, "9002"), store.ErrXAttemptState)
	_testRequire.ErrorIs(st.RejectXReplyContext(t.Context(), a.XAttemptKey, "provider_rejected"), store.ErrXAttemptState)
	done, err := st.FinishXReplyContext(t.Context(), a.XAttemptKey)
	_testRequire.NoError(err)
	_testAssert.Equal("sent", done.State)
	_testAssert.Equal("9001", done.RemoteID)
	_, err = st.GetChatDraftContext(t.Context(), d.DraftID)
	_testRequire.ErrorIs(err, store.ErrChatDraftNotFound)
	receipt, err := st.GetXReplyReceiptContext(t.Context(), d.DraftID)
	_testRequire.NoError(err)
	_testAssert.Equal(done, receipt)
	done2, err := st.FinishXReplyContext(t.Context(), a.XAttemptKey)
	_testRequire.NoError(err)
	_testAssert.Equal(done, done2)
	state, err = st.LoadXControlContext(t.Context(), d.SourceID)
	_testRequire.NoError(err)
	_testAssert.Zero(state.SpendBuckets[0].ReservedMicros)
	_testAssert.Equal(int64(200_000), state.SpendBuckets[0].SpentMicros)
	target, err := st.ResolveXPostContext(t.Context(), d.SourceID, "9001")
	_testRequire.NoError(err)
	body, err := st.GetMessageBodyText(t.Context(), target.MessageID)
	_testRequire.NoError(err)
	_testAssert.Equal(d.Body, body)
	// Receipts are independent of drafts, but source removal owns their lifetime.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("DELETE FROM sources WHERE id=?"), d.SourceID)
	_testRequire.NoError(err)
	_, err = st.GetXReplyReceiptContext(t.Context(), d.DraftID)
	_testRequire.ErrorIs(err, store.ErrXReplyReceiptNotFound)
}

func TestXDraftChangedAndInvalidAdmissionRollBack(t *testing.T) {
	for _, mode := range []string{"revision", "target", "owner", "budget", "unverified", "consolidated"} {
		t.Run(mode, func(t *testing.T) {
			_testRequire := require.New(t)

			st, d, input := xReplyFixture(t)
			expected := store.ErrXTargetChanged
			switch mode {
			case "revision":
				input.Revision++
				expected = store.ErrChatDraftRevisionConflict
			case "target":
				_, err := st.PersistXPostContext(t.Context(), xPostInput(d.SourceID, "103", "100", "edited", "100", "102", "103"))
				_testRequire.NoError(err)
			case "owner":
				input.OwnerID = "2001"
				expected = store.ErrXBudgetIdentity
			case "budget":
				input.Cost.MonthlyBudgetMicros = 199_999
				expected = store.ErrXBudgetExceeded
			case "unverified":
				_, err := st.DB().ExecContext(t.Context(), st.Rebind("UPDATE x_post_versions SET availability='unverified' WHERE source_id=?"), d.SourceID)
				_testRequire.NoError(err)
				expected = store.ErrChatDraftInvalidDestination
			case "consolidated":
				_, err := st.DB().ExecContext(t.Context(), st.Rebind("UPDATE conversations SET source_conversation_id='99' WHERE id=?"), d.ConversationID)
				_testRequire.NoError(err)
			}
			_, err := st.AdmitXReplyContext(t.Context(), input)
			_testRequire.ErrorIs(err, expected)
			state, err := st.LoadXControlContext(t.Context(), d.SourceID)
			_testRequire.NoError(err)
			assert.Empty(t, state.Reservations)
			_, err = st.GetXReplyReceiptContext(t.Context(), d.DraftID)
			_testRequire.ErrorIs(err, store.ErrXReplyReceiptNotFound)
			_, err = st.UpdateChatDraftContext(t.Context(), d.DraftID, d.Revision, "still editable")
			_testRequire.NoError(err)
		})
	}
}

func TestXDraftRejectedHistoryAndStaleAttemptKeys(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, d, input := xReplyFixture(t)
	first, err := st.AdmitXReplyContext(t.Context(), input)
	_testRequire.NoError(err)
	_testRequire.NoError(st.RejectXReplyContext(t.Context(), first.Attempt.XAttemptKey, "provider_rejected"))
	edited, err := st.UpdateChatDraftContext(t.Context(), d.DraftID, d.Revision, "new review")
	_testRequire.NoError(err)
	input.Revision = edited.Revision
	second, err := st.AdmitXReplyContext(t.Context(), input)
	_testRequire.NoError(err)
	_testRequire.True(second.Created)
	_testAssert.NotEqual(first.Attempt.AttemptID, second.Attempt.AttemptID)
	_testRequire.ErrorIs(st.RecordXReplyAcceptedContext(t.Context(), first.Attempt.XAttemptKey, "9001"), store.ErrXAttemptState)
	_testRequire.NoError(st.RejectXReplyContext(t.Context(), first.Attempt.XAttemptKey, "provider_rejected"))
	wrong := second.Attempt.XAttemptKey
	wrong.SourceID++
	_testRequire.ErrorIs(st.MarkXReplyUnknownContext(t.Context(), wrong), store.ErrXAttemptState)
	_testRequire.NoError(st.MarkXReplyUnknownContext(t.Context(), second.Attempt.XAttemptKey))
	_testRequire.ErrorIs(st.RejectXReplyContext(t.Context(), second.Attempt.XAttemptKey, "provider_rejected"), store.ErrXAttemptState)
	again, err := st.AdmitXReplyContext(t.Context(), input)
	_testRequire.NoError(err)
	_testAssert.False(again.Created)
	_testAssert.Equal("remote_unknown", again.Attempt.State)
	state, err := st.LoadXControlContext(t.Context(), d.SourceID)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(200_000), state.SpendBuckets[0].ReservedMicros, "unknown send remains reserved")
	_testAssert.Equal(int64(200_000), state.SpendBuckets[0].SpentMicros, "rejection is conservatively charged")
}

func TestXRejectedRepliesSettleWithoutExhaustingReceiptCapacity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, d, input := xReplyFixture(t)
	input.Cost.MonthlyBudgetMicros = 30_000_000
	input.Cost.At = time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC)
	state, err := st.LoadXControlContext(t.Context(), d.SourceID)
	requirements.NoError(err)
	job := store.XArchiveJob{ID: "archive-send", Fingerprint: "frozen", Username: "example_user", After: input.Cost.At.Add(-24 * time.Hour), Before: input.Cost.At, CapMicros: 60_000_000,
		Own:      &store.XWalk{Fingerprint: "own", Endpoint: "search_all", Query: "from:example_user -is:retweet", After: input.Cost.At.Add(-24 * time.Hour), Before: input.Cost.At},
		Mentions: &store.XWalk{Fingerprint: "mentions", Endpoint: "search_all", Query: "to:example_user -is:retweet", After: input.Cost.At.Add(-24 * time.Hour), Before: input.Cost.At}}
	_, err = st.UpdateXControlContext(t.Context(), d.SourceID, state.Revision, store.XControlPatch{OwnerID: "1001", Jobs: &[]store.XArchiveJob{job}})
	requirements.NoError(err)
	input.Cost.JobID, input.Cost.JobFingerprint = job.ID, job.Fingerprint
	const perMonth = 130
	var first store.XAttemptKey
	for month := range 2 {
		for n := range perMonth {
			admission, err := st.AdmitXReplyContext(t.Context(), input)
			requirements.NoError(err)
			requirements.True(admission.Created)
			if month == 0 && n == 0 {
				first = admission.Attempt.XAttemptKey
			}
			requirements.NoError(st.RejectXReplyContext(t.Context(), admission.Attempt.XAttemptKey, "provider_rejected"))
		}
		input.Cost.At = input.Cost.At.Add(2 * time.Minute)
	}
	requirements.NoError(st.RejectXReplyContext(t.Context(), first, "provider_rejected"))
	unknown, err := st.AdmitXReplyContext(t.Context(), input)
	requirements.NoError(err)
	requirements.NoError(st.MarkXReplyUnknownContext(t.Context(), unknown.Attempt.XAttemptKey))
	replay, err := st.AdmitXReplyContext(t.Context(), input)
	requirements.NoError(err)
	assertions.False(replay.Created)
	assertions.Equal(unknown.Attempt.AttemptID, replay.Attempt.AttemptID)
	state, err = st.LoadXControlContext(t.Context(), d.SourceID)
	requirements.NoError(err)
	requirements.Len(state.SpendBuckets, 2)
	assertions.Equal(int64(perMonth*200_000), state.SpendBuckets[0].SpentMicros)
	assertions.Equal(int64(perMonth*200_000), state.SpendBuckets[1].SpentMicros)
	assertions.Equal(int64(200_000), state.SpendBuckets[1].ReservedMicros)
	requirements.Len(state.Jobs, 1)
	assertions.Equal(int64(2*perMonth*200_000), state.Jobs[0].SpentMicros)
	assertions.Equal(int64(200_000), state.Jobs[0].ReservedMicros)
	requirements.Len(state.Reservations, 1)
	assertions.Equal(unknown.Attempt.ReservationID, state.Reservations[0].ID)
}
