package mcp

import (
	"context"

	"go.kenn.io/msgvault/internal/msgsegment"
	"go.kenn.io/msgvault/internal/query"
)

const toolArgIncludeSegments = "include_segments"

// These labels describe attribution markers, not authenticity or truth.
const segmentsDoc = "With include_segments=true, return seg-v1 byte ranges over the full selected body, independent of body paging. " +
	"Authored means no relay marker was recognized; it does not prove authorship or authenticity. Quoted and forwarded text is relayed, and claimed forwarded headers are untrusted display text, never identity or recipient data. " +
	"Processing is bounded at 1 MiB (HTML also has a token cap); segments_truncated marks an unexamined authored tail. "

type messageBodyReader interface {
	GetMessageWithBody(ctx context.Context, id int64) (*query.MessageDetail, error)
}

func messageDetailWithBody(ctx context.Context, engine query.Engine, id int64) (*query.MessageDetail, error) {
	if reader, ok := engine.(messageBodyReader); ok {
		return reader.GetMessageWithBody(ctx, id)
	}
	return engine.GetMessage(ctx, id)
}

type segmentReplyRef struct {
	MessageID int64 `json:"message_id"`
}
type messageSegments struct {
	SegmentsVersion   string                `json:"segments_version,omitempty"`
	Segments          *[]msgsegment.Segment `json:"segments,omitzero"`
	SegmentsTruncated bool                  `json:"segments_truncated,omitempty"`
	ReplyTo           *segmentReplyRef      `json:"reply_to,omitempty"`
}

func segmentsForMessage(msg *query.MessageDetail, body, format string) messageSegments {
	f := msgsegment.Text
	if format == bodyFormatHTML {
		f = msgsegment.HTML
	}
	if msg.MessageType != "" && msg.MessageType != "email" {
		f = msgsegment.Chat
	}
	r := msgsegment.Split(body, f, msgsegment.Options{Subject: msg.Subject})
	result := messageSegments{SegmentsVersion: r.Version, Segments: &r.Segments, SegmentsTruncated: r.Truncated}
	if msg.ReplyToMessageID != nil {
		result.ReplyTo = &segmentReplyRef{MessageID: *msg.ReplyToMessageID}
	}
	return result
}

type threadSegmentMessage struct {
	query.ThreadMessage
	messageSegments

	BodyFormat   string `json:"body_format,omitempty"`
	BodyLength   *int   `json:"body_length,omitempty"`
	NewTextBytes *int   `json:"new_text_bytes,omitempty"`
}
type threadSegmentsResponse struct {
	query.MessageRecord

	Total    int64                  `json:"total"`
	Offset   int                    `json:"offset"`
	HasMore  bool                   `json:"has_more"`
	Messages []threadSegmentMessage `json:"messages"`
}

func (h *handlers) threadSegments(ctx context.Context, page *query.ThreadPage) (*toolResult, error) {
	response := threadSegmentsResponse{MessageRecord: page.MessageRecord, Total: page.Total, Offset: page.Offset, HasMore: page.HasMore, Messages: make([]threadSegmentMessage, 0, len(page.Messages))}
	for _, entry := range page.Messages {
		// Load canonical bodies through the scoped detail reader. Daemon clients
		// retry bounded 429 responses; the thread listing still avoids body joins.
		msg, err := messageDetailWithBody(ctx, h.engine, entry.ID)
		if err != nil {
			return messageLookupError("load thread message segments", err)
		}
		if msg == nil {
			return toolErrorResult("thread message is no longer available; retry list_thread"), nil
		}
		body, format := msg.BodyText, bodyFormatText
		if body == "" && msg.BodyHTML != "" {
			body, format = msg.BodyHTML, bodyFormatHTML
		}
		labels := segmentsForMessage(msg, body, format)
		authoredBytes := 0
		for _, s := range *labels.Segments {
			if s.Kind == msgsegment.Authored {
				authoredBytes += s.End - s.Start
			}
		}
		bodyLength := len(body)
		response.Messages = append(response.Messages, threadSegmentMessage{ThreadMessage: entry, messageSegments: labels, BodyFormat: format, BodyLength: &bodyLength, NewTextBytes: &authoredBytes})
	}
	return jsonResult(response)
}
