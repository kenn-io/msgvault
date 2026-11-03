package meta

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const replyResponse = `{"id":"100_200_301","message":"Example reply","created_time":"2026-11-01T12:00:00+0000","from":{"id":"100","name":"Example Page"},"parent":{"id":"100_200_300"}}`

func replyRun(t *testing.T, a *Actions, id int64, kind ActionKind, body string) (*ActionResult, error) {
	t.Helper()
	preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, Body: body, DryRun: true})
	require.NoError(t, err)
	return a.Run(t.Context(), ActionRequest{MessageID: id, Action: kind, Body: body, Confirm: preview.Confirm})
}
func privateClock(t *testing.T, a *Actions, id int64) Target {
	t.Helper()
	target, err := ResolveTarget(t.Context(), a.st, id)
	require.NoError(t, err)
	a.opts.Now = func() time.Time { return target.SentAt.Add(time.Hour) }
	return target
}
func TestPublicReplyFacebookArchivesOnce(t *testing.T) {
	assert := assert.New(t)
	st, src, id, a, g := actionFixture(t)
	g.response = replyResponse
	got, err := replyRun(t, a, id, ActionPublicReply, "Example reply")
	require.NoError(t, err)
	assert.Equal("succeeded", got.Receipt.State)
	assert.True(got.Disclosure.Public)
	assert.Equal("Example reply", got.Disclosure.Body)
	assert.Equal("public", got.Visibility)
	assert.True(got.Receipt.ArchiveComplete)
	assert.Equal(message(t, st, src.ID, "100_200_301").ID, got.Receipt.ReplyMessageID)
	assert.Equal("/v26.0/100_200_300/comments", g.path)
}
func TestPublicReplyIdempotentSameBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, _, id, a, g := actionFixture(t)
	g.response = replyResponse
	first, err := replyRun(t, a, id, ActionPublicReply, "Example reply")
	require.NoError(err)
	again, err := replyRun(t, a, id, ActionPublicReply, "Example reply")
	require.NoError(err)
	assert.Equal(first.Receipt.ID, again.Receipt.ID)
	assert.Equal(1, g.count())
}
func TestPublicReplyDifferentBodyAllowed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, _, id, a, g := actionFixture(t)
	g.response = replyResponse
	_, err := replyRun(t, a, id, ActionPublicReply, "First reply")
	require.NoError(err)
	g.response = `{"id":"100_200_302"}`
	got, err := replyRun(t, a, id, ActionPublicReply, "Second reply")
	require.NoError(err)
	assert.Equal("100_200_302", got.Receipt.RemoteID)
	assert.False(got.Receipt.ArchiveComplete)
	assert.Equal(2, g.count())
}
func TestActionsConcurrentReplySingleDispatch(t *testing.T) {
	requirements := require.New(t)
	_, _, id, a, g := actionFixture(t)
	g.response = replyResponse
	preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "reply", DryRun: true})
	requirements.NoError(err)
	req := ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "reply", Confirm: preview.Confirm}
	g.barrier, g.release = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { _, e := a.Run(t.Context(), req); done <- e }()
	<-g.barrier
	_, err = a.Run(t.Context(), req)
	close(g.release)
	_ = actionCode(t, err, "sync_active")
	requirements.NoError(<-done)
	g.barrier = nil
	_, err = a.Run(t.Context(), req)
	requirements.NoError(err)
	assert.Equal(t, 1, g.count())
}
func TestPrivateReplyWindowBoundary(t *testing.T) {
	for _, d := range []time.Duration{7*24*time.Hour - time.Second, 7 * 24 * time.Hour} {
		t.Run(d.String(), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			_, _, id, a, g := actionFixture(t)
			target := privateClock(t, a, id)
			a.opts.Now = func() time.Time { return target.SentAt.Add(d) }
			g.response = `{"recipient_id":"opaque-300","message_id":"private-301"}`
			got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", DryRun: true})
			if d == 7*24*time.Hour {
				_ = actionCode(t, err, "private_reply_window_closed")
				assertions.Zero(g.count())
				return
			}
			requirements.NoError(err)
			got, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", Confirm: got.Confirm})
			requirements.NoError(err)
			assertions.Equal("opaque-300", got.Receipt.RecipientID)
			assertions.False(got.Disclosure.Public)
			assertions.Equal("private", got.Visibility)
			assertions.Equal("/v26.0/100/messages", g.path)
		})
	}
}
func TestPrivateReplySelfRefused(t *testing.T) {
	st, _, id, a, g := actionFixture(t)
	privateClock(t, a, id)
	_, err := st.DB().Exec("UPDATE messages SET is_from_me=1 WHERE id=?", id)
	require.NoError(t, err)
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "reply", DryRun: true})
	_ = actionCode(t, err, "private_reply_not_allowed")
	assert.Zero(t, g.count())
}
func TestPrivateReplyReceiptRecoveryAfterAuthorMarkedFromMe(t *testing.T) {
	for _, resolution := range []string{"sent", "not-sent"} {
		t.Run(resolution, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, _, id, a, g := actionFixture(t)
			privateClock(t, a, id)
			g.status = 500
			preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private reply", DryRun: true})
			require.NoError(err)
			_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private reply", Confirm: preview.Confirm})
			_ = actionCode(t, err, "outcome_unknown")
			require.Equal(1, g.count())

			_, err = st.DB().Exec("UPDATE messages SET is_from_me=1 WHERE id=?", id)
			require.NoError(err)
			preview, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private reply", DryRun: true})
			require.NoError(err)
			require.NotNil(preview.Receipt)
			assert.Equal("unknown", preview.Receipt.State)
			assert.Contains(preview.Warnings, "prior_private_reply_outcome_unknown")

			got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private reply", Confirm: preview.Confirm, ResolveUnknown: resolution})
			require.NoError(err)
			want := "succeeded"
			if resolution == "not-sent" {
				want = "failed"
			}
			assert.Equal(want, got.Receipt.State)
			assert.Equal(1, g.count(), "receipt recovery must not dispatch another provider request")
		})
	}
}
func TestPrivateReplyOncePerComment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, _, id, a, g := actionFixture(t)
	privateClock(t, a, id)
	g.response = `{"recipient_id":"opaque-300","message_id":"private-301"}`
	first, err := replyRun(t, a, id, ActionPrivateReply, "First reply")
	require.NoError(err)
	preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "Another reply", DryRun: true})
	if err == nil {
		_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "Another reply", Confirm: preview.Confirm})
	}
	failure := actionCode(t, err, "already_sent")
	require.NotNil(failure.Result.Receipt)
	assert.Equal(first.Receipt.ID, failure.Result.Receipt.ID)
	assert.Equal(1, g.count())
}
func TestPrivateReplyAuthorWarning(t *testing.T) {
	for _, scenario := range []string{"archived", "garbage-collected", "delayed-retry", "omitted-author"} {
		t.Run(scenario, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, src, id, a, g := actionFixture(t)
			target := privateClock(t, a, id)
			if scenario == "omitted-author" {
				fixture, client := newGraphFixture(t)
				obj := comment("100_200_300", "100_200", "author omitted on replay")
				obj.From = nil
				fixture.comments["100_200"] = []Object{*obj}
				_, err := NewImporter(st, client).Import(t.Context(), "9", "100", ImportOptions{})
				require.NoError(err)
				archived, readErr := st.MetaCommentTargetContext(t.Context(), id)
				require.NoError(readErr)
				require.NoError(st.SetParticipantIdentifierContext(t.Context(), archived.SenderID, SourceType, ActorIdentifier("9", "101", "999")))
				restored, resolveErr := ResolveTarget(t.Context(), st, id)
				require.NoError(resolveErr)
				assert.Equal(target.AuthorID, restored.AuthorID)
				assert.Equal(target.AuthorName, restored.AuthorName)
			}
			if scenario == "delayed-retry" {
				g.status, g.response = 400, `{"error":{"code":100}}`
				_, err := replyRun(t, a, id, ActionPrivateReply, "First reply")
				_ = actionCode(t, err, "provider_rejected")
				_, err = st.DB().Exec(st.Rebind(`UPDATE meta_comment_actions SET created_at=?,target_author_id=NULL WHERE message_id=?`), a.opts.Now().Add(-8*24*time.Hour), id)
				require.NoError(err)
			}
			g.status, g.response = 200, `{"recipient_id":"opaque-300","message_id":"private-301"}`
			sent, err := replyRun(t, a, id, ActionPrivateReply, "First reply")
			require.NoError(err)
			assert.Equal(target.AuthorName, sent.Disclosure.AuthorName)
			original := message(t, st, src.ID, "100_200_300")
			original.ID = 0
			original.SentAt.Time, original.SentAt.Valid = target.SentAt, true
			original.SourceMessageID = "100_200_302"
			other, err := st.UpsertMessage(original)
			require.NoError(err)
			metadata, err := st.GetMessageMetadataContext(t.Context(), id)
			require.NoError(err)
			require.NoError(st.SetMessageMetadataContext(t.Context(), other, metadata))
			require.NoError(st.UpsertMessageRawWithFormat(other, []byte(`{"id":"100_200_302","from":{"id":"300","name":"Example Author"}}`), rawFormat))
			if scenario == "garbage-collected" || scenario == "omitted-author" {
				require.NoError(st.MarkMessageDeleted(src.ID, "100_200_300"))
				plan, planErr := st.PlanGCContext(t.Context())
				require.NoError(planErr)
				deleted, deleteErr := st.ExecuteGCContext(t.Context(), plan)
				require.NoError(deleteErr)
				assert.Equal(int64(1), deleted)
			}
			got, err := a.Run(t.Context(), ActionRequest{MessageID: other, Action: ActionPrivateReply, Body: "Another comment", DryRun: true})
			require.NoError(err)
			assert.Contains(got.Warnings, "prior_private_reply_to_author")
			rows, err := st.ListMetaCommentActionsContext(t.Context(), src.ID, "100_200_300")
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal(sent.Receipt.ID, rows[0].ID)
			assert.Equal(target.AuthorID, rows[0].TargetAuthorID)
			if scenario == "garbage-collected" || scenario == "omitted-author" {
				assert.False(rows[0].MessageID.Valid)
			}
			require.NoError(st.UpsertMessageRawWithFormat(other, []byte(`{"id":"100_200_302","from":{"id":"301","name":"Another Author"}}`), rawFormat))
			got, err = a.Run(t.Context(), ActionRequest{MessageID: other, Action: ActionPrivateReply, Body: "Another comment", DryRun: true})
			require.NoError(err)
			assert.NotContains(got.Warnings, "prior_private_reply_to_author", "a different source-scoped author must not match")
		})
	}
}
func TestPrivateReplyThreadOwnedByOtherApp(t *testing.T) {
	testReplyProviderCode(t, 2018300, "thread_owned_by_other_app")
}
func TestPrivateReplyWindowClosedProvider(t *testing.T) {
	testReplyProviderCode(t, 2534022, "messaging_window_closed")
}
func testReplyProviderCode(t *testing.T, subcode int, want string) {
	t.Helper()
	_, _, id, a, g := actionFixture(t)
	privateClock(t, a, id)
	g.status, g.response = 400, fmt.Sprintf(`{"error":{"code":10,"error_subcode":%d}}`, subcode)
	got, err := replyRun(t, a, id, ActionPrivateReply, "private")
	_ = actionCode(t, err, want)
	assert.Equal(t, "failed", got.Receipt.State)
	assert.Equal(t, 1, g.count())
}
func TestReplyUnknownBlocksThenResolve(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, _, id, a, g := actionFixture(t)
	g.status = 500
	_, err := replyRun(t, a, id, ActionPublicReply, "reply")
	_ = actionCode(t, err, "outcome_unknown")
	_, err = replyRun(t, a, id, ActionPublicReply, "reply")
	_ = actionCode(t, err, "outcome_unknown")
	preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "reply", DryRun: true})
	require.NoError(err)
	got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "reply", Confirm: preview.Confirm, ResolveUnknown: "sent"})
	require.NoError(err)
	assert.Equal("succeeded", got.Receipt.State)
	assert.Equal(1, g.count())
}
func TestReplyNoFallbackBetweenVisibilities(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.status, g.response = 403, `{"error":{"code":200}}`
	_, err := replyRun(t, a, id, ActionPublicReply, "reply")
	_ = actionCode(t, err, "permission_missing")
	assert.Equal(t, 1, g.count())
	assert.Equal(t, "/v26.0/100_200_300/comments", g.path)
}
func TestReplyArchiveFailureReportsRemoteAccepted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, id, a, g := actionFixture(t)
	g.response = replyResponse
	_, err := st.DB().Exec(`CREATE TRIGGER fail_reply BEFORE INSERT ON messages WHEN NEW.source_message_id='100_200_301' BEGIN SELECT RAISE(FAIL,'synthetic archive failure'); END`)
	require.NoError(err)
	got, err := replyRun(t, a, id, ActionPublicReply, "reply")
	_ = actionCode(t, err, "remote_accepted_local_failed")
	assert.Equal("succeeded", got.Receipt.State)
	assert.Equal("100_200_301", got.Receipt.RemoteID)
	assert.False(got.Receipt.ArchiveComplete)
	_, err = st.DB().Exec("DROP TRIGGER fail_reply")
	require.NoError(err)
	_, err = replyRun(t, a, id, ActionPublicReply, "reply")
	require.NoError(err)
	assert.Equal(1, g.count())
}
func TestReplyBodyAndConfirmation(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	for _, body := range []string{"", "bad\x00body", string([]byte{0xff})} {
		_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: body, DryRun: true})
		_ = actionCode(t, err, "invalid_body")
	}
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "reply"})
	_ = actionCode(t, err, "confirmation_mismatch")
	assert.Zero(t, g.count())
}

