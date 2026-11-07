package youtube

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type replyFixtureProvider struct {
	checks   atomic.Int32
	sends    atomic.Int32
	checkErr error
	sendErr  error
	comment  Comment
}

func (p *replyFixtureProvider) CheckReply(context.Context, ReplyTarget) error {
	p.checks.Add(1)
	return p.checkErr
}
func (p *replyFixtureProvider) InsertReply(context.Context, string, string) (Comment, error) {
	p.sends.Add(1)
	return p.comment, p.sendErr
}
func (p *replyFixtureProvider) GetReply(context.Context, string) (Comment, error) {
	return p.comment, nil
}
func replyFixture(t *testing.T) (*ReplyService, *replyFixtureProvider, ReplyTarget, *atomic.Int32) {
	t.Helper()
	var comment Comment
	require.NoError(t, json.Unmarshal([]byte(`{"id":"sent-1","snippet":{"parentId":"parent-1","videoId":"video-1","textOriginal":"Example reply","authorChannelId":{"value":"owner-1"},"publishedAt":"2026-11-06T12:00:00Z"}}`), &comment))
	p := &replyFixtureProvider{comment: comment}
	archived := &atomic.Int32{}
	s := &ReplyService{Store: testutil.NewTestStore(t), Provider: p, Persist: func(_ context.Context, c Comment) (int64, error) {
		archived.Add(1)
		assert.Equal(t, "sent-1", c.ID)
		return 42, nil
	}}
	source, err := s.Store.GetOrCreateSource(SourceType, SourceIdentifier("owner-1"))
	require.NoError(t, err)
	target := ReplyTarget{MessageID: 12, SourceID: source.ID, ChannelID: "owner-1", VideoID: "video-1", CommentID: "nested-1", ParentID: "parent-1", ThreadID: "thread-1", CanReply: true, ParentStatus: "published"}
	return s, p, target, archived
}

