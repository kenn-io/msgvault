package meta

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func dmTestWriter(t *testing.T, platform DMPlatform) *dmWriter {
	t.Helper()
	require := require.New(t)

	st := testutil.NewSQLiteTestStore(t)
	src := DMSource{AppID: "9", PageID: "100", Platform: platform}
	if platform == PlatformInstagram {
		src.IGID = "1784"
	}
	s, err := st.GetOrCreateSource(src.Platform.SourceType(), src.Identifier())
	require.NoError(err)
	return &dmWriter{store: st, sourceID: s.ID, src: src}
}
func dmTestRecord(t *testing.T, w *dmWriter, mid, text string) dmRecord {
	t.Helper()
	require := require.New(t)

	owner := "100"
	if w.src.Platform == PlatformInstagram {
		owner = "1784"
	}
	r, err := recordFromEvent(w.src, messagingEvent{Sender: DMParticipant{ID: "300", Name: "Example Customer"}, Recipient: DMParticipant{ID: owner}, Timestamp: 1793577600123, Message: &dmEventMessage{Mid: mid, Text: text}})
	require.NoError(err)
	return r
}
func dmSavedMeta(t *testing.T, w *dmWriter, id int64) (dmMetadata, string) {
	t.Helper()
	require := require.New(t)

	blob, err := w.store.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	var m dmMetadata
	require.NoError(json.Unmarshal([]byte(blob.String), &m))
	return m, blob.String
}
func TestDMPersistText(t *testing.T) {
	for _, p := range []DMPlatform{PlatformMessenger, PlatformInstagram} {
		t.Run(string(p), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			w := dmTestWriter(t, p)
			id, added, err := w.persist(t.Context(), dmTestRecord(t, w, "m_1", "hello inbox"))
			require.NoError(err)
			assert.True(added)
			require.Positive(id)
			body, err := w.store.GetMessageBodyText(id)
			require.NoError(err)
			assert.Equal("hello inbox", body)
			var kind, key string
			var mine bool
			require.NoError(w.store.DB().QueryRow("SELECT m.message_type,c.source_conversation_id,m.is_from_me FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.id=?", id).Scan(&kind, &key, &mine))
			assert.Equal(p.SourceType(), kind)
			want := "psid:300"
			if p == PlatformInstagram {
				want = "igsid:300"
			}
			assert.Equal(want, key)
			assert.False(mine)
			var found int
			require.NoError(w.store.DB().QueryRow("SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'inbox'").Scan(&found))
			assert.Equal(1, found)
		})
	}
}
func TestDMPersistEchoIsFromMe(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	r, err := recordFromEvent(w.src, messagingEvent{Sender: DMParticipant{ID: "100"}, Recipient: DMParticipant{ID: "300"}, Timestamp: 1793577600123, Message: &dmEventMessage{Mid: "m_1", Text: "reply", IsEcho: true, AppID: []byte(`9`)}})
	require.NoError(err)
	id, _, err := w.persist(t.Context(), r)
	require.NoError(err)
	mine, err := w.store.GetMessageIsFromMe(id)
	require.NoError(err)
	assert.True(mine)
	m, _ := dmSavedMeta(t, w, id)
	assert.Equal("9", m.EchoAppID)
}

func TestDMPersistEchoRetainsCampaignAttribution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	r, err := recordFromEvent(w.src, messagingEvent{Sender: DMParticipant{ID: "100"}, Recipient: DMParticipant{ID: "300"}, Timestamp: 1793577600123, Message: &dmEventMessage{Mid: "m_campaign", Text: "campaign reply", IsEcho: true}})
	require.NoError(err)
	id, _, err := w.persist(t.Context(), r)
	require.NoError(err)
	meta, err := w.store.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	var attributed map[string]any
	require.NoError(json.Unmarshal([]byte(meta.String), &attributed))
	attributed["channel"] = "marketing"
	attributed["broadcast_id"] = "broadcast_example"
	attributed["campaign_id"] = "campaign_example"
	blob, err := json.Marshal(attributed)
	require.NoError(err)
	require.NoError(w.store.SetMessageMetadataContext(t.Context(), id, sql.NullString{Valid: true, String: string(blob)}))
	next, added, err := w.persist(t.Context(), r)
	require.NoError(err)
	assert.Equal(id, next)
	assert.False(added)
	saved, err := w.store.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	var got map[string]any
	require.NoError(json.Unmarshal([]byte(saved.String), &got))
	assert.Equal("marketing", got["channel"])
	assert.Equal("broadcast_example", got["broadcast_id"])
	assert.Equal("campaign_example", got["campaign_id"])
}

func TestDMPersistAttachmentsMetadataOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	w := dmTestWriter(t, PlatformMessenger)
	item := dmItem(w, "messages", `{"sender":{"id":"300"},"recipient":{"id":"100"},"timestamp":1793577600123,"message":{"mid":"m_1","attachments":[{"type":"sticker","payload":{"url":"https://cdn.example/secret-url","sticker_id":123}}]}}`)
	require.Equal(metawebhook.Applied, dmApply(t, dmTestHandler(t, w), item).Class)
	id := message(t, w.store, w.sourceID, "m_1").ID
	metadata, blob := dmSavedMeta(t, w, id)
	require.Len(metadata.Attachments, 1)
	assert.Equal("123", string(metadata.Attachments[0].StickerID))
	assert.NotContains(blob, "cdn.example")
	body, err := w.store.GetMessageBodyText(id)
	require.NoError(err)
	assert.Empty(body)
	var found int
	require.NoError(w.store.DB().QueryRow("SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'secret'").Scan(&found))
	assert.Zero(found)
}
func TestDMPersistNodeShares(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	w := dmTestWriter(t, PlatformMessenger)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":"m_1","from":{"id":"300"},"shares":{"data":[{"link":"https://cdn.example/one"},{"link":"https://cdn.example/two"}]}}`)
	})
	node, err := c.Message(t.Context(), "m_1")
	require.NoError(err)
	record, err := recordFromNode(w.src, node)
	require.NoError(err)
	id, _, err := w.persist(t.Context(), record)
	require.NoError(err)
	metadata, blob := dmSavedMeta(t, w, id)
	assert.Equal(2, metadata.Shares)
	assert.NotContains(blob, "cdn.example")
}
func TestDMPersistStoryReplyAndMention(t *testing.T) {
	for _, kind := range []string{"reply", "mention", "ephemeral"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			w := dmTestWriter(t, PlatformInstagram)
			var id int64
			if kind == "reply" {
				c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
					_, _ = fmt.Fprint(w, `{"id":"m_1","from":{"id":"300"},"reply_to":{"story":{"id":"s_1","url":"https://cdn.example/story"}}}`)
				})
				node, err := c.Message(t.Context(), "m_1")
				require.NoError(err)
				record, err := recordFromNode(w.src, node)
				require.NoError(err)
				id, _, err = w.persist(t.Context(), record)
				require.NoError(err)
			} else {
				attachment := kind
				if kind == "mention" {
					attachment = "story_mention"
				}
				item := dmItem(w, "messages", fmt.Sprintf(`{"sender":{"id":"300"},"recipient":{"id":"1784"},"timestamp":1793577600123,"message":{"mid":"m_1","attachments":[{"type":%q,"payload":{"url":"https://cdn.example/story"}}]}}`, attachment))
				require.Equal(metawebhook.Applied, dmApply(t, dmTestHandler(t, w), item).Class)
				id = message(t, w.store, w.sourceID, "m_1").ID
			}
			metadata, blob := dmSavedMeta(t, w, id)
			assert.NotContains(blob, "cdn.example")
			if kind == "ephemeral" {
				assert.Nil(metadata.Story)
			} else {
				require.NotNil(metadata.Story)
				assert.Equal(kind, metadata.Story.Kind)
			}
		})
	}
}
func TestDMPersistReferralAdID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	r, err := recordFromEvent(w.src, messagingEvent{Sender: DMParticipant{ID: "300"}, Recipient: DMParticipant{ID: "100"}, Timestamp: 1793577600123, Message: &dmEventMessage{Mid: "m_1", Text: "hello", Referral: &dmReferral{AdID: "700"}}})
	require.NoError(err)
	id, _, err := w.persist(t.Context(), r)
	require.NoError(err)
	meta, _ := dmSavedMeta(t, w, id)
	require.NotNil(meta.Referral)
	assert.Equal("700", meta.Referral.AdID)
}
func TestDMPersistUnsupported(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	r, err := recordFromNode(w.src, DMMessage{ID: "m_1", From: DMParticipant{ID: "300"}, IsUnsupported: true, Raw: []byte(`{}`)})
	require.NoError(err)
	id, _, err := w.persist(t.Context(), r)
	require.NoError(err)
	meta, _ := dmSavedMeta(t, w, id)
	assert.True(meta.IsUnsupported)
}
func TestDMPersistSameMidOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	r, err := recordFromNode(w.src, DMMessage{ID: "m_1", From: DMParticipant{ID: "300", Name: "Example Customer"}, Message: "hello", Raw: []byte(`{"id":"m_1"}`)})
	require.NoError(err)
	id, _, err := w.persist(t.Context(), r)
	require.NoError(err)
	first := message(t, w.store, w.sourceID, "m_1")
	require.True(first.SenderID.Valid)
	for _, echo := range []bool{false, true} {
		e := messagingEvent{Sender: DMParticipant{ID: "300", Name: "Example Customer"}, Recipient: DMParticipant{ID: "100"}, Timestamp: 1793577600123, Message: &dmEventMessage{Mid: "m_1", Text: "hello", IsEcho: echo}}
		if echo {
			e.Sender.ID, e.Recipient.ID = "100", "300"
		}
		replay, err := recordFromEvent(w.src, e)
		require.NoError(err)
		next, added, err := w.persist(t.Context(), replay)
		require.NoError(err)
		assert.Equal(id, next)
		assert.False(added)
	}
	r.CustomerName = ""
	r.Raw = []byte(`{}`)
	for range 2 {
		next, added, err := w.persist(t.Context(), r)
		require.NoError(err)
		assert.Equal(id, next)
		assert.False(added)
	}
	rows, err := w.store.StoredMessagesContext(t.Context(), w.sourceID, dmRawFormat, []string{"m_1"})
	require.NoError(err)
	assert.Len(rows, 1)
	assert.Equal(first.SenderID, rows["m_1"].SenderID)
	var name string
	require.NoError(w.store.DB().QueryRow("SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from'", id).Scan(&name))
	assert.Equal("Example Customer", name)
}
func TestDMReplyToOutOfOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	child := dmTestRecord(t, w, "m_child", "child")
	child.ReplyToMid = "m_parent"
	_, _, err := w.persist(t.Context(), child)
	require.NoError(err)
	// Recreate the writer to prove the missing-parent reference is durable.
	next := &dmWriter{store: w.store, sourceID: w.sourceID, src: w.src}
	parent, _, err := next.persist(t.Context(), dmTestRecord(t, w, "m_parent", "parent"))
	require.NoError(err)
	assert.Equal(parent, message(t, w.store, w.sourceID, "m_child").ReplyToMessageID.Int64)
}
func TestDMMetadataVersionRefused(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	r := dmTestRecord(t, w, "m_1", "original")
	id, _, err := w.persist(t.Context(), r)
	require.NoError(err)
	require.NoError(w.store.SetMessageMetadataContext(t.Context(), id, sql.NullString{Valid: true, String: `{"version":99}`}))
	r.Text = "replacement"
	_, _, err = w.persist(t.Context(), r)
	require.Error(err)
	body, err := w.store.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("original", body)
}

