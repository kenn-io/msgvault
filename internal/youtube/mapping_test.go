package youtube

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	yt "google.golang.org/api/youtube/v3"
	"testing"
	"time"
)

func TestVideoOwnerReceivesRelationshipActivityWithoutCommenting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, SourceIdentifier("UCfixture"))
	require.NoError(err)
	owner, err := st.EnsureParticipantByIdentifierContext(t.Context(), SourceType, SourceIdentifier("UCfixture"), "Fixture channel")
	require.NoError(err)
	require.NoError(st.AddAccountIdentityContext(t.Context(), src.ID, SourceIdentifier("UCfixture"), "youtube_channel"))
	v := yt.Video{Id: "videoone", Snippet: &yt.VideoSnippet{ChannelId: "UCfixture", Title: "Fixture video"}}
	_, _, err = persistVideo(t.Context(), st, src.ID, v)
	require.NoError(err)
	when := time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC)
	c := yt.Comment{Id: "comment", Snippet: &yt.CommentSnippet{TextOriginal: "Viewer comment", PublishedAt: when.Format(time.RFC3339), AuthorDisplayName: "Fixture viewer"}}
	c.Snippet.AuthorChannelId = &yt.CommentSnippetAuthorChannelId{Value: "UCviewer"}
	comment, err := persistCommentForTest(t.Context(), st, src.ID, v, c)
	require.NoError(err)
	var conversation, author int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT conversation_id, sender_id FROM messages WHERE id = ?`, comment).Scan(&conversation, &author))
	require.NotEqual(owner, author)

	rows, err := st.DB().QueryContext(t.Context(), `SELECT participant_id FROM conversation_participants WHERE conversation_id = ?`, conversation)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var members []int64
	for rows.Next() {
		var member int64
		require.NoError(rows.Scan(&member))
		members = append(members, member)
	}
	require.NoError(rows.Err())
	assert.ElementsMatch([]int64{owner, author}, members)
}

func TestCommentCannotMoveBetweenVideos(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, SourceIdentifier("UCfixture"))
	requirements.NoError(err)
	v := yt.Video{Id: "videoone", Snippet: &yt.VideoSnippet{ChannelId: "UCfixture", Title: "Original title"}}
	_, _, err = persistVideo(t.Context(), st, src.ID, v)
	requirements.NoError(err)
	c := yt.Comment{Id: "comment", Snippet: &yt.CommentSnippet{ModerationStatus: "published", TextDisplay: "Hello"}}
	_, err = persistCommentForTest(t.Context(), st, src.ID, v, c)
	requirements.NoError(err)
	v.Id = "videotwo"
	_, _, err = persistVideo(t.Context(), st, src.ID, v)
	requirements.NoError(err)
	_, err = persistCommentForTest(t.Context(), st, src.ID, v, c)
	requirements.Error(err)
	rows, err := st.MessageMetadataBatch(t.Context(), src.ID, []string{"comment"})
	requirements.NoError(err)
	assertions.Contains(rows["comment"].Metadata.String, `"video_id":"videoone"`)
}

func TestYouTubeSnapshotsPreserveRecipientsAndRepairReplyLinks(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, SourceIdentifier("UCfixture"))
	requirements.NoError(err)

	journalCount := func(messageID int64) int {
		var count int
		requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM cache_related_change_journal
			WHERE dataset = 'message_facts' AND message_id = ?`, messageID).Scan(&count))
		return count
	}

	v := yt.Video{Id: "videoone", Snippet: &yt.VideoSnippet{ChannelId: "UCfixture", Title: "Fixture video", Description: "Fixture description"}}
	videoID, _, err := persistVideo(t.Context(), st, src.ID, v)
	requirements.NoError(err)
	body, err := st.GetMessageBodyText(t.Context(), videoID)
	requirements.NoError(err)
	assertions.Equal("Fixture description\n\nhttps://www.youtube.com/watch?v=videoone", body)
	videoJournalCount := journalCount(videoID)
	_, _, err = persistVideo(t.Context(), st, src.ID, v)
	requirements.NoError(err)
	assertions.Equal(videoJournalCount, journalCount(videoID))

	c := yt.Comment{
		Id:      "comment",
		Snippet: &yt.CommentSnippet{VideoId: v.Id, TextDisplay: "Fixture comment", AuthorDisplayName: "Fixture author", ModerationStatus: "published"},
	}
	c.Snippet.AuthorChannelId = &yt.CommentSnippetAuthorChannelId{Value: "UCauthor"}
	commentID, _, err := persistComment(t.Context(), st, src.ID, v, c)
	requirements.NoError(err)
	stored, err := st.StoredMessagesContext(t.Context(), src.ID, "youtube_json", []string{"comment"})
	requirements.NoError(err)
	assertions.Equal("Fixture video", stored["comment"].Subject.String)
	commentJournalCount := journalCount(commentID)
	// Observation-only replay must succeed even when snapshot tables reject writes.
	_, err = st.DB().ExecContext(t.Context(), `
		CREATE TRIGGER forbid_body_replay BEFORE INSERT ON message_bodies
		BEGIN SELECT RAISE(ABORT, 'unexpected body rewrite'); END;
		CREATE TRIGGER forbid_raw_replay BEFORE INSERT ON message_raw
		BEGIN SELECT RAISE(ABORT, 'unexpected raw rewrite'); END;`)
	requirements.NoError(err)
	replayedID, change, err := persistComment(t.Context(), st, src.ID, v, c)
	requirements.NoError(err)
	assertions.Equal(commentID, replayedID)
	assertions.Equal(messageUnchanged, change)
	_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER forbid_body_replay; DROP TRIGGER forbid_raw_replay;`)
	requirements.NoError(err)
	assertions.Equal(commentJournalCount, journalCount(commentID))
}

func TestCommentParentIsPresentInInitialTransaction(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, SourceIdentifier("UCfixture"))
	requirements.NoError(err)
	v := yt.Video{Id: "videoone", Snippet: &yt.VideoSnippet{ChannelId: "UCfixture"}}
	root, _, err := persistVideo(t.Context(), st, src.ID, v)
	requirements.NoError(err)
	// A database invariant at INSERT detects the crash window before any
	// subsequent parent update can repair it. No provider replay is involved.
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fixture_require_parent BEFORE INSERT ON messages WHEN NEW.source_message_id = 'comment' AND NEW.reply_to_message_id IS NULL BEGIN SELECT RAISE(FAIL, 'parent missing at initial insert'); END`)
	requirements.NoError(err)
	id, err := persistCommentForTest(t.Context(), st, src.ID, v, yt.Comment{Id: "comment", Snippet: &yt.CommentSnippet{}})
	requirements.NoError(err)
	var parent int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT reply_to_message_id FROM messages WHERE id=?`, id).Scan(&parent))
	assertions.Equal(root, parent)
}

func TestModerationOmissionPreservesObservationAndExplicitChangeReplacesLabels(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, SourceIdentifier("UCfixture"))
	requirements.NoError(err)
	v := yt.Video{Id: "videoone", Snippet: &yt.VideoSnippet{ChannelId: "UCfixture"}}
	_, _, err = persistVideo(t.Context(), st, src.ID, v)
	requirements.NoError(err)
	c := yt.Comment{Id: "comment", Snippet: &yt.CommentSnippet{ModerationStatus: "heldForReview", TextDisplay: "Fallback text"}}
	id, err := persistCommentForTest(t.Context(), st, src.ID, v, c)
	requirements.NoError(err)
	messageLabels := func() []string {
		var labels []string
		rows, err := st.DB().QueryContext(t.Context(), `SELECT l.name FROM labels l JOIN message_labels ml ON ml.label_id=l.id WHERE ml.message_id=?`, id)
		requirements.NoError(err)
		defer func() { requirements.NoError(rows.Close()) }()
		for rows.Next() {
			var name string
			requirements.NoError(rows.Scan(&name))
			labels = append(labels, name)
		}
		requirements.NoError(rows.Err())
		return labels
	}
	assertions.Equal([]string{"YouTube: held for review"}, messageLabels())
	before, err := st.GetMessageMetadata(id)
	requirements.NoError(err)
	c.Snippet.ModerationStatus = ""
	_, err = persistCommentForTest(t.Context(), st, src.ID, v, c)
	requirements.NoError(err)
	after, err := st.GetMessageMetadata(id)
	requirements.NoError(err)
	assertions.Equal(before, after)
	c.Snippet.ModerationStatus = "likelySpam"
	_, err = persistCommentForTest(t.Context(), st, src.ID, v, c)
	requirements.NoError(err)
	assertions.Equal([]string{"YouTube: likely spam"}, messageLabels())
	c.Snippet.ModerationStatus = "published"
	_, err = persistCommentForTest(t.Context(), st, src.ID, v, c)
	requirements.NoError(err)
	assertions.Equal([]string{"YouTube moderation: published"}, messageLabels())
	body, err := st.GetMessageBodyText(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal("Fallback text", body)
}

func persistCommentForTest(ctx context.Context, st *store.Store, sourceID int64, v yt.Video, c yt.Comment) (int64, error) {
	id, _, err := persistCommentSDK(ctx, st, sourceID, "UCfixture", v.Id, &c)
	if err != nil {
		return id, err
	}
	return id, st.RecomputeConversationStatsForMessageContext(ctx, id)
}

func persistVideo(ctx context.Context, st *store.Store, sourceID int64, v yt.Video) (int64, messageChange, error) {
	if v.Status == nil {
		v.Status = &yt.VideoStatus{}
	}
	return persistVideoSDK(ctx, st, sourceID, "UCfixture", &v)
}
func persistComment(ctx context.Context, st *store.Store, sourceID int64, v yt.Video, c yt.Comment) (int64, messageChange, error) {
	if c.Snippet == nil {
		c.Snippet = &yt.CommentSnippet{}
	}
	return persistCommentSDK(ctx, st, sourceID, "UCfixture", v.Id, &c)
}

func TestCommentWithoutChannelIDPreservesDistinctAuthorNames(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, SourceIdentifier("UCfixture"))
	require.NoError(err)
	v := yt.Video{Id: "videoone", Snippet: &yt.VideoSnippet{ChannelId: "UCfixture"}}
	_, _, err = persistVideo(t.Context(), st, src.ID, v)
	require.NoError(err)
	ids := []int64{}
	for _, commentID := range []string{"comment-a", "comment-b"} {
		c := yt.Comment{Id: commentID, Snippet: &yt.CommentSnippet{AuthorDisplayName: "Alice", TextDisplay: "hello"}}
		id, _, err := persistComment(t.Context(), st, src.ID, v, c)
		require.NoError(err)
		ids = append(ids, id)
	}
	summaries, err := st.GetMessagesSummariesByIDsContext(t.Context(), ids)
	require.NoError(err)
	require.Len(summaries, 2)
	for _, summary := range summaries {
		require.Equal("Alice", summary.FromName)
	}
	saved, err := st.StoredMessagesContext(t.Context(), src.ID, "youtube_json", []string{"comment-a", "comment-b"})
	require.NoError(err)
	require.True(saved["comment-a"].SenderID.Valid)
	require.True(saved["comment-b"].SenderID.Valid)
	require.NotEqual(saved["comment-a"].SenderID.Int64, saved["comment-b"].SenderID.Int64)
	replay := yt.Comment{Id: "comment-a", Snippet: &yt.CommentSnippet{AuthorDisplayName: "Alice", TextDisplay: "hello"}}
	_, change, err := persistComment(t.Context(), st, src.ID, v, replay)
	require.NoError(err)
	require.Equal(messageUnchanged, change)
	c := yt.Comment{Id: "comment-a", Snippet: &yt.CommentSnippet{TextDisplay: "edited"}}
	_, _, err = persistComment(t.Context(), st, src.ID, v, c)
	require.NoError(err)
	summaries, err = st.GetMessagesSummariesByIDsContext(t.Context(), ids[:1])
	require.NoError(err)
	require.Len(summaries, 1)
	require.Equal("Alice", summaries[0].FromName)
	c.Snippet.AuthorChannelId = &yt.CommentSnippetAuthorChannelId{Value: "UCnew"}
	_, _, err = persistComment(t.Context(), st, src.ID, v, c)
	require.NoError(err)
	summaries, err = st.GetMessagesSummariesByIDsContext(t.Context(), ids[:1])
	require.NoError(err)
	assert.Equal(t, "Alice", summaries[0].FromName)
	var oldMembers int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM conversation_participants WHERE conversation_id = ? AND participant_id = ?`), saved["comment-a"].ConversationID, saved["comment-a"].SenderID.Int64).Scan(&oldMembers))
	require.Zero(oldMembers)
	c.Snippet.AuthorDisplayName = "Alice Updated"
	_, _, err = persistComment(t.Context(), st, src.ID, v, c)
	require.NoError(err)
	updated, err := st.StoredMessagesContext(t.Context(), src.ID, "youtube_json", []string{"comment-a", "comment-b"})
	require.NoError(err)
	require.NotEqual(saved["comment-a"].SenderID.Int64, updated["comment-a"].SenderID.Int64)
	require.Equal(saved["comment-b"].SenderID, updated["comment-b"].SenderID)
	summaries, err = st.GetMessagesSummariesByIDsContext(t.Context(), ids[:1])
	require.NoError(err)
	require.Equal("Alice Updated", summaries[0].FromName)
	{
		c := yt.Comment{Id: "comment", Snippet: &yt.CommentSnippet{AuthorDisplayName: "Owner", TextDisplay: "First"}}
		c.Snippet.AuthorChannelId = &yt.CommentSnippetAuthorChannelId{Value: "UCfixture"}
		id, _, err := persistComment(t.Context(), st, src.ID, v, c)
		require.NoError(err)
		before, err := st.StoredMessagesContext(t.Context(), src.ID, "youtube_json", []string{c.Id})
		require.NoError(err)
		require.True(before[c.Id].SourceIsFromMe.Valid && before[c.Id].SourceIsFromMe.Bool)
		c.Snippet.AuthorChannelId = &yt.CommentSnippetAuthorChannelId{Value: ""}
		c.Snippet.AuthorDisplayName = ""
		c.Snippet.TextDisplay = "Updated"
		_, _, err = persistComment(t.Context(), st, src.ID, v, c)
		require.NoError(err)
		after, err := st.StoredMessagesContext(t.Context(), src.ID, "youtube_json", []string{c.Id})
		require.NoError(err)
		require.Equal(before[c.Id].SenderID, after[c.Id].SenderID)
		require.Equal(before[c.Id].SourceIsFromMe, after[c.Id].SourceIsFromMe)
		summaries, err := st.GetMessagesSummariesByIDsContext(t.Context(), []int64{id})
		require.NoError(err)
		require.Len(summaries, 1)
		require.Equal("Owner", summaries[0].FromName)
		c.Snippet.AuthorDisplayName = "Owner Renamed"
		_, _, err = persistComment(t.Context(), st, src.ID, v, c)
		require.NoError(err)
		summaries, err = st.GetMessagesSummariesByIDsContext(t.Context(), []int64{id})
		require.NoError(err)
		require.Equal("Owner Renamed", summaries[0].FromName)
	}
}