// Removing the durable claim lets simultaneous confirmations each dispatch a POST.
func TestReplyPreviewAndConcurrentConfirm(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, p, target, archived := replyFixture(t)
	preview, err := s.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	require.NotEmpty(preview.Token)
	assert.Zero(p.checks.Load())
	assert.Zero(p.sends.Load())
	assert.Equal("parent-1", preview.Target.ParentID)
	var wg sync.WaitGroup
	var successes atomic.Int32
	for range 8 {
		wg.Go(func() {
			_, sendErr := s.Confirm(t.Context(), target, "Example reply", preview.Token)
			if sendErr == nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(int32(1), p.sends.Load())
	assert.GreaterOrEqual(successes.Load(), int32(1))
	assert.GreaterOrEqual(archived.Load(), int32(1))
	again, err := s.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.NoError(err)
	assert.Equal(int64(42), again.MessageID)
	assert.Equal(int32(1), p.sends.Load())
}

// A fresh preview or alias target must not bypass an unknown send after restart.
func TestReplyUncertainOutcomeSurvivesNewServiceAndAlias(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, p, target, _ := replyFixture(t)
	preview, err := s.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	p.sendErr = errors.New("transport lost")
	_, err = s.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.ErrorIs(err, ErrReplyUncertain)
	restarted := *s
	_, err = restarted.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.ErrorIs(err, ErrReplyUncertain)
	_, err = restarted.Preview(t.Context(), target, "Example reply")
	require.ErrorIs(err, ErrReplyUncertain)
	alias := target
	alias.MessageID = 99
	alias.CommentID = target.ParentID
	_, err = restarted.Preview(t.Context(), alias, "Example reply")
	require.ErrorIs(err, ErrReplyUncertain)
	assert.Equal(int32(1), p.sends.Load())
}

func TestReplyRejectsChangedBodyTokenAndIneligibleParent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, p, target, _ := replyFixture(t)
	preview, err := s.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	_, err = s.Confirm(t.Context(), target, "Changed", preview.Token)
	require.Error(err)
	_, err = s.Confirm(t.Context(), target, "Example reply", "wrong")
	require.Error(err)
	for _, status := range []string{"", "heldForReview", "likelySpam"} {
		bad := target
		bad.ParentStatus = status
		_, err = s.Preview(t.Context(), bad, "Different reply")
		require.Error(err)
	}
	bad := target
	bad.CanReply = false
	_, err = s.Preview(t.Context(), bad, "Different reply")
	require.Error(err)
	bad = target
	bad.CommentID = "video:video-1"
	_, err = s.Preview(t.Context(), bad, "Different reply")
	require.Error(err)
	p.checkErr = ErrReplyRejected
	_, err = s.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.ErrorIs(err, ErrReplyRejected)
	assert.Zero(p.sends.Load())
}

func TestReplyExplicitResolutionRequiresVerifiedProviderEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, p, target, _ := replyFixture(t)
	preview, err := s.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	p.sendErr = errors.New("timeout")
	_, err = s.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.ErrorIs(err, ErrReplyUncertain)
	p.comment.Snippet.TextOriginal = "Wrong body"
	_, err = s.Resolve(t.Context(), target, "Example reply", preview.Token, "sent-1")
	require.Error(err)
	p.comment.Snippet.TextOriginal = "Example reply"
	receipt, err := s.Resolve(t.Context(), target, "Example reply", preview.Token, "sent-1")
	require.NoError(err)
	assert.Equal("archived", receipt.State)
	assert.Equal(int32(1), p.sends.Load())
}

// A parent losing eligibility after acceptance cannot prevent local archival.
func TestReplyArchiveRecoveryDoesNotRequireCurrentSendEligibility(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, p, target, _ := replyFixture(t)
	preview, err := s.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	persist := s.Persist
	s.Persist = func(context.Context, Comment) (int64, error) { return 0, errors.New("disk unavailable") }
	accepted, err := s.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.Error(err)
	require.NotNil(accepted)
	assert.Equal("accepted", accepted.State)
	s.Persist = persist
	target.ParentStatus = "heldForReview"
	target.CanReply = false
	receipt, err := s.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.NoError(err)
	assert.Equal("archived", receipt.State)
	assert.Equal(int64(42), receipt.MessageID)
	assert.Equal(int32(1), p.sends.Load())
}

func TestReplyPreviewAcceptsOpaqueNestedCommentIDs(t *testing.T) {
	s, _, target, _ := replyFixture(t)
	target.CommentID = "parent-1.reply-2"
	receipt, err := s.Preview(t.Context(), target, "Example reply")
	require.NoError(t, err)
	assert.Equal(t, "parent-1.reply-2", receipt.Target.CommentID)
}

// Send ingestion and later polling must converge on one owner/message identity.
func TestReplyAcceptedCommentAndPollingShareArchiveMapping(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, p, _, _ := replyFixture(t)
	st := s.Store
	source, err := st.GetOrCreateSource(SourceType, SourceIdentifier("owner-1"))
	require.NoError(err)
	video := Video{ID: "video-1", Snippet: VideoSnippet{ChannelID: "owner-1", Title: "Example video", Description: "Example description"}}
	rootID, _, err := persistVideo(t.Context(), st, source.ID, "owner-1", video)
	require.NoError(err)
	var parent Comment
	require.NoError(json.Unmarshal([]byte(`{"id":"parent-1","snippet":{"videoId":"video-1","textOriginal":"Example parent","moderationStatus":"published","authorChannelId":{"value":"other-author"}}}`), &parent))
	allowed := true
	parent.ThreadID = "thread-1"
	parent.CanReply = &allowed
	parentID, err := PersistComment(t.Context(), st, source.ID, "owner-1", video, parent, "")
	require.NoError(err)
	target, err := ResolveReplyTarget(t.Context(), st, parentID)
	require.NoError(err)
	s.Persist = func(ctx context.Context, comment Comment) (int64, error) {
		return PersistComment(ctx, st, source.ID, "owner-1", video, comment, "")
	}
	preview, err := s.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	receipt, err := s.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.NoError(err)
	polledID, err := PersistComment(t.Context(), st, source.ID, "owner-1", video, p.comment, "")
	require.NoError(err)
	assert.Equal(receipt.MessageID, polledID)
	root, err := st.GetMessageContext(t.Context(), rootID)
	require.NoError(err)
	sent, err := st.GetMessageContext(t.Context(), receipt.MessageID)
	require.NoError(err)
	assert.Equal(root.FromParticipantID, sent.FromParticipantID)
	assert.True(sent.IsFromMe)
	assert.Equal("video-1", sent.SourceConversationID)
	replyTo, err := st.GetMessageReplyToMessageIDContext(t.Context(), receipt.MessageID)
	require.NoError(err)
	require.True(replyTo.Valid)
	assert.Equal(parentID, replyTo.Int64)
	assert.Equal("Example video", root.Subject)
}

func TestReplyAcceptedRetryRecomputesStatsWithoutOverwritingPolledEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, p, _, _ := replyFixture(t)
	st := s.Store
	source, err := st.GetOrCreateSource(SourceType, SourceIdentifier("owner-1"))
	require.NoError(err)
	video := Video{ID: "video-1", Snippet: VideoSnippet{
		ChannelID: "owner-1", Title: "Example video", Description: "Example description",
		PublishedAt: time.Date(2026, time.November, 1, 12, 0, 0, 0, time.UTC),
	}}
	_, _, err = persistVideo(t.Context(), st, source.ID, "owner-1", video)
	require.NoError(err)
	var parent Comment
	require.NoError(json.Unmarshal([]byte(`{"id":"parent-1","snippet":{"videoId":"video-1","textOriginal":"Example parent","moderationStatus":"published","publishedAt":"2026-11-02T12:00:00Z","authorChannelId":{"value":"other-author"}}}`), &parent))
	allowed := true
	parent.ThreadID = "thread-1"
	parent.CanReply = &allowed
	parentID, err := PersistComment(t.Context(), st, source.ID, "owner-1", video, parent, "")
	require.NoError(err)
	target, err := ResolveReplyTarget(t.Context(), st, parentID)
	require.NoError(err)

	var persistCalls atomic.Int32
	s.Persist = func(ctx context.Context, comment Comment) (int64, error) {
		persistCalls.Add(1)
		id, _, persistErr := persistComment(ctx, st, source.ID, "owner-1", video, comment)
		if persistErr != nil {
			return id, persistErr
		}
		return id, errors.New("conversation statistics update failed")
	}
	preview, err := s.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	first, err := s.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.ErrorContains(err, "conversation statistics update failed")
	require.NotNil(first)
	assert.Equal("accepted", first.State)
	assert.Equal(int32(1), p.sends.Load())

	stored, err := st.StoredMessagesContext(t.Context(), source.ID, "youtube_json", []string{"sent-1"})
	require.NoError(err)
	reply, ok := stored["sent-1"]
	require.True(ok)

	polled := p.comment
	polled.Snippet.TextOriginal = "Edited reply"
	polled.Snippet.TextDisplay = "Edited reply"
	polledID, _, err := persistComment(t.Context(), st, source.ID, "owner-1", video, polled)
	require.NoError(err)
	assert.Equal(reply.ID, polledID)

	var messageCount int
	var lastMessageAt sql.NullTime
	var lastMessagePreview sql.NullString
	queryStats := func() {
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
			SELECT message_count, last_message_at, last_message_preview
			FROM conversations WHERE id = (SELECT conversation_id FROM messages WHERE id = ?)
		`), reply.ID).Scan(&messageCount, &lastMessageAt, &lastMessagePreview))
	}
	queryStats()
	assert.Equal(2, messageCount)
	assert.Equal("Example parent", lastMessagePreview.String)

	recovered, err := s.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.NoError(err)
	assert.Equal("archived", recovered.State)
	assert.Equal(reply.ID, recovered.MessageID)
	assert.Equal(int32(1), persistCalls.Load())
	assert.Equal(int32(1), p.sends.Load())

	queryStats()
	assert.Equal(3, messageCount)
	assert.True(lastMessageAt.Valid)
	assert.True(p.comment.Snippet.PublishedAt.Equal(lastMessageAt.Time))
	assert.Equal("Edited reply", lastMessagePreview.String)
	matches, err := st.MessageContentMatchesContext(t.Context(), reply.ID,
		sql.NullString{String: "Edited reply", Valid: true},
		store.FTSDoc{Subject: "Example video", Body: "Edited reply"},
	)
	require.NoError(err)
	assert.True(matches)
	stored, err = st.StoredMessagesContext(t.Context(), source.ID, "youtube_json", []string{"sent-1"})
	require.NoError(err)
	var archived Comment
	require.NoError(json.Unmarshal(stored["sent-1"].Raw, &archived))
	assert.Equal("Edited reply", archived.Snippet.TextOriginal)
}

// The provider comment resource may omit videoId; its verified parent is the
// stable association to the video checked immediately before dispatch.
func TestReplyAcceptsCommentWithoutOptionalVideoID(t *testing.T) {
	s, p, target, _ := replyFixture(t)
	p.comment.Snippet.VideoID = ""
	preview, err := s.Preview(t.Context(), target, "Example reply")
	require.NoError(t, err)
	receipt, err := s.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.NoError(t, err)
	assert.Equal(t, "archived", receipt.State)
}

// A proven rejection consumed the old confirmation but did not deliver a reply.
func TestReplyFreshConfirmationCanRetryDefiniteRejection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, p, target, _ := replyFixture(t)
	first, err := s.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	p.sendErr = ErrReplyRejected
	_, err = s.Confirm(t.Context(), target, "Example reply", first.Token)
	require.ErrorIs(err, ErrReplyRejected)
	p.sendErr = nil
	second, err := s.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	require.NotEqual(first.Token, second.Token)
	_, err = s.Confirm(t.Context(), target, "Example reply", first.Token)
	require.Error(err)
	receipt, err := s.Confirm(t.Context(), target, "Example reply", second.Token)
	require.NoError(err)
	assert.Equal("archived", receipt.State)
	assert.Equal(int32(2), p.sends.Load())
}

func TestReplyRemovedSourceCannotReopenUncertainSend(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, p, target, _ := replyFixture(t)
	preview, err := s.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	p.sendErr = errors.New("fixture transport lost")
	_, err = s.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.ErrorIs(err, ErrReplyUncertain)
	require.NoError(s.Store.RemoveSource(target.SourceID))
	_, err = s.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.Error(err)
	source, err := s.Store.GetOrCreateSource(SourceType, SourceIdentifier(target.ChannelID))
	require.NoError(err)
	target.SourceID = source.ID
	target.MessageID += 100
	_, err = s.Preview(t.Context(), target, "Example reply")
	require.Error(err)
	assert.Equal(int32(1), p.sends.Load())
	key, err := replyKey(target, "Example reply")
	require.NoError(err)
	raw, err := s.Store.GetYouTubeReplyReceiptContext(t.Context(), key)
	require.NoError(err)
	assert.JSONEq(`{"state":"removed"}`, raw)
}

func TestReplyMergeDiscardsPreparedPreviewBeforeSourceRetirement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, p, target, _ := replyFixture(t)
	preview, err := s.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	destination, err := s.Store.GetOrCreateSource(SourceType, SourceIdentifier("destination-owner"))
	require.NoError(err)
	_, err = s.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: target.SourceID,
		IntoSourceID: destination.ID,
	})
	require.NoError(err)

	_, err = s.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.ErrorContains(err, "preview was discarded")
	_, err = s.Preview(t.Context(), target, "Example reply")
	require.ErrorContains(err, "preview was discarded")
	assert.Zero(p.checks.Load())
	assert.Zero(p.sends.Load())

	key, err := replyKey(target, "Example reply")
	require.NoError(err)
	raw, err := s.Store.GetYouTubeReplyReceiptContext(t.Context(), key)
	require.NoError(err)
	assert.JSONEq(`{"state":"discarded"}`, raw)
}
