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
	st, src, id, a, g := actionFixture(t)
	g.response = replyResponse
	got, err := replyRun(t, a, id, ActionPublicReply, "Example reply")
	require.NoError(t, err)
	assert.Equal(t, "succeeded", got.Receipt.State)
	assert.True(t, got.Disclosure.Public)
	assert.Equal(t, "Example reply", got.Disclosure.Body)
	assert.Equal(t, "public", got.Visibility)
	assert.True(t, got.Receipt.ArchiveComplete)
	assert.Equal(t, message(t, st, src.ID, "100_200_301").ID, got.Receipt.ReplyMessageID)
	assert.Equal(t, "/v26.0/100_200_300/comments", g.path)
}
func TestPublicReplyIdempotentSameBody(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.response = replyResponse
	first, err := replyRun(t, a, id, ActionPublicReply, "Example reply")
	require.NoError(t, err)
	again, err := replyRun(t, a, id, ActionPublicReply, "Example reply")
	require.NoError(t, err)
	assert.Equal(t, first.Receipt.ID, again.Receipt.ID)
	assert.Equal(t, 1, g.count())
}
func TestPublicReplyDifferentBodyAllowed(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.response = replyResponse
	_, err := replyRun(t, a, id, ActionPublicReply, "First reply")
	require.NoError(t, err)
	g.response = `{"id":"100_200_302"}`
	got, err := replyRun(t, a, id, ActionPublicReply, "Second reply")
	require.NoError(t, err)
	assert.Equal(t, "100_200_302", got.Receipt.RemoteID)
	assert.False(t, got.Receipt.ArchiveComplete)
	assert.Equal(t, 2, g.count())
}
func TestActionsConcurrentReplySingleDispatch(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.response = replyResponse
	preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "reply", DryRun: true})
	require.NoError(t, err)
	req := ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "reply", Confirm: preview.Confirm}
	g.barrier, g.release = make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { _, e := a.Run(t.Context(), req); done <- e }()
	<-g.barrier
	_, err = a.Run(t.Context(), req)
	close(g.release)
	actionCode(t, err, "sync_active")
	require.NoError(t, <-done)
	g.barrier = nil
	_, err = a.Run(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, 1, g.count())
}
func TestPrivateReplyWindowBoundary(t *testing.T) {
	for _, d := range []time.Duration{7*24*time.Hour - time.Second, 7 * 24 * time.Hour} {
		t.Run(d.String(), func(t *testing.T) {
			_, _, id, a, g := actionFixture(t)
			target := privateClock(t, a, id)
			a.opts.Now = func() time.Time { return target.SentAt.Add(d) }
			g.response = `{"recipient_id":"opaque-300","message_id":"private-301"}`
			got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", DryRun: true})
			if d == 7*24*time.Hour {
				actionCode(t, err, "private_reply_window_closed")
				assert.Zero(t, g.count())
				return
			}
			require.NoError(t, err)
			got, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", Confirm: got.Confirm})
			require.NoError(t, err)
			assert.Equal(t, "opaque-300", got.Receipt.RecipientID)
			assert.False(t, got.Disclosure.Public)
			assert.Equal(t, "private", got.Visibility)
			assert.Equal(t, "/v26.0/100/messages", g.path)
		})
	}
}
func TestPrivateReplySelfRefused(t *testing.T) {
	st, _, id, a, g := actionFixture(t)
	privateClock(t, a, id)
	_, err := st.DB().Exec("UPDATE messages SET is_from_me=1 WHERE id=?", id)
	require.NoError(t, err)
	_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "reply", DryRun: true})
	actionCode(t, err, "private_reply_not_allowed")
	assert.Zero(t, g.count())
}
func TestPrivateReplyOncePerComment(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	privateClock(t, a, id)
	g.response = `{"recipient_id":"opaque-300","message_id":"private-301"}`
	first, err := replyRun(t, a, id, ActionPrivateReply, "First reply")
	require.NoError(t, err)
	preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "Another reply", DryRun: true})
	if err == nil {
		_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "Another reply", Confirm: preview.Confirm})
	}
	failure := actionCode(t, err, "already_sent")
	require.NotNil(t, failure.Result.Receipt)
	assert.Equal(t, first.Receipt.ID, failure.Result.Receipt.ID)
	assert.Equal(t, 1, g.count())
}
func TestPrivateReplyAuthorWarning(t *testing.T) {
	st, src, id, a, g := actionFixture(t)
	target := privateClock(t, a, id)
	g.response = `{"recipient_id":"opaque-300","message_id":"private-301"}`
	_, err := replyRun(t, a, id, ActionPrivateReply, "First reply")
	require.NoError(t, err)
	original := message(t, st, src.ID, "100_200_300")
	original.ID = 0
	original.SentAt.Time, original.SentAt.Valid = target.SentAt, true
	original.SourceMessageID = "100_200_302"
	other, err := st.UpsertMessage(original)
	require.NoError(t, err)
	metadata, err := st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(t, err)
	require.NoError(t, st.SetMessageMetadataContext(t.Context(), other, metadata))
	require.NoError(t, st.UpsertMessageRawWithFormat(other, []byte(`{"id":"100_200_302","from":{"id":"300","name":"Example Author"}}`), rawFormat))
	got, err := a.Run(t.Context(), ActionRequest{MessageID: other, Action: ActionPrivateReply, Body: "Another comment", DryRun: true})
	require.NoError(t, err)
	assert.Contains(t, got.Warnings, "prior_private_reply_to_author")
	assert.Equal(t, 1, g.count())
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
	actionCode(t, err, want)
	assert.Equal(t, "failed", got.Receipt.State)
	assert.Equal(t, 1, g.count())
}
func TestReplyUnknownBlocksThenResolve(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.status = 500
	_, err := replyRun(t, a, id, ActionPublicReply, "reply")
	actionCode(t, err, "outcome_unknown")
	_, err = replyRun(t, a, id, ActionPublicReply, "reply")
	actionCode(t, err, "outcome_unknown")
	preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "reply", DryRun: true})
	require.NoError(t, err)
	got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "reply", Confirm: preview.Confirm, ResolveUnknown: "sent"})
	require.NoError(t, err)
	assert.Equal(t, "succeeded", got.Receipt.State)
	assert.Equal(t, 1, g.count())
}
func TestReplyNoFallbackBetweenVisibilities(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	g.status, g.response = 403, `{"error":{"code":200}}`
	_, err := replyRun(t, a, id, ActionPublicReply, "reply")
	actionCode(t, err, "permission_missing")
	assert.Equal(t, 1, g.count())
	assert.Equal(t, "/v26.0/100_200_300/comments", g.path)
}
func TestReplyArchiveFailureReportsRemoteAccepted(t *testing.T) {
	st, _, id, a, g := actionFixture(t)
	g.response = replyResponse
	_, err := st.DB().Exec(`CREATE TRIGGER fail_reply BEFORE INSERT ON messages WHEN NEW.source_message_id='100_200_301' BEGIN SELECT RAISE(FAIL,'synthetic archive failure'); END`)
	require.NoError(t, err)
	got, err := replyRun(t, a, id, ActionPublicReply, "reply")
	actionCode(t, err, "remote_accepted_local_failed")
	assert.Equal(t, "succeeded", got.Receipt.State)
	assert.Equal(t, "100_200_301", got.Receipt.RemoteID)
	assert.False(t, got.Receipt.ArchiveComplete)
	_, err = st.DB().Exec("DROP TRIGGER fail_reply")
	require.NoError(t, err)
	_, err = replyRun(t, a, id, ActionPublicReply, "reply")
	require.NoError(t, err)
	assert.Equal(t, 1, g.count())
}
func TestReplyBodyAndConfirmation(t *testing.T) {
	_, _, id, a, g := actionFixture(t)
	for _, body := range []string{"", "bad\x00body", string([]byte{0xff})} {
		_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: body, DryRun: true})
		actionCode(t, err, "invalid_body")
	}
	_, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "reply"})
	actionCode(t, err, "confirmation_mismatch")
	assert.Zero(t, g.count())
}

