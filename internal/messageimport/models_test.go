package messageimport

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestNormalizeCanonicalizesValues(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	normalized, err := validRequest(t).Normalize()
	require.NoError(t, err)

	assert.Equal(MessageImportSource{
		Identifier:   "feeds",
		DisplayName:  "Feeds",
		AccountEmail: "user@example.com",
	}, normalized.Source)
	message := normalized.Message
	assert.Equal("entry-42", message.ExternalID)
	assert.Equal("feed_item", message.MessageType)
	assert.Equal(MessageImportConversation{ExternalID: "feed-7", Title: "Example Project releases"}, message.Conversation)
	assert.Equal(MessageImportSender{Name: "Example Project", URL: "https://Example.com/blog"}, message.Sender)
	assert.Equal("Example Project 1.2.0", message.Title)
	assert.Equal("https://example.com/releases/1.2.0", message.URL)
	assert.Equal(time.Date(2026, time.November, 7, 15, 0, 0, 0, time.UTC), message.SentAt)
	assert.Equal(BodyFormatMarkdown, message.BodyFormat)
	assert.Equal("## Changes\n\n- Faster widgetsync for everyone.", message.Body)
}

func TestRequestNormalizeSenderAddresses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		sender MessageImportSender
		want   MessageImportSender
	}{
		{
			name:   "email is lowercased",
			sender: MessageImportSender{Email: " Author@Example.com "},
			want:   MessageImportSender{Email: "author@example.com"},
		},
		{
			name:   "formatted international phone",
			sender: MessageImportSender{Phone: "+1 (555) 010-0123"},
			want:   MessageImportSender{Phone: "+15550100123"},
		},
		{
			name:   "00 prefix and trunk zero",
			sender: MessageImportSender{Phone: "0044 (0)20 7946 0000"},
			want:   MessageImportSender{Phone: "+442079460000"},
		},
		{
			name:   "id alone",
			sender: MessageImportSender{ID: " author-7 "},
			want:   MessageImportSender{ID: "author-7"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := validRequest(t)
			req.Message.Sender = tc.sender
			normalized, err := req.Normalize()
			require.NoError(t, err)
			assert.Equal(t, tc.want, normalized.Message.Sender)
		})
	}
}

func TestRequestNormalizeBodies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		edit       func(*ImportedMessage)
		wantFormat BodyFormat
		wantBody   string
	}{
		{
			name:       "html",
			edit:       func(m *ImportedMessage) { m.BodyMarkdown, m.BodyHTML = "", " <p>Hi</p> " },
			wantFormat: BodyFormatHTML,
			wantBody:   "<p>Hi</p>",
		},
		{
			name:       "text",
			edit:       func(m *ImportedMessage) { m.BodyMarkdown, m.BodyText = "", "Hi" },
			wantFormat: BodyFormatText,
			wantBody:   "Hi",
		},
		{
			name:       "title only",
			edit:       func(m *ImportedMessage) { m.BodyMarkdown = "" },
			wantFormat: BodyFormatNone,
		},
		{
			name:       "blank second body is not a second body",
			edit:       func(m *ImportedMessage) { m.BodyText = "   " },
			wantFormat: BodyFormatMarkdown,
			wantBody:   "## Changes\n\n- Faster widgetsync for everyone.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := validRequest(t)
			tc.edit(&req.Message)
			normalized, err := req.Normalize()
			require.NoError(t, err)
			assert.Equal(t, tc.wantFormat, normalized.Message.BodyFormat)
			assert.Equal(t, tc.wantBody, normalized.Message.Body)
		})
	}
}

func TestRequestNormalizeValidates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(*Request)
		want string
	}{
		{"source identifier", func(r *Request) { r.Source.Identifier = " " }, "source.identifier is required"},
		{"source identifier length", func(r *Request) { r.Source.Identifier = strings.Repeat("é", 129) }, "source.identifier must be at most 128 characters"},
		{"account email", func(r *Request) { r.Source.AccountEmail = "Example <user@example.com>" }, "source.account_email must be one email address"},
		{"external id", func(r *Request) { r.Message.ExternalID = "" }, "message.external_id is required"},
		{"message type", func(r *Request) { r.Message.MessageType = "" }, "message.message_type is required"},
		{"message type case", func(r *Request) { r.Message.MessageType = "Feed_Item" }, "message.message_type must be lowercase letters"},
		{"message type dash", func(r *Request) { r.Message.MessageType = "feed-item" }, "message.message_type must be lowercase letters"},
		{"message type length", func(r *Request) { r.Message.MessageType = strings.Repeat("a", 65) }, "message.message_type must be at most 64 characters"},
		{"conversation", func(r *Request) { r.Message.Conversation.ExternalID = "" }, "message.conversation.external_id is required"},
		{"empty sender", func(r *Request) { r.Message.Sender = MessageImportSender{Name: " "} }, "message.sender requires a name, url, email, phone or id"},
		{"sender email", func(r *Request) { r.Message.Sender.Email = "not-an-address" }, "message.sender.email must be one email address"},
		{"national phone", func(r *Request) { r.Message.Sender.Phone = "555 010 0123" }, "message.sender.phone must be an international number"},
		{"sender id control", func(r *Request) { r.Message.Sender.ID = "a\nb" }, "message.sender.id must not contain control characters"},
		{"relative sender url", func(r *Request) { r.Message.Sender.URL = "/blog" }, "message.sender.url must be an absolute URL"},
		{"url with space", func(r *Request) { r.Message.URL = "https://example.com/a b" }, "message.url must not contain whitespace"},
		{"sent at", func(r *Request) { r.Message.SentAt = "" }, "message.sent_at is required"},
		{"sent at offset", func(r *Request) { r.Message.SentAt = "2026-11-07T10:00:00" }, "message.sent_at must be an RFC3339 timestamp"},
		{"two bodies", func(r *Request) { r.Message.BodyText = "also" }, "mutually exclusive"},
		{"nothing to show", func(r *Request) { r.Message.Title, r.Message.BodyMarkdown = "", "" }, "message requires a title or a body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := validRequest(t)
			tc.edit(&req)
			_, err := req.Normalize()
			require.ErrorIs(t, err, ErrValidation)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRequestNormalizeErrorsNeverEchoValues(t *testing.T) {
	t.Parallel()
	req := validRequest(t)
	req.Message.Sender.Phone = "555-secret-0123"
	_, err := req.Normalize()
	require.ErrorIs(t, err, ErrValidation)
	assert.NotContains(t, err.Error(), "secret")
}