func TestDMConversationVersionRefusedBeforeWrites(t *testing.T) {
	for _, mid := range []string{"m_1", "m_2"} {
		t.Run(mid, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			w := dmTestWriter(t, PlatformMessenger)
			id, _, err := w.persist(t.Context(), dmTestRecord(t, w, "m_1", "original"))
			require.NoError(err)
			ref, err := w.store.MessageConversation(id)
			require.NoError(err)
			blob := sql.NullString{Valid: true, String: `{"version":2,"future_field":"retain"}`}
			require.NoError(w.store.SetConversationMetadataContext(t.Context(), ref.ConversationID, blob))
			_, _, err = w.persist(t.Context(), dmTestRecord(t, w, mid, "replacement"))
			require.Error(err)
			saved, err := w.store.GetConversationMetadataContext(t.Context(), ref.ConversationID)
			require.NoError(err)
			assert.Equal(blob, saved)
			body, err := w.store.GetMessageBodyText(id)
			require.NoError(err)
			assert.Equal("original", body)
			n, err := w.store.CountMessagesWithRaw(w.sourceID)
			require.NoError(err)
			assert.EqualValues(1, n)
		})
	}
}

func TestDMGraphStoryRetained(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformInstagram)
	rec, err := recordFromNode(w.src, DMMessage{ID: "m_story", From: DMParticipant{ID: "300"}, Story: &DMStory{ID: "story_example", URL: "https://cdn.example/story"}, Raw: []byte(`{}`)})
	require.NoError(err)
	id, _, err := w.persist(t.Context(), rec)
	require.NoError(err)
	m, blob := dmSavedMeta(t, w, id)
	require.NotNil(m.Story)
	assert.Equal("story_example", m.Story.ID)
	assert.NotContains(blob, "https://")
}
func TestDMTimestampMillisAndGraphTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	id, _, err := w.persist(t.Context(), dmTestRecord(t, w, "m_1", "text"))
	require.NoError(err)
	for _, timestamp := range []string{"2026-11-02T00:00:00+0000", "2026-11-02T00:00:00Z"} {
		r, err := recordFromNode(w.src, DMMessage{ID: "m_1", From: DMParticipant{ID: "300"}, CreatedTime: timestamp, Raw: []byte(`{}`)})
		require.NoError(err)
		_, _, err = w.persist(t.Context(), r)
		require.NoError(err)
		rows, err := w.store.StoredMessagesContext(t.Context(), w.sourceID, dmRawFormat, []string{"m_1"})
		require.NoError(err)
		assert.Equal(time.UnixMilli(1793577600123).UTC(), rows["m_1"].SentAt.Time.UTC())
	}
	require.Positive(id)
}
func TestDMReplayDoesNotRevertEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	r := dmTestRecord(t, w, "m_1", "original")
	id, _, err := w.persist(t.Context(), r)
	require.NoError(err)
	meta, _ := dmSavedMeta(t, w, id)
	meta.EditCount = 2
	blob, err := json.Marshal(meta)
	require.NoError(err)
	require.NoError(w.store.UpdateMessageDerivedText(id, sql.NullString{String: "edited", Valid: true}, sql.NullString{}, sql.NullString{String: "edited", Valid: true}, store.FTSDoc{Body: "edited"}))
	require.NoError(w.store.SetMessageMetadataContext(t.Context(), id, sql.NullString{Valid: true, String: string(blob)}))
	_, _, err = w.persist(t.Context(), r)
	require.NoError(err)
	node, err := recordFromNode(w.src, DMMessage{ID: "m_1", From: DMParticipant{ID: "300"}, Message: "original", Raw: []byte(`{}`)})
	require.NoError(err)
	_, _, err = w.persist(t.Context(), node)
	require.NoError(err)
	body, err := w.store.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("edited", body)
}

