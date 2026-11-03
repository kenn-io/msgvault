package meta

import (
	"database/sql"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func mentionStore(t *testing.T) (*store.Store, mentionScope) {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource("instagram_mentions", InstagramSourceIdentifier("9", "100", mentionIG))
	require.NoError(t, err)
	return st, mentionScope{SourceID: src.ID, App: "9", Page: "100", IG: mentionIG}
}

func mentionRoot() IGMentionMedia {
	return IGMentionMedia{ID: mentionMedia, Caption: "hi acct", Username: "example_user", MediaType: "IMAGE", Timestamp: "2026-10-30T10:00:00+0000"}
}

func TestPersistMentionMediaCreatesConversation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, s := mentionStore(t)
	id, err := persistMentionMedia(t.Context(), st, s, mentionRoot(), surfaceCaptionMention)
	requirements.NoError(err)
	m := message(t, st, s.SourceID, mentionMedia)
	assertions.Equal(id, m.ID)
	assertions.Equal("instagram_mentions", m.MessageType)
	assertions.False(m.IsFromMe)
	assertions.False(m.SenderID.Valid)
	var title, kind, native string
	requirements.NoError(st.DB().QueryRow(`SELECT title, conversation_type, source_conversation_id FROM conversations WHERE id=?`, m.ConversationID).Scan(&title, &kind, &native))
	assertions.Equal("@example_user: hi acct", title)
	assertions.Equal("public_comments", kind)
	assertions.Equal(mentionMedia, native)
	stored, err := st.StoredMessagesContext(t.Context(), s.SourceID, "instagram-mention-json", []string{mentionMedia})
	requirements.NoError(err)
	requirements.Len(stored, 1)
	var raw IGMentionMedia
	requirements.NoError(json.Unmarshal(stored[mentionMedia].Raw, &raw))
	assertions.Equal(mentionRoot(), raw)
	for _, term := range []string{"acct", "example_user"} {
		var count int
		requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`, term).Scan(&count))
		assertions.Equal(1, count)
	}
}

func TestMentionUsernameSearchSurvivesFTSRebuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, scope := mentionStore(t)
	root := mentionRoot()
	root.Caption = "A caption without the author's handle"
	id, err := persistMentionMedia(t.Context(), st, scope, root, surfaceTagged)
	require.NoError(err)
	checkSearch := func() {
		t.Helper()
		var count int
		require.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`, root.Username).Scan(&count))
		assert.Equal(1, count)
	}
	checkSearch()
	_, err = st.RebuildFTSContext(t.Context(), nil)
	require.NoError(err)
	checkSearch()
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal(root.Caption, body)
	var participants int
	require.NoError(st.DB().QueryRow(`SELECT count(*) FROM participants`).Scan(&participants))
	assert.Zero(participants, "a foreign handle must not create a linkable author")
}

func TestMentionCommentBeforeCaptionConverges(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, s := mentionStore(t)
	root := mentionRoot()
	child := IGMentionComment{ID: mentionComment, Text: "hello acct", Timestamp: "2026-10-30T11:00:00Z", Media: &root}
	id, err := persistMentionComment(t.Context(), st, s, child)
	requirements.NoError(err)
	_, err = persistMentionMedia(t.Context(), st, s, root, surfaceCaptionMention)
	requirements.NoError(err)
	again, err := persistMentionComment(t.Context(), st, s, child)
	requirements.NoError(err)
	assertions.Equal(id, again)
	m, r := message(t, st, s.SourceID, mentionComment), message(t, st, s.SourceID, mentionMedia)
	assertions.Equal(r.ID, m.ReplyToMessageID.Int64)
	assertions.Equal(r.ConversationID, m.ConversationID)
	saved, err := st.GetMessageMetadata(id)
	requirements.NoError(err)
	var meta mentionMetadata
	requirements.NoError(json.Unmarshal([]byte(saved.String), &meta))
	assertions.Equal(mentionComment, meta.CommentID)
	assertions.False(meta.ParentKnown)
	assertions.True(meta.CommentMention)
	var count int
	for table, want := range map[string]int{"messages": 2, "conversations": 1, "participants": 0, "message_recipients": 0} {
		requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count))
		assertions.Equal(want, count, table)
	}
}

