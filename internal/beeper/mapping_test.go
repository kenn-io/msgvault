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
		{"image placeholder", Message{Type: typeImage}, "[image]"},
		{"video placeholder", Message{Type: "VIDEO"}, "[video]"},
		{"voice placeholder", Message{Type: "VOICE"}, "[voice message]"},
		{"sticker placeholder", Message{Type: "STICKER"}, "[sticker]"},
		{"location placeholder", Message{Type: "LOCATION"}, "[location]"},
		{"location with text keeps text", Message{Type: "LOCATION", Text: "123 Main St"}, "123 Main St"},
		{"file with name", Message{Type: "FILE", Attachments: []Attachment{{FileName: "report.pdf"}}}, "[file: report.pdf]"},
		{"file without name", Message{Type: "FILE"}, "[file]"},
		{"media with caption keeps caption", Message{Type: typeImage, Text: "look at this"}, "look at this"},
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

// TestBodyTextHTML covers the Beeper API serving HTML in the `text` field for
// some messages and plain text for others, with no field distinguishing them.
// The inputs are shapes observed from a live account across bridges.
func TestBodyTextHTML(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			"matrix formatted message with mention link",
			`<p><a href="https://matrix.to/#/@bob:beeper.com" rel="noopener noreferrer" target="_blank">@bob</a>, good to know.</p>`,
			"@bob, good to know.",
		},
		{
			"matrix reply fallback is excluded",
			`<mx-reply><blockquote><a href="https://matrix.to/#/!room:example.com/$parent">In reply to</a> <a href="https://matrix.to/#/@alice:example.com">Alice</a><br>quoted parent text</blockquote></mx-reply><p>fresh reply</p>`,
			"fresh reply",
		},
		{
			"whatsapp line breaks",
			"Thanks :) <br><br>Either way we will be around.",
			"Thanks :)\n\nEither way we will be around.",
		},
		{
			"entity-only text is unescaped without tag stripping",
			"meet at 2:15&amp;k?",
			"meet at 2:15&k?",
		},
		{
			"numeric control entities cannot inject terminal escapes",
			"first line\nsecond line&#27;]52;c;evil&#7;safe&#155;tail",
			"first line\nsecond linesafe›tail",
		},
		{
			"telegram custom emoji image is dropped",
			`written in go too <img data-mx-emoticon="" src="mxc://local.beeper.com/abc">`,
			"written in go too",
		},
		{
			"instagram share collapses to the bare url",
			`<a href="https://www.instagram.com/p/ABC123/" rel="noopener noreferrer" target="_blank">https://www.instagram.com/p/ABC123/</a>`,
			"https://www.instagram.com/p/ABC123/",
		},
		// Plain messages must survive untouched: angle brackets and bare
		// ampersands are ordinary prose, not markup.
		{"bare ampersand is left alone", "Settings, Help & About", "Settings, Help & About"},
		{"comparison is not a tag", "confirmed a < b for all inputs", "confirmed a < b for all inputs"},
		{"emoticon is not a tag", "nice work <3", "nice work <3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, bodyText(&Message{Type: "TEXT", Text: tt.text}))
		})
	}
}

func TestMapMessageDropsMatrixReplyFallbackFromSnippet(t *testing.T) {
	formatted := `<mx-reply><blockquote>quoted parent text</blockquote></mx-reply><p>fresh reply</p>`
	msg, body := mapMessage(&Message{ID: "reply", Type: "TEXT", Text: formatted}, 1, 1)
	assert.Equal(t, "fresh reply", body)
	assert.Equal(t, "fresh reply", msg.Snippet.String)
}

// TestSharedLink covers telling a forwarded link preview apart from media the
// sender composed: the share's whole body is the URL it previews.
func TestSharedLink(t *testing.T) {
	att := []Attachment{{FileName: "image.jpg"}}
	tests := []struct {
		name string
		msg  Message
		want string
	}{
		{
			"instagram reel share",
			Message{Type: typeImage, Text: `<a href="https://www.instagram.com/p/ABC/">https://www.instagram.com/p/ABC/</a>`, Attachments: att},
			"https://www.instagram.com/p/ABC/",
		},
		{
			"plain url share",
			Message{Type: typeImage, Text: "https://github.com/example/repo", Attachments: att},
			"https://github.com/example/repo",
		},
		{"photo with no text is not a share", Message{Type: typeImage, Attachments: att}, ""},
		{
			"url with commentary is the sender's own message",
			Message{Type: typeImage, Text: "look at this https://www.instagram.com/p/ABC/", Attachments: att},
			"",
		},
		{"link without media is not a share", Message{Type: "TEXT", Text: "https://example.com"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sharedLink(&tt.msg))
		})
	}
}

func TestShareMetadata(t *testing.T) {
	assert := assert.New(t)
	att := []Attachment{{FileName: "image.jpg"}}
	assert.Empty(shareMetadata(&Message{Type: typeImage, Attachments: att}))
	assert.JSONEq(
		`{"shared_url":"https://www.instagram.com/p/ABC/"}`,
		shareMetadata(&Message{Type: typeImage, Text: "https://www.instagram.com/p/ABC/", Attachments: att}),
	)
	// A URL carrying a quote must not be able to break out of the JSON value.
	meta := shareMetadata(&Message{Type: typeImage, Text: `https://x.example/"+evil`, Attachments: att})
	assert.NotEmpty(meta)
	assert.JSONEq(`{"shared_url":"https://x.example/\"+evil"}`, meta)
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