func TestDMFreshProviderSnapshotRepairsMissedEdit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	id, _, err := w.persist(t.Context(), dmTestRecord(t, w, "m_1", "original"))
	requirements.NoError(err)
	meta, _ := dmSavedMeta(t, w, id)
	meta.EditCount = 2
	blob, err := json.Marshal(meta)
	requirements.NoError(err)
	requirements.NoError(w.store.UpdateMessageDerivedText(id, sql.NullString{String: "webhook edit", Valid: true}, sql.NullString{}, sql.NullString{String: "webhook edit", Valid: true}, store.FTSDoc{Body: "webhook edit"}))
	requirements.NoError(w.store.SetMessageMetadataContext(t.Context(), id, sql.NullString{Valid: true, String: string(blob)}))

	rec, err := recordFromNode(w.src, DMMessage{ID: "m_1", From: DMParticipant{ID: "300", Name: "Example Customer"}, Message: "latest provider text", Raw: []byte(`{"id":"m_1","message":"latest provider text"}`)})
	requirements.NoError(err)
	_, _, err = w.persistProviderSnapshot(t.Context(), rec)
	requirements.NoError(err)

	body, err := w.store.GetMessageBodyText(id)
	requirements.NoError(err)
	assertions.Equal("latest provider text", body)
}

func TestDMPersistRefusesConversationMove(t *testing.T) {
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	r := dmTestRecord(t, w, "m_1", "original")
	_, _, err := w.persist(t.Context(), r)
	require.NoError(err)
	r.CustomerID = "301"
	_, _, err = w.persist(t.Context(), r)
	require.ErrorContains(err, "different conversation")
	child := dmTestRecord(t, w, "m_2", "child")
	child.CustomerID = "301"
	child.ReplyToMid = "m_1"
	_, _, err = w.persist(t.Context(), child)
	require.ErrorContains(err, "different conversation")
}

func TestDMReplyIgnoresUnrelatedFutureMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	w := dmTestWriter(t, PlatformMessenger)
	id, _, err := w.persist(t.Context(), dmTestRecord(t, w, "m_future", "future"))
	require.NoError(err)
	require.NoError(w.store.SetMessageMetadataContext(t.Context(), id, sql.NullString{String: `{"version":999,"referenced_message_id":"absent"}`, Valid: true}))
	child := dmTestRecord(t, w, "m_child", "child")
	child.ReplyToMid = "m_parent"
	_, _, err = w.persist(t.Context(), child)
	require.NoError(err)
	parent, _, err := w.persist(t.Context(), dmTestRecord(t, w, "m_parent", "parent"))
	require.NoError(err)
	assert.Equal(parent, message(t, w.store, w.sourceID, "m_child").ReplyToMessageID.Int64)
}