func TestMentionSurfacesOrTogether(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, order := range [][]mentionSurface{{surfaceTagged, surfaceCaptionMention}, {surfaceCaptionMention, surfaceTagged}} {
		st, s := mentionStore(t)
		var id int64
		for _, surface := range order {
			got, err := persistMentionMedia(t.Context(), st, s, mentionRoot(), surface)
			requirements.NoError(err)
			if id != 0 {
				assertions.Equal(id, got)
			}
			id = got
		}
		saved, err := st.GetMessageMetadata(id)
		requirements.NoError(err)
		assertions.Contains(saved.String, `"tagged":true`)
		assertions.Contains(saved.String, `"caption_mention":true`)
	}
}

func TestMentionImmutableProvenanceRefused(t *testing.T) {
	for _, field := range []string{"version", "app_id", "page_id", "ig_user_id", "media_id", "comment_id"} {
		t.Run(field, func(t *testing.T) {
			requirements := require.New(t)
			st, s := mentionStore(t)
			root := mentionRoot()
			child := IGMentionComment{ID: mentionComment, Text: "original", Media: &root}
			id, err := persistMentionComment(t.Context(), st, s, child)
			requirements.NoError(err)
			saved, err := st.GetMessageMetadata(id)
			requirements.NoError(err)
			var meta map[string]any
			requirements.NoError(json.Unmarshal([]byte(saved.String), &meta))
			meta[field] = "999"
			if field == "version" {
				meta[field] = 2
			}
			data, err := json.Marshal(meta)
			requirements.NoError(err)
			requirements.NoError(st.SetMessageMetadata(id, sql.NullString{String: string(data), Valid: true}))
			child.Text = "must not overwrite"
			_, err = persistMentionComment(t.Context(), st, s, child)
			requirements.Error(err)
			body, err := st.GetMessageBodyText(id)
			requirements.NoError(err)
			assert.Equal(t, "original", body)
		})
	}
}

func TestPersistMentionTitleFallback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, s := mentionStore(t)
	for _, caption := range []string{"", strings.Repeat("界", 130) + "\nsecond line"} {
		root := mentionRoot()
		root.Caption = caption
		_, err := persistMentionMedia(t.Context(), st, s, root, surfaceTagged)
		requirements.NoError(err)
		var title string
		requirements.NoError(st.DB().QueryRow(`SELECT title FROM conversations WHERE source_id=?`, s.SourceID).Scan(&title))
		if caption == "" {
			assertions.Equal("Instagram media "+mentionMedia, title)
		} else {
			assertions.LessOrEqual(len([]rune(title)), 120)
		}
	}
}

func TestMentionCommentExpansionPreservesRoot(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, scope := mentionStore(t)
	id, err := persistMentionMedia(t.Context(), st, scope, mentionRoot(), surfaceTagged)
	requirements.NoError(err)
	thin := IGMentionMedia{ID: mentionMedia}
	_, err = persistMentionComment(t.Context(), st, scope, IGMentionComment{ID: mentionComment, Text: "hello", Media: &thin})
	requirements.NoError(err)
	body, err := st.GetMessageBodyText(id)
	requirements.NoError(err)
	assertions.Equal("hi acct", body)
	var title string
	requirements.NoError(st.DB().QueryRow(`SELECT title FROM conversations WHERE source_id=?`, scope.SourceID).Scan(&title))
	assertions.Equal("@example_user: hi acct", title)
	saved, err := st.GetMessageMetadata(id)
	requirements.NoError(err)
	assertions.Contains(saved.String, `"owner_username":"example_user"`)
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'example_user'`).Scan(&count))
	assertions.Equal(1, count)
}
