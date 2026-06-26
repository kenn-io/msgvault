package teams

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTMLToText(t *testing.T) {
	got := htmlToText(`<p>Hello <at id="0">Bob</at> see <a href="x">link</a></p>`)
	assert.Contains(t, got, "Hello")
	assert.Contains(t, got, "Bob")
	assert.NotContains(t, got, "<p>")
}

func TestMapMessageBasics(t *testing.T) {
	assert := assert.New(t)
	gm := &ChatMessage{
		ID:                   "m1",
		CreatedDateTime:      time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
		LastModifiedDateTime: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
		Body:                 MessageBody{ContentType: "html", Content: "<p>hi there</p>"},
		Attachments:          []Attachment{{ContentType: "reference", ContentURL: "http://sp/f", Name: "f.docx"}},
	}
	msg, text := mapMessage(gm, 10, 20, chatSourceMessageID("chatA", gm.ID))
	assert.Equal("teams", msg.MessageType)
	assert.Equal("chat:chatA:m1", msg.SourceMessageID)
	assert.True(msg.SentAt.Valid)
	assert.True(msg.HasAttachments)
	assert.Equal(1, msg.AttachmentCount)
	assert.Equal("hi there", text)
	assert.Contains(msg.Snippet.String, "hi there")
}

func TestCallRecordingParsing(t *testing.T) {
	assert := assert.New(t)

	gm := &ChatMessage{
		EventDetail: json.RawMessage([]byte(`{"@odata.type":"#microsoft.graph.callRecordingEventMessageDetail","callRecordingUrl":"https://sp/rec.mp4","callRecordingDisplayName":"Dev guild"}`)),
	}
	url, name, ok := gm.callRecording()
	assert.True(ok)
	assert.Equal("https://sp/rec.mp4", url)
	assert.Equal("Dev guild", name)

	// Negative: a membersAdded-style event has no callRecordingUrl.
	gmNoRec := &ChatMessage{
		EventDetail: json.RawMessage([]byte(`{"@odata.type":"#microsoft.graph.membersAddedEventMessageDetail","members":["aad-bob"]}`)),
	}
	_, _, ok = gmNoRec.callRecording()
	assert.False(ok)

	// Negative: empty EventDetail.
	gmEmpty := &ChatMessage{}
	_, _, ok = gmEmpty.callRecording()
	assert.False(ok)
}

func TestMapMessageRecording(t *testing.T) {
	assert := assert.New(t)

	gm := &ChatMessage{
		ID:                   "sys1",
		CreatedDateTime:      time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
		LastModifiedDateTime: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
		Body:                 MessageBody{ContentType: "html", Content: "<systemEventMessage/>"},
		EventDetail:          json.RawMessage([]byte(`{"@odata.type":"#microsoft.graph.callRecordingEventMessageDetail","callRecordingUrl":"https://sp/rec.mp4","callRecordingDisplayName":"Dev guild"}`)),
	}
	msg, text := mapMessage(gm, 10, 20, chatSourceMessageID("chatA", gm.ID))
	assert.Contains(text, "📹 recording")
	assert.Contains(text, "https://sp/rec.mp4")
	assert.True(msg.HasAttachments)
	assert.Equal(1, msg.AttachmentCount)
	assert.Contains(msg.Snippet.String, "📹 recording")
	assert.Contains(msg.Snippet.String, "https://sp/rec.mp4")
}

func TestMapMessageHostedContentCountsInlineImages(t *testing.T) {
	assert := assert.New(t)

	gm := &ChatMessage{
		ID: "inline1",
		Body: MessageBody{
			ContentType: "html",
			Content: `<p><img src="https://graph.microsoft.com/v1.0/chats/c/messages/inline1/hostedContents/1/$value">` +
				`<img src="https://graph.microsoft.com/v1.0/chats/c/messages/inline1/hostedContents/2/$value"></p>`,
		},
	}

	msg, _ := mapMessage(gm, 10, 20, chatSourceMessageID("chatA", gm.ID))
	assert.True(msg.HasAttachments)
	assert.Equal(2, msg.AttachmentCount)
}

func TestTeamsSourceMessageIDNamespacesConversations(t *testing.T) {
	assert.Equal(t, "chat:chatA:m1", chatSourceMessageID("chatA", "m1"))
	assert.Equal(t, "chat:chatB:m1", chatSourceMessageID("chatB", "m1"))
	assert.Equal(t, "channel:team1:chanA:m1", channelSourceMessageID("team1", "chanA", "m1"))
}

// TestChatMessageRawPreservesUnknownFields verifies that decoding a ChatMessage
// retains the full original JSON (including fields we do not model, e.g. webUrl),
// so the archived raw blob is truly lossless.
func TestChatMessageRawPreservesUnknownFields(t *testing.T) {
	assert := assert.New(t)
	src := `{"id":"m1","webUrl":"https://teams/msg/m1","summary":"s","body":{"contentType":"text","content":"hi"}}`
	var gm ChatMessage
	require.NoError(t, json.Unmarshal([]byte(src), &gm))
	assert.Equal("m1", gm.ID)
	assert.Equal("hi", gm.Body.Content)
	assert.Contains(string(gm.Raw), "webUrl")
	assert.Contains(string(gm.Raw), "https://teams/msg/m1")
}

func TestConversationType(t *testing.T) {
	assert.Equal(t, "direct_chat", conversationType("oneOnOne"))
	assert.Equal(t, "group_chat", conversationType("group"))
	assert.Equal(t, "group_chat", conversationType("meeting"))
}