func TestPrivateReplyUnknownResolvedAfterWindow(t *testing.T) {
	for _, resolution := range []string{"sent", "not-sent"} {
		t.Run(resolution, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			_, _, id, a, g := actionFixture(t)
			target := privateClock(t, a, id)
			a.opts.Now = func() time.Time { return target.SentAt.Add(7*24*time.Hour - time.Minute) }
			preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", DryRun: true})
			requirements.NoError(err)
			g.status = 500
			_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", Confirm: preview.Confirm})
			_ = actionCode(t, err, "outcome_unknown")
			a.opts.Now = func() time.Time { return target.SentAt.Add(7*24*time.Hour + time.Hour) }
			preview, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", DryRun: true})
			requirements.NoError(err)
			got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", Confirm: preview.Confirm, ResolveUnknown: resolution})
			requirements.NoError(err)
			want := "succeeded"
			if resolution == "not-sent" {
				want = "failed"
			}
			assertions.Equal(want, got.Receipt.State)
			assertions.Equal(1, g.count())
			if resolution == "not-sent" {
				_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", Confirm: preview.Confirm})
				_ = actionCode(t, err, "private_reply_window_closed")
				assertions.Equal(1, g.count())
			}
		})
	}
}

func TestPrivateReplyResolveSentRetainsPartialEvidence(t *testing.T) {
	for _, tc := range []struct{ response, remoteID, recipientID string }{
		{`{"recipient_id":"opaque-300"}`, "", "opaque-300"},
		{`{"message_id":"private-301"}`, "private-301", ""},
	} {
		t.Run(tc.response, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, _, id, a, g := actionFixture(t)
			privateClock(t, a, id)
			g.response = tc.response
			got, err := replyRun(t, a, id, ActionPrivateReply, "private reply")
			_ = actionCode(t, err, "outcome_unknown")
			require.NotNil(got.Receipt)
			assert.Equal(tc.remoteID, got.Receipt.RemoteID)
			assert.Equal(tc.recipientID, got.Receipt.RecipientID)
			preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private reply", DryRun: true})
			require.NoError(err)
			resolved, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private reply", Confirm: preview.Confirm, ResolveUnknown: "sent"})
			require.NoError(err)
			assert.Equal("succeeded", resolved.Receipt.State)
			assert.Equal(tc.remoteID, resolved.Receipt.RemoteID)
			assert.Equal(tc.recipientID, resolved.Receipt.RecipientID)
			rows, err := st.ListMessageMetaCommentActionsContext(t.Context(), id)
			require.NoError(err)
			require.Len(rows, 1)
			assert.Equal(tc.remoteID, rows[0].RemoteID)
			assert.Equal(tc.recipientID, rows[0].RemoteRecipientID)
			assert.Equal(1, g.count())
		})
	}
}
