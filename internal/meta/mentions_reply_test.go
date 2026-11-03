package meta

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func mentionReplyFixture(t *testing.T, comment bool) (*store.Store, mentionScope, int64) {
	t.Helper()
	st, scope := mentionStore(t)
	root := mentionRoot()
	id, err := persistMentionMedia(t.Context(), st, scope, root, surfaceCaptionMention)
	require.NoError(t, err)
	if comment {
		id, err = persistMentionComment(t.Context(), st, scope, IGMentionComment{ID: mentionComment, Text: "Example mention", Media: &root})
		require.NoError(t, err)
	}
	return st, scope, id
}

func TestMentionReplyPostShape(t *testing.T) {
	for _, comment := range []bool{false, true} {
		t.Run(strconv.FormatBool(comment), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			target := MentionTarget{IG: mentionIG, MediaID: mentionMedia}
			if comment {
				target.CommentID = mentionComment
			}
			var calls atomic.Int32
			client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/v26.0/"+mentionIG+"/mentions", r.URL.Path)
				assert.Empty(t, r.URL.RawQuery)
				if !assert.NoError(t, r.ParseForm()) {
					return
				}
				assert.Equal(t, mentionMedia, r.PostForm.Get("media_id"))
				assert.Equal(t, "Example reply", r.PostForm.Get("message"))
				assert.Equal(t, target.CommentID, r.PostForm.Get("comment_id"))
				assert.Len(t, r.PostForm, 2+btoi(comment))
				_, _ = fmt.Fprint(w, `{"id":"500"}`)
			})
			got, err := client.ReplyToMention(t.Context(), target, "Example reply")
			requirements.NoError(err)
			assertions.Equal("500", got)
			target.MediaID = ""
			_, err = client.ReplyToMention(t.Context(), target, "Example reply")
			requirements.Error(err)
			assertions.Equal(int32(1), calls.Load(), "invalid media identity cannot dispatch")
		})
	}
}

func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestMentionReplyTargetRefusals(t *testing.T) {
	requirements := require.New(t)
	st, scope, id := mentionReplyFixture(t, false)
	for _, kind := range []ActionKind{ActionPrivateReply, ActionHide, ActionUnhide, ActionDelete} {
		_, err := MentionReplyTarget(t.Context(), st, id, kind)
		requirements.Error(err)
	}
	tag := mentionRoot()
	tag.ID = "600"
	tagID, err := persistMentionMedia(t.Context(), st, scope, tag, surfaceTagged)
	requirements.NoError(err)
	_, err = MentionReplyTarget(t.Context(), st, tagID, ActionPublicReply)
	requirements.ErrorContains(err, "Instagram does not support commenting on tagged photos")
	fb, fbSource, fbID, _, _ := actionFixture(t)
	_, err = MentionReplyTarget(t.Context(), fb, fbID, ActionPublicReply)
	requirements.Error(err)
	rows, err := fb.ListMetaCommentActionsContext(t.Context(), fbSource.ID, "100_200_300")
	requirements.NoError(err)
	assert.Empty(t, rows)
}

