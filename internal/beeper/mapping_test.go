package beeper

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBodyText(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
		want string
	}{
		{"plain text", Message{Type: "TEXT", Text: "hello"}, "hello"},
		{"image placeholder", Message{Type: "IMAGE"}, "[image]"},
		{"video placeholder", Message{Type: "VIDEO"}, "[video]"},
		{"voice placeholder", Message{Type: "VOICE"}, "[voice message]"},
		{"sticker placeholder", Message{Type: "STICKER"}, "[sticker]"},
		{"location placeholder", Message{Type: "LOCATION"}, "[location]"},
		{"location with text keeps text", Message{Type: "LOCATION", Text: "123 Main St"}, "123 Main St"},
		{"file with name", Message{Type: "FILE", Attachments: []Attachment{{FileName: "report.pdf"}}}, "[file: report.pdf]"},
		{"file without name", Message{Type: "FILE"}, "[file]"},
		{"media with caption keeps caption", Message{Type: "IMAGE", Text: "look at this"}, "look at this"},
		{
			"voice transcription appended",
			Message{Type: "VOICE", Attachments: []Attachment{{IsVoiceNote: true, Transcription: &Transcription{Transcription: "call me back"}}}},
			"[voice message]\n🎤 transcript: call me back",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, bodyText(&tt.msg))
		})
	}
}

func TestMapMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ts := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	m := Message{
		ID:        "12345",
		Timestamp: ts,
		Type:      "TEXT",
		Text:      "hello world",
		IsSender:  true,
		Attachments: []Attachment{
			{FileName: "a.png"}, {FileName: "b.png"},
		},
	}
	msg, text := mapMessage(&m, 7, 3)
	assert.Equal(int64(7), msg.ConversationID)
	assert.Equal(int64(3), msg.SourceID)
	assert.Equal("12345", msg.SourceMessageID)
	assert.Equal("beeper", msg.MessageType)
	assert.True(msg.IsFromMe)
	require.True(msg.SentAt.Valid)
	assert.True(msg.SentAt.Time.Equal(ts))
	require.True(msg.ReceivedAt.Valid)
	assert.True(msg.HasAttachments)
	assert.Equal(2, msg.AttachmentCount)
	assert.Equal("hello world", text)
	assert.Equal("hello world", msg.Snippet.String)
	assert.False(msg.Subject.Valid, "chat messages have no subject")
}

func TestMapMessageSnippetTruncation(t *testing.T) {
	long := strings.Repeat("é", 150)
	msg, _ := mapMessage(&Message{ID: "1", Timestamp: time.Now(), Text: long}, 1, 1)
	assert.Len(t, []rune(msg.Snippet.String), 100)
}

func TestConversationType(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("direct_chat", conversationType("single"))
	assert.Equal("group_chat", conversationType("group"))
	assert.Equal("group_chat", conversationType("anything-else"))
}
