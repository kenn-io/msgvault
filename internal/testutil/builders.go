package testutil

import (
	"time"

	"go.kenn.io/msgvault/internal/query"
)

// MessageSummaryBuilder provides a fluent API for constructing query.MessageSummary in tests.
type MessageSummaryBuilder struct {
	s query.MessageSummary
}

// NewMessageSummary creates a builder with sensible defaults.
func NewMessageSummary(id int64) *MessageSummaryBuilder {
	return &MessageSummaryBuilder{
		s: query.MessageSummary{
			ID:        id,
			Subject:   "Test Subject",
			FromEmail: "sender@example.com",
			FromName:  "Sender",
			SentAt:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
}

func (b *MessageSummaryBuilder) WithSubject(s string) *MessageSummaryBuilder {
	b.s.Subject = s
	return b
}

func (b *MessageSummaryBuilder) WithFromEmail(e string) *MessageSummaryBuilder {
	b.s.FromEmail = e
	return b
}

func (b *MessageSummaryBuilder) WithFromName(n string) *MessageSummaryBuilder {
	b.s.FromName = n
	return b
}

func (b *MessageSummaryBuilder) WithSentAt(t time.Time) *MessageSummaryBuilder {
	b.s.SentAt = t
	return b
}

func (b *MessageSummaryBuilder) WithSize(sz int64) *MessageSummaryBuilder {
	b.s.SizeEstimate = sz
	return b
}

func (b *MessageSummaryBuilder) WithSourceMessageID(id string) *MessageSummaryBuilder {
	b.s.SourceMessageID = id
	return b
}

func (b *MessageSummaryBuilder) WithLabels(labels ...string) *MessageSummaryBuilder {
	b.s.Labels = labels
	return b
}

// WithAttachmentCount sets the attachment count and HasAttachments flag.
// Named differently from MessageDetailBuilder.WithAttachments to clarify
// that this takes a count, not actual attachment structs.
func (b *MessageSummaryBuilder) WithAttachmentCount(count int) *MessageSummaryBuilder {
	b.s.HasAttachments = count > 0
	b.s.AttachmentCount = count
	return b
}

func (b *MessageSummaryBuilder) WithSnippet(s string) *MessageSummaryBuilder {
	b.s.Snippet = s
	return b
}

func (b *MessageSummaryBuilder) WithConversationID(id int64) *MessageSummaryBuilder {
	b.s.ConversationID = id
	return b
}

func (b *MessageSummaryBuilder) WithSourceConversationID(id string) *MessageSummaryBuilder {
	b.s.SourceConversationID = id
	return b
}

func (b *MessageSummaryBuilder) WithDeletedAt(t *time.Time) *MessageSummaryBuilder {
	b.s.DeletedAt = t
	return b
}

// WithDeleted is a convenience method that sets DeletedAt from a time.Time value,
// handling pointer conversion internally.
func (b *MessageSummaryBuilder) WithDeleted(t time.Time) *MessageSummaryBuilder {
	b.s.DeletedAt = &t
	return b
}

func (b *MessageSummaryBuilder) Build() query.MessageSummary {
	return b.s
}

// BuildPtr returns a pointer to the constructed MessageSummary.
func (b *MessageSummaryBuilder) BuildPtr() *query.MessageSummary {
	s := b.s
	return &s
}

// MessageDetailBuilder provides a fluent API for constructing query.MessageDetail in tests.
type MessageDetailBuilder struct {
	d query.MessageDetail
}

// NewMessageDetail creates a builder with sensible defaults.
func NewMessageDetail(id int64) *MessageDetailBuilder {
	return &MessageDetailBuilder{
		d: query.MessageDetail{
			ID:       id,
			Subject:  "Test Subject",
			From:     []query.Address{{Email: "sender@example.com", Name: "Sender"}},
			To:       []query.Address{{Email: "recipient@example.com"}},
			SentAt:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			BodyText: "Test body",
		},
	}
}

func (b *MessageDetailBuilder) WithSubject(s string) *MessageDetailBuilder {
	b.d.Subject = s
	return b
}

func (b *MessageDetailBuilder) WithFrom(addrs ...query.Address) *MessageDetailBuilder {
	b.d.From = addrs
	return b
}

// WithFromAddress is a convenience method for setting a single sender
// without needing to construct a query.Address struct.
func (b *MessageDetailBuilder) WithFromAddress(email, name string) *MessageDetailBuilder {
	b.d.From = []query.Address{{Email: email, Name: name}}
	return b
}

func (b *MessageDetailBuilder) WithTo(addrs ...query.Address) *MessageDetailBuilder {
	b.d.To = addrs
	return b
}

func (b *MessageDetailBuilder) WithSentAt(t time.Time) *MessageDetailBuilder {
	b.d.SentAt = t
	return b
}

func (b *MessageDetailBuilder) WithBodyText(s string) *MessageDetailBuilder {
	b.d.BodyText = s
	return b
}

func (b *MessageDetailBuilder) WithBodyHTML(s string) *MessageDetailBuilder {
	b.d.BodyHTML = s
	return b
}

func (b *MessageDetailBuilder) WithLabels(labels ...string) *MessageDetailBuilder {
	b.d.Labels = labels
	return b
}

func (b *MessageDetailBuilder) WithAttachments(atts ...query.AttachmentInfo) *MessageDetailBuilder {
	b.d.Attachments = atts
	b.d.HasAttachments = len(atts) > 0
	return b
}

func (b *MessageDetailBuilder) WithSize(sz int64) *MessageDetailBuilder {
	b.d.SizeEstimate = sz
	return b
}

func (b *MessageDetailBuilder) WithSourceMessageID(id string) *MessageDetailBuilder {
	b.d.SourceMessageID = id
	return b
}

func (b *MessageDetailBuilder) WithConversationID(id int64) *MessageDetailBuilder {
	b.d.ConversationID = id
	return b
}

func (b *MessageDetailBuilder) WithSourceConversationID(id string) *MessageDetailBuilder {
	b.d.SourceConversationID = id
	return b
}

func (b *MessageDetailBuilder) WithSnippet(s string) *MessageDetailBuilder {
	b.d.Snippet = s
	return b
}

func (b *MessageDetailBuilder) WithCc(addrs ...query.Address) *MessageDetailBuilder {
	b.d.Cc = addrs
	return b
}

func (b *MessageDetailBuilder) WithBcc(addrs ...query.Address) *MessageDetailBuilder {
	b.d.Bcc = addrs
	return b
}

func (b *MessageDetailBuilder) Build() query.MessageDetail {
	return b.d
}

func (b *MessageDetailBuilder) BuildPtr() *query.MessageDetail {
	d := b.d
	return &d
}