func TestMentionReplyArchivedOnce(t *testing.T) {
	for _, comment := range []bool{false, true} {
		t.Run(strconv.FormatBool(comment), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st, scope, id := mentionReplyFixture(t, comment)
			requirements.NoError(st.UpdateSourceDisplayNameContext(t.Context(), scope.SourceID, "example_account"))
			target, err := MentionReplyTarget(t.Context(), st, id, ActionPublicReply)
			requirements.NoError(err)
			sent := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
			first, err := persistMentionReply(t.Context(), st, scope, target, "500", "Example reply", sent)
			requirements.NoError(err)
			again, err := persistMentionReply(t.Context(), st, scope, target, "500", "Example reply", sent)
			requirements.NoError(err)
			assertions.Equal(first, again)
			row := message(t, st, scope.SourceID, "500")
			assertions.True(row.IsFromMe)
			assertions.Equal(id, row.ReplyToMessageID.Int64)
			assertions.Equal(message(t, st, scope.SourceID, mentionMedia).ConversationID, row.ConversationID)
			assertions.True(row.SenderID.Valid)
			var recipientName, participantName string
			requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT mr.display_name, p.display_name FROM message_recipients mr JOIN participants p ON p.id=mr.participant_id WHERE mr.message_id=? AND mr.recipient_type='from'`, row.ID).Scan(&recipientName, &participantName))
			assertions.Equal("example_account", recipientName)
			assertions.Equal("example_account", participantName)
			var self string
			requirements.NoError(st.DB().QueryRow(`SELECT identifier_value FROM participant_identifiers WHERE participant_id=?`, row.SenderID.Int64).Scan(&self))
			assertions.Equal(instagramActorIdentifier(scope.App, scope.Page, scope.IG, "ig_user", scope.IG), self)
			metadata, err := st.GetMessageMetadata(row.ID)
			requirements.NoError(err)
			assertions.Contains(metadata.String, `"reply":true`)
			body, err := st.GetMessageBodyText(first)
			requirements.NoError(err)
			assertions.Equal("Example reply", body)
			_, err = MentionReplyTarget(t.Context(), st, first, ActionPublicReply)
			assertions.Error(err, "an outgoing reply is not a received mention")
		})
	}
}

func TestMentionReplyRetryArchivesSuccessfulDispatchTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, scope, id := mentionReplyFixture(t, false)
	var calls atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"code":100}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"500"}`)
	})
	actions := NewActions(st, ActionsOptions{Allowed: func(int64, ActionKind) (bool, error) { return true, nil }, OpenAPI: func(context.Context, *store.Source) (ActionAPI, error) { return client, nil }})
	preview, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "Example reply", DryRun: true})
	require.NoError(err)
	req := ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "Example reply", Confirm: preview.Confirm}
	first, err := actions.Run(t.Context(), req)
	require.Error(err)
	require.NotNil(first.Receipt)
	assert.Equal("failed", first.Receipt.State)

	rows, err := st.ListMessageMetaCommentActionsContext(t.Context(), id)
	require.NoError(err)
	require.Len(rows, 1)
	oldCreatedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE meta_comment_actions SET created_at=? WHERE id=?`), oldCreatedAt, rows[0].ID)
	require.NoError(err)

	retried, err := actions.Run(t.Context(), req)
	require.NoError(err)
	require.NotNil(retried.Receipt)
	assert.Equal("succeeded", retried.Receipt.State)
	rows, err = st.ListMessageMetaCommentActionsContext(t.Context(), id)
	require.NoError(err)
	require.Len(rows, 1)
	require.NotNil(rows[0].DispatchedAt)
	archivedID := message(t, st, scope.SourceID, "500").ID
	archived, err := st.GetMessageContext(t.Context(), archivedID)
	require.NoError(err)
	require.False(archived.SentAt.IsZero())
	assert.Equal(rows[0].DispatchedAt.Unix(), archived.SentAt.Unix())
	assert.True(archived.SentAt.After(oldCreatedAt))
	assert.Equal(int32(2), calls.Load())
}

func TestMentionReplyCommentRequiresMediaID(t *testing.T) {
	requirements := require.New(t)
	st, _, id := mentionReplyFixture(t, true)
	saved, err := st.GetMessageMetadata(id)
	requirements.NoError(err)
	var metadata mentionMetadata
	requirements.NoError(json.Unmarshal([]byte(saved.String), &metadata))
	metadata.MediaID = ""
	data, err := json.Marshal(metadata)
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(id, sql.NullString{String: string(data), Valid: true}))
	_, err = MentionReplyTarget(t.Context(), st, id, ActionPublicReply)
	assert.Error(t, err)
}

func TestMentionReplyActionsArchiveAndReplay(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, scope, id := mentionReplyFixture(t, true)
	var calls atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "/v26.0/"+mentionIG+"/mentions", r.URL.Path)
		_, _ = fmt.Fprint(w, `{"id":"500"}`)
	})
	actions := NewActions(st, ActionsOptions{Allowed: func(int64, ActionKind) (bool, error) { return true, nil }, OpenAPI: func(context.Context, *store.Source) (ActionAPI, error) { return client, nil }})
	preview, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "Example reply", DryRun: true})
	requirements.NoError(err)
	assertions.Equal("instagram", preview.Disclosure.Provider)
	assertions.Equal(int32(0), calls.Load())
	req := ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "Example reply", Confirm: preview.Confirm}
	got, err := actions.Run(t.Context(), req)
	requirements.NoError(err)
	assertions.Equal("succeeded", got.Receipt.State)
	assertions.True(got.Receipt.ArchiveComplete)
	assertions.Equal(message(t, st, scope.SourceID, "500").ID, got.Receipt.ReplyMessageID)
	again, err := actions.Run(t.Context(), req)
	requirements.NoError(err)
	assertions.Equal(got.Receipt.ID, again.Receipt.ID)
	assertions.True(again.Receipt.ArchiveComplete)
	assertions.Equal(int32(1), calls.Load())
	_, err = actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPrivateReply, Body: "private", DryRun: true})
	requirements.ErrorContains(err, "private replies are not supported for Instagram mentions")
}

func TestMentionReplyUnknownOutcomeNotRetried(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, _, id := mentionReplyFixture(t, false)
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-release
	})
	t.Cleanup(func() { close(release) })
	actions := NewActions(st, ActionsOptions{Allowed: func(int64, ActionKind) (bool, error) { return true, nil }, OpenAPI: func(context.Context, *store.Source) (ActionAPI, error) { return client, nil }})
	preview, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "reply", DryRun: true})
	requirements.NoError(err)
	req := ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "reply", Confirm: preview.Confirm}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { <-started; cancel(); close(done) }()
	got, err := actions.Run(ctx, req)
	<-done
	requirements.Error(err)
	requirements.NotNil(got.Receipt)
	assertions.Equal("unknown", got.Receipt.State)
	again, err := actions.Run(t.Context(), req)
	requirements.Error(err)
	assertions.Equal(got.Receipt.ID, again.Receipt.ID)
	assertions.Equal(int32(1), calls.Load())
}

func TestMentionReplyRepairsArchiveWithoutResend(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, scope, id := mentionReplyFixture(t, false)
	saved, err := st.GetMessageMetadata(id)
	requirements.NoError(err)
	var calls atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// The remote write succeeds while local provenance becomes unavailable.
		if !assert.NoError(t, st.SetMessageMetadata(id, sql.NullString{String: `{"version":2}`, Valid: true})) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"500"}`)
	})
	actions := NewActions(st, ActionsOptions{Allowed: func(int64, ActionKind) (bool, error) { return true, nil }, OpenAPI: func(context.Context, *store.Source) (ActionAPI, error) { return client, nil }})
	preview, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "Example reply", DryRun: true})
	requirements.NoError(err)
	req := ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "Example reply", Confirm: preview.Confirm}
	changed := req
	changed.Body = "Different reply"
	_, err = actions.Run(t.Context(), changed)
	requirements.ErrorContains(err, "confirmation_mismatch")
	assertions.Zero(calls.Load())
	got, err := actions.Run(t.Context(), req)
	requirements.ErrorContains(err, "remote_accepted_local_failed")
	requirements.NotNil(got.Receipt)
	assertions.Equal("succeeded", got.Receipt.State)
	assertions.False(got.Receipt.ArchiveComplete)
	rows, err := st.ListMessageMetaCommentActionsContext(t.Context(), id)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	requirements.NotNil(rows[0].DispatchedAt)
	oldCreatedAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE meta_comment_actions SET created_at=? WHERE id=?`), oldCreatedAt, rows[0].ID)
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(id, saved))
	again, err := actions.Run(t.Context(), req)
	requirements.NoError(err)
	assertions.Equal(got.Receipt.ID, again.Receipt.ID)
	assertions.True(again.Receipt.ArchiveComplete)
	assertions.Equal(message(t, st, scope.SourceID, "500").ID, again.Receipt.ReplyMessageID)
	archived, err := st.GetMessageContext(t.Context(), again.Receipt.ReplyMessageID)
	requirements.NoError(err)
	requirements.False(archived.SentAt.IsZero())
	assertions.Equal(rows[0].DispatchedAt.Unix(), archived.SentAt.Unix())
	assertions.True(archived.SentAt.After(oldCreatedAt))
	assertions.Equal(int32(1), calls.Load())
}

func TestMentionReplyHistoryOnlyRefused(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, scope, id := mentionReplyFixture(t, false)
	target, err := MentionReplyTarget(t.Context(), st, id, ActionPublicReply)
	requirements.NoError(err)
	var calls atomic.Int32
	client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"id":"500"}`)
	})
	actions := NewActions(st, ActionsOptions{Allowed: func(int64, ActionKind) (bool, error) { return true, nil }, OpenAPI: func(context.Context, *store.Source) (ActionAPI, error) { return client, nil }})
	preview, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "Example reply", DryRun: true})
	requirements.NoError(err)
	// A restored archive may retain historical source settings from an older
	// importer, even when its configured profile still permits public replies.
	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO source_settings(source_id, history_only) VALUES (?, TRUE)`, scope.SourceID)
	requirements.NoError(err)
	_, err = actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "Example reply", Confirm: preview.Confirm})
	requirements.ErrorIs(err, store.ErrSourceRetired)
	assertions.Zero(calls.Load())
	_, err = persistMentionReply(t.Context(), st, scope, target, "500", "Example reply", time.Now())
	requirements.ErrorIs(err, store.ErrSourceRetired)
	rows, err := st.ListMetaCommentActionsContext(t.Context(), scope.SourceID, mentionMedia)
	requirements.NoError(err)
	assertions.Empty(rows)
	var replies int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id=?`, scope.SourceID, "500").Scan(&replies))
	assertions.Zero(replies)
}

