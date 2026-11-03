package store_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func metaUpdateMessage(t *testing.T, st *store.Store, sourceID int64, conversation, mid, metadata string) int64 {
	t.Helper()
	require := require.New(t)
	actor, err := st.EnsureParticipantByIdentifier("facebook_messages", "synthetic:300", "Example Customer")
	require.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:      &store.Message{SourceID: sourceID, SourceMessageID: mid, MessageType: "facebook_messages", SenderID: sql.NullInt64{Int64: actor, Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: conversation, ConversationType: "direct_chat"},
		Metadata:     &sql.NullString{String: metadata, Valid: true}, BodyText: sql.NullString{String: "original", Valid: true},
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{actor}, DisplayNames: []string{"Example Customer"}}},
		FTS:        &store.FTSDoc{Body: "original", FromAddr: "Example Customer"},
	})
	require.NoError(err)
	return id
}

func TestMetaMessageAtomicUpdates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("facebook_messages", "app:9:page:100")
	require.NoError(err)
	prior := sql.NullString{String: `{"version":1}`, Valid: true}
	next := sql.NullString{String: `{"version":1,"edit_count":1}`, Valid: true}
	id := metaUpdateMessage(t, st, src.ID, "customer:300", "m_1", prior.String)
	prior, err = st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	before, err := st.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	text := "edited inbox"
	require.NoError(st.ApplyMetaMessageUpdateContext(t.Context(), id, store.MetaMessageUpdate{PreviousMetadata: prior, Metadata: next, Text: &text}))
	after, err := st.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	assert.Equal(before+1, after, "an edited inbox row invalidates published analytics")
	next, err = st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal(text, body)
	var size int64
	var edited bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT size_estimate,is_edited FROM messages WHERE id=?`), id).Scan(&size, &edited))
	assert.Equal(int64(len(text)), size)
	assert.True(edited)
	// A stale projection cannot overwrite text or the newer replay marker.
	stale := "stale"
	require.ErrorContains(st.ApplyMetaMessageUpdateContext(t.Context(), id, store.MetaMessageUpdate{PreviousMetadata: prior, Metadata: prior, Text: &stale}), "metadata changed")
	revision, err := st.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	assert.Equal(after, revision, "a rejected edit must not invalidate the cache")
	body, err = st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal(text, body)
	actor, err := st.EnsureParticipantByIdentifier("facebook_messages", "synthetic:300", "Example Customer")
	require.NoError(err)
	for _, value := range []string{"❤", "👍", ""} {
		reaction := &store.MetaMessageReaction{SourceReactionID: "m_1|synthetic:300", ParticipantID: actor, Value: value, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
		require.NoError(st.ApplyMetaMessageUpdateContext(t.Context(), id, store.MetaMessageUpdate{PreviousMetadata: next, Metadata: next, Reaction: reaction}))
		var count int
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM reactions WHERE message_id=?`), id).Scan(&count))
		if value == "" {
			assert.Zero(count)
		} else {
			assert.Equal(1, count)
		}
	}
	require.Error(st.ApplyMetaMessageUpdateContext(t.Context(), id, store.MetaMessageUpdate{}))
}

func TestMetaMessageEditLimitsSnippetWithoutTruncatingBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("facebook_messages", "app:9:page:100")
	require.NoError(err)
	text := strings.Repeat("界", 499) + "🧪 searchtailneedle"
	wantSnippet := strings.Repeat("界", 499) + "🧪"
	id := metaUpdateMessage(t, st, src.ID, "customer:300", "m_1", `{"version":1}`)
	previous, err := st.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	next := sql.NullString{String: `{"version":1,"edit_count":1}`, Valid: true}
	require.NoError(st.ApplyMetaMessageUpdateContext(t.Context(), id, store.MetaMessageUpdate{PreviousMetadata: previous, Metadata: next, Text: &text}))

	var snippet string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT snippet FROM messages WHERE id=?`), id).Scan(&snippet))
	assert.Equal(wantSnippet, snippet)
	assert.NotContains(snippet, "searchtailneedle")
	body, err := st.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal(text, body)
	var hits int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM messages_fts WHERE rowid=? AND messages_fts MATCH 'searchtailneedle'`), id).Scan(&hits))
	assert.Equal(1, hits)
}

func FuzzMetaMessageEditSnippetIsBounded(f *testing.F) {
	f.Add(strings.Repeat("a", 499) + "🧪 tail")
	f.Add(strings.Repeat("é", 510))
	f.Add("")
	f.Fuzz(func(t *testing.T, text string) {
		if !utf8.ValidString(text) || utf8.RuneCountInString(text) > 2048 {
			t.Skip()
		}
		require := require.New(t)
		assert := assert.New(t)
		st := testutil.NewTestStore(t)
		src, err := st.GetOrCreateSource("facebook_messages", "app:9:page:100")
		require.NoError(err)
		id := metaUpdateMessage(t, st, src.ID, "customer:300", "m_1", `{"version":1}`)
		previous, err := st.GetMessageMetadataContext(t.Context(), id)
		require.NoError(err)
		next := sql.NullString{String: `{"version":1,"edit_count":1}`, Valid: true}
		require.NoError(st.ApplyMetaMessageUpdateContext(t.Context(), id, store.MetaMessageUpdate{PreviousMetadata: previous, Metadata: next, Text: &text}))
		var snippet string
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT snippet FROM messages WHERE id=?`), id).Scan(&snippet))
		runes := []rune(text)
		if len(runes) > store.MetaMessageSnippetRunes {
			runes = runes[:store.MetaMessageSnippetRunes]
		}
		assert.Equal(string(runes), snippet)
		assert.LessOrEqual(utf8.RuneCountInString(snippet), store.MetaMessageSnippetRunes)
		body, err := st.GetMessageBodyText(id)
		require.NoError(err)
		assert.Equal(text, body)
	})
}

func TestMetaMessageTargetedReplies(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("facebook_messages", "app:9:page:100")
	require.NoError(err)
	child := metaUpdateMessage(t, st, src.ID, "customer:300", "m_child", `{"version":1,"referenced_message_id":"m_parent"}`)
	other := metaUpdateMessage(t, st, src.ID, "customer:301", "m_other", `{"version":1,"referenced_message_id":"m_parent"}`)
	future := metaUpdateMessage(t, st, src.ID, "customer:300", "m_future", `{"version":999,"referenced_message_id":"m_parent"}`)
	require.NoError(st.ResolveMetaMessageRepliesContext(t.Context(), src.ID, child, 1))
	parent := metaUpdateMessage(t, st, src.ID, "customer:300", "m_parent", `{"version":1}`)
	require.NoError(st.ResolveMetaMessageRepliesContext(t.Context(), src.ID, parent, 1))
	for _, id := range []int64{child, other, future} {
		var reply sql.NullInt64
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), id).Scan(&reply))
		if id == child {
			assert.Equal(parent, reply.Int64)
		} else {
			assert.False(reply.Valid)
		}
	}
	late := metaUpdateMessage(t, st, src.ID, "customer:300", "m_late", `{"version":1,"referenced_message_id":"m_parent"}`)
	require.NoError(st.ResolveMetaMessageRepliesContext(t.Context(), src.ID, late, 1))
	var reply sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), late).Scan(&reply))
	assert.Equal(parent, reply.Int64)
	require.Error(st.ResolveMetaMessageRepliesContext(t.Context(), src.ID+1, child, 1))
}
