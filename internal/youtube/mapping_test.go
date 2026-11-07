package youtube

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/duckdbutil"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/testutil"
	"hegel.dev/go/hegel"
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
	v := Video{ID: "videoone", Snippet: VideoSnippet{ChannelID: "UCfixture", Title: "Fixture video"}}
	root, _, err := persistVideo(t.Context(), st, src.ID, "UCfixture", v)
	require.NoError(err)
	when := time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC)
	c := Comment{ID: "comment", Snippet: CommentSnippet{TextOriginal: "Viewer comment", PublishedAt: when, AuthorDisplayName: "Fixture viewer"}}
	c.Snippet.AuthorChannelID.Value = "UCviewer"
	comment, err := PersistComment(t.Context(), st, src.ID, "UCfixture", v, c, "")
	require.NoError(err)
	var conversation, author int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT conversation_id, sender_id FROM messages WHERE id = ?`, comment).Scan(&conversation, &author))
	require.NotEqual(owner, author)

	db, err := duckdbutil.Open(t.Context(), duckdbutil.Policy{
		MemoryLimit: "128MB", Threads: 1, TempDirectory: t.TempDir(), MaxTempDirectorySize: "256MB",
	})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(db.Close()) })
	_, err = db.ExecContext(t.Context(), `CREATE TABLE roster (conversation_id BIGINT, participant_id BIGINT);
		CREATE TABLE participants (id BIGINT, domain VARCHAR);
		CREATE TABLE clusters (participant_id BIGINT, canonical_id BIGINT);
		CREATE TABLE owners (source_id BIGINT, participant_id BIGINT)`)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO participants VALUES (?, ''), (?, '')`, owner, author)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO owners VALUES (?, ?)`, src.ID, owner)
	require.NoError(err)
	// The comment has only its viewer as a direct edge. Owner presence must
	// come from the conversation roster written by the YouTube importer.
	_, err = db.ExecContext(t.Context(), `CREATE TABLE sparse AS SELECT
		?::BIGINT AS message_id, ?::BIGINT AS conversation_id, ?::BIGINT AS source_id,
		'youtube' AS source_type, ?::TIMESTAMP AS occurred_at, 'youtube' AS message_type,
		'group_chat' AS conversation_type, `+identityindex.EntryKindSQL("'youtube'")+` AS entry_kind,
		`+identityindex.IsChatSQL("'youtube'", "'group_chat'")+` AS is_chat,
		false AS is_from_me, 0 AS attachment_count, false AS has_attachments,
		false AS deleted_from_source, false AS unsent, NULL::BIGINT AS canonical_id,
		NULL::VARCHAR AS participant_domain, false AS is_direct, false AS is_conversation_member,
		false AS is_sender, false AS is_author, false AS is_owner, 2026 AS occurred_year`,
		comment, conversation, src.ID, when)
	require.NoError(err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO sparse SELECT * REPLACE (
		?::BIGINT AS canonical_id, '' AS participant_domain,
		true AS is_direct, true AS is_sender, true AS is_author) FROM sparse`, author)
	require.NoError(err)

	for _, phase := range []string{"initial import", "unchanged replay"} {
		if phase == "unchanged replay" {
			// Simulate an archive written before the owner-roster repair.
			_, err = st.DB().ExecContext(t.Context(), `DELETE FROM conversation_participants WHERE conversation_id = ? AND participant_id = ?`, conversation, owner)
			require.NoError(err)
			replayed, change, replayErr := persistVideo(t.Context(), st, src.ID, "UCfixture", v)
			require.NoError(replayErr)
			assert.Equal(root, replayed)
			assert.Equal(messageUnchanged, change)
		}
		_, err = db.ExecContext(t.Context(), `DELETE FROM roster`)
		require.NoError(err)
		rows, queryErr := st.DB().QueryContext(t.Context(), `SELECT participant_id FROM conversation_participants WHERE conversation_id = ?`, conversation)
		require.NoError(queryErr)
		t.Cleanup(func() { require.NoError(rows.Close()) })
		var members []int64
		for rows.Next() {
			var member int64
			require.NoError(rows.Scan(&member))
			members = append(members, member)
		}
		require.NoError(rows.Err())
		require.NoError(rows.Close())
		assert.ElementsMatch([]int64{owner, author}, members, phase)
		for _, member := range members {
			_, err = db.ExecContext(t.Context(), `INSERT INTO roster VALUES (?, ?)`, conversation, member)
			require.NoError(err)
		}
		activity := identityindex.ExpandedActivityRelation("sparse", "roster", "participants", "clusters", "owners")
		facts := identityindex.RelationshipTemperatureFactsSQL(activity, when.Add(time.Hour))
		var received int64
		require.NoError(db.QueryRowContext(t.Context(), `SELECT count(*) FROM (`+facts+`) WHERE canonical_id = ? AND received`, author).Scan(&received))
		assert.Equal(int64(1), received, phase)
	}
}