func TestMentionReplyInvalidAcceptanceUnknown(t *testing.T) {
	for _, remoteID := range []string{"", "not-an-id", mentionMedia, mentionComment} {
		t.Run(remoteID, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st, _, id := mentionReplyFixture(t, true)
			var calls atomic.Int32
			client := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = fmt.Fprintf(w, `{"id":%q}`, remoteID)
			})
			actions := NewActions(st, ActionsOptions{Allowed: func(int64, ActionKind) (bool, error) { return true, nil }, OpenAPI: func(context.Context, *store.Source) (ActionAPI, error) { return client, nil }})
			preview, err := actions.Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "Example reply", DryRun: true})
			requirements.NoError(err)
			request := ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "Example reply", Confirm: preview.Confirm}
			got, err := actions.Run(t.Context(), request)
			requirements.Error(err)
			requirements.NotNil(got.Receipt)
			assertions.Equal("unknown", got.Receipt.State)
			assertions.False(got.Receipt.ArchiveComplete)
			_, err = actions.Run(t.Context(), request)
			requirements.Error(err)
			assertions.Equal(int32(1), calls.Load())
		})
	}
}

func TestMentionReplyPreviewSurfaceWarnings(t *testing.T) {
	for _, comment := range []bool{false, true} {
		t.Run(strconv.FormatBool(comment), func(t *testing.T) {
			st, _, id := mentionReplyFixture(t, comment)
			result, err := NewActions(st, ActionsOptions{}).Run(t.Context(), ActionRequest{MessageID: id, Action: ActionPublicReply, Body: "Example reply", DryRun: true})
			require.NoError(t, err)
			marker := "instagram_caption_mention"
			if comment {
				marker = "instagram_comment_mention"
			}
			assert.Contains(t, result.Warnings, marker)
		})
	}
}