func TestPrivateReplyUnknownResolvedAfterWindow(t *testing.T) {
	for _, resolution := range []string{"sent", "not-sent"} {
		t.Run(resolution, func(t *testing.T) {
			_, _, id, a, g := actionFixture(t)
			target := privateClock(t, a, id)
			a.opts.Now = func() time.Time { return target.SentAt.Add(7*24*time.Hour - time.Minute) }
			preview, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", DryRun: true})
			require.NoError(t, err)
			g.status = 500
			_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", Confirm: preview.Confirm})
			actionCode(t, err, "outcome_unknown")
			a.opts.Now = func() time.Time { return target.SentAt.Add(7*24*time.Hour + time.Hour) }
			preview, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", DryRun: true})
			require.NoError(t, err)
			got, err := a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", Confirm: preview.Confirm, ResolveUnknown: resolution})
			require.NoError(t, err)
			want := "succeeded"
			if resolution == "not-sent" {
				want = "failed"
			}
			assert.Equal(t, want, got.Receipt.State)
			assert.Equal(t, 1, g.count())
			if resolution == "not-sent" {
				_, err = a.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", Confirm: preview.Confirm})
				actionCode(t, err, "private_reply_window_closed")
				assert.Equal(t, 1, g.count())
			}
		})
	}
}