func TestCommentCannotMoveBetweenVideos(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, SourceIdentifier("UCfixture"))
	requirements.NoError(err)
	v := Video{ID: "videoone", Snippet: VideoSnippet{ChannelID: "UCfixture", Title: "Original title"}}
	_, _, err = persistVideo(t.Context(), st, src.ID, "UCfixture", v)
	requirements.NoError(err)
	c := Comment{ID: "comment", Snippet: CommentSnippet{ModerationStatus: "published", TextDisplay: "Hello"}}
	_, err = PersistComment(t.Context(), st, src.ID, "UCfixture", v, c, "")
	requirements.NoError(err)
	v.ID = "videotwo"
	_, _, err = persistVideo(t.Context(), st, src.ID, "UCfixture", v)
	requirements.NoError(err)
	_, err = PersistComment(t.Context(), st, src.ID, "UCfixture", v, c, "")
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

	v := Video{ID: "videoone", Snippet: VideoSnippet{ChannelID: "UCfixture", Title: "Fixture video"}}
	videoID, _, err := persistVideo(t.Context(), st, src.ID, "UCfixture", v)
	requirements.NoError(err)
	videoJournalCount := journalCount(videoID)
	_, _, err = persistVideo(t.Context(), st, src.ID, "UCfixture", v)
	requirements.NoError(err)
	assertions.Equal(videoJournalCount, journalCount(videoID))

	canReply := true
	c := Comment{
		ID: "comment", ThreadID: "thread-one", CanReply: &canReply,
		Snippet: CommentSnippet{VideoID: v.ID, TextDisplay: "Fixture comment", AuthorDisplayName: "Fixture author", ModerationStatus: "published"},
	}
	c.Snippet.AuthorChannelID.Value = "UCauthor"
	commentID, _, err := persistComment(t.Context(), st, src.ID, "UCfixture", v, c)
	requirements.NoError(err)
	commentJournalCount := journalCount(commentID)
	_, err = st.DB().ExecContext(t.Context(), `UPDATE messages SET reply_to_message_id = NULL WHERE id = ?`, commentID)
	requirements.NoError(err)
	_, _, err = persistComment(t.Context(), st, src.ID, "UCfixture", v, c)
	requirements.NoError(err)
	var replyTo int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT reply_to_message_id FROM messages WHERE id = ?`, commentID).Scan(&replyTo))
	assertions.Equal(videoID, replyTo)
	assertions.Equal(commentJournalCount, journalCount(commentID))
	beforeObservation, err := st.GetMessageMetadataContext(t.Context(), commentID)
	requirements.NoError(err)
	var previousObservation commentMetadata
	requirements.NoError(json.Unmarshal([]byte(beforeObservation.String), &previousObservation))
	previousObservation.StatusObservedAt = time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	encodedObservation, err := json.Marshal(previousObservation)
	requirements.NoError(err)
	beforeObservation.String = string(encodedObservation)
	requirements.NoError(st.SetMessageMetadataContext(t.Context(), commentID, beforeObservation))
	_, err = st.DB().ExecContext(t.Context(), `DELETE FROM conversation_participants
		WHERE conversation_id = (SELECT conversation_id FROM messages WHERE id = ?)
		AND participant_id = (SELECT sender_id FROM messages WHERE id = ?)`, commentID, commentID)
	requirements.NoError(err)
	// Observation-only replay must succeed even when snapshot tables reject writes.
	_, err = st.DB().ExecContext(t.Context(), `
		CREATE TRIGGER forbid_body_replay BEFORE INSERT ON message_bodies
		BEGIN SELECT RAISE(ABORT, 'unexpected body rewrite'); END;
		CREATE TRIGGER forbid_raw_replay BEFORE INSERT ON message_raw
		BEGIN SELECT RAISE(ABORT, 'unexpected raw rewrite'); END;`)
	requirements.NoError(err)
	replayedID, change, err := persistComment(t.Context(), st, src.ID, "UCfixture", v, c)
	requirements.NoError(err)
	assertions.Equal(commentID, replayedID)
	assertions.Equal(messageUnchanged, change)
	_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER forbid_body_replay; DROP TRIGGER forbid_raw_replay;`)
	requirements.NoError(err)
	var authorMembership int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_participants cp
		JOIN messages m ON m.conversation_id = cp.conversation_id
		WHERE m.id = ? AND cp.participant_id = m.sender_id`, commentID).Scan(&authorMembership))
	assertions.Equal(1, authorMembership)
	assertions.Equal(commentJournalCount, journalCount(commentID))
	afterObservation, err := st.GetMessageMetadataContext(t.Context(), commentID)
	requirements.NoError(err)
	var before, after commentMetadata
	requirements.NoError(json.Unmarshal([]byte(beforeObservation.String), &before))
	requirements.NoError(json.Unmarshal([]byte(afterObservation.String), &after))
	assertions.True(after.StatusObservedAt.After(before.StatusObservedAt))

	c.ThreadID = "thread-two"
	_, _, err = persistComment(t.Context(), st, src.ID, "UCfixture", v, c)
	requirements.NoError(err)
	assertions.Equal(commentJournalCount, journalCount(commentID))
	metadata, err := st.GetMessageMetadataContext(t.Context(), commentID)
	requirements.NoError(err)
	var archived commentMetadata
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &archived))
	assertions.Equal("thread-two", archived.ThreadID)
	assertions.Equal("published", archived.ModerationStatus)
}

func TestCommentRejectsParentFromAnotherVideoBeforeWriting(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, SourceIdentifier("UCfixture"))
	requirements.NoError(err)
	v := Video{ID: "videoone", Snippet: VideoSnippet{ChannelID: "UCfixture"}}
	_, _, err = persistVideo(t.Context(), st, src.ID, "UCfixture", v)
	requirements.NoError(err)
	_, err = PersistComment(t.Context(), st, src.ID, "UCfixture", v, Comment{ID: "parent"}, "")
	requirements.NoError(err)
	v.ID = "videotwo"
	_, _, err = persistVideo(t.Context(), st, src.ID, "UCfixture", v)
	requirements.NoError(err)
	_, err = PersistComment(t.Context(), st, src.ID, "UCfixture", v, Comment{ID: "child", Snippet: CommentSnippet{ParentID: "parent"}}, "")
	requirements.Error(err)
	rows, err := st.MessageMetadataBatch(t.Context(), src.ID, []string{"child"})
	requirements.NoError(err)
	assertions.Empty(rows)
}

func TestCommentParentIsPresentInInitialTransaction(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, SourceIdentifier("UCfixture"))
	requirements.NoError(err)
	v := Video{ID: "videoone", Snippet: VideoSnippet{ChannelID: "UCfixture"}}
	root, _, err := persistVideo(t.Context(), st, src.ID, "UCfixture", v)
	requirements.NoError(err)
	// A database invariant at INSERT detects the crash window before any
	// subsequent parent update can repair it. No provider replay is involved.
	_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fixture_require_parent BEFORE INSERT ON messages WHEN NEW.source_message_id = 'comment' AND NEW.reply_to_message_id IS NULL BEGIN SELECT RAISE(FAIL, 'parent missing at initial insert'); END`)
	requirements.NoError(err)
	id, err := PersistComment(t.Context(), st, src.ID, "UCfixture", v, Comment{ID: "comment"}, "")
	requirements.NoError(err)
	var parent int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT reply_to_message_id FROM messages WHERE id=?`, id).Scan(&parent))
	assertions.Equal(root, parent)
}

func TestPaginationRejectsAlternatingTokensAcrossCheckpoints(t *testing.T) {
	requirements := require.New(t)
	job := work{Kind: "threads"}
	for _, token := range []string{"A", "B"} {
		next, err := nextPage(job, token)
		requirements.NoError(err)
		job = next[0]
		blob, err := json.Marshal(job)
		requirements.NoError(err)
		var restored work
		requirements.NoError(json.Unmarshal(blob, &restored))
		job = restored
	}
	_, err := nextPage(job, "A")
	requirements.Error(err)
}

func TestPaginationRecoveryProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		token := hegel.Draw(ht, hegel.Text())
		if token == "" {
			return
		} // No token terminates a listing; covered separately.
		next, err := nextPage(work{Token: "", Reset: true}, token)
		require.NoError(ht, err)
		require.Len(ht, next, 1)
		assert.False(ht, next[0].Reset)
		assert.Equal(ht, token, next[0].Token)
	}, hegel.WithTestCases(2000))
}

func TestModerationOmissionPreservesObservationAndExplicitChangeReplacesLabels(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(SourceType, SourceIdentifier("UCfixture"))
	requirements.NoError(err)
	v := Video{ID: "videoone", Snippet: VideoSnippet{ChannelID: "UCfixture"}}
	_, _, err = persistVideo(t.Context(), st, src.ID, "UCfixture", v)
	requirements.NoError(err)
	c := Comment{ID: "comment", Snippet: CommentSnippet{ModerationStatus: "heldForReview", TextDisplay: "Fallback text"}}
	id, err := PersistComment(t.Context(), st, src.ID, "UCfixture", v, c, "published")
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
	_, err = PersistComment(t.Context(), st, src.ID, "UCfixture", v, c, "likelySpam")
	requirements.NoError(err)
	after, err := st.GetMessageMetadata(id)
	requirements.NoError(err)
	assertions.Equal(before, after)
	c.Snippet.ModerationStatus = "likelySpam"
	_, err = PersistComment(t.Context(), st, src.ID, "UCfixture", v, c, "")
	requirements.NoError(err)
	assertions.Equal([]string{"YouTube: likely spam"}, messageLabels())
	c.Snippet.ModerationStatus = "published"
	_, err = PersistComment(t.Context(), st, src.ID, "UCfixture", v, c, "heldForReview")
	requirements.NoError(err)
	assertions.Equal([]string{"YouTube moderation: published"}, messageLabels())
	body, err := st.GetMessageBodyText(t.Context(), id)
	requirements.NoError(err)
	assertions.Equal("Fallback text", body)
}
