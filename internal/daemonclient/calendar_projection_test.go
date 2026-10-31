package daemonclient

import (
	"context"
	"net/http"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/query"
)

// Remote mode parses daemon responses through the generated client. The
// calendar projection, including null fields, must survive that trip for both
// the message-detail route and the CLI message route.
func TestRemoteMessageReadsKeepCalendarProjection(t *testing.T) {
	calendar := map[string]any{"status": "cancelled", "sequence": nil, "start": nil, "end": nil, "all_day": nil, "time_zone": nil, "ical_uid": nil}
	for _, tc := range []struct {
		name string
		path string
		body map[string]any
		read func(context.Context, *Client) (*query.MessageDetail, error)
	}{
		{"message detail", "/api/v1/messages/42",
			map[string]any{"id": 42, "source_id": 9, "conversation_id": 7, "source_message_id": "synthetic-event", "message_type": "calendar_event", "subject": "Synthetic meeting", "from": "organizer@example.test", "to": []string{}, "sent_at": "2026-10-06T12:00:00Z", "snippet": "", "labels": []string{}, "body": "", "attachments": []any{}, "calendar": calendar},
			func(ctx context.Context, c *Client) (*query.MessageDetail, error) {
				return NewEngineAdapter(c).GetMessage(ctx, 42)
			}},
		{"CLI message", "/api/v1/cli/message",
			map[string]any{"id": 42, "source_id": 9, "conversation_id": 7, "source_message_id": "synthetic-event", "source_conversation_id": "", "message_type": "calendar_event", "subject": "Synthetic meeting", "from": []any{}, "to": []any{}, "cc": []any{}, "bcc": []any{}, "sent_at": "2026-10-06T12:00:00Z", "snippet": "", "labels": []string{}, "body_text": "", "body_html": "", "attachments": []any{}, "size_estimate": 0, "has_attachments": false, "received_at": nil, "deleted_at": nil, "calendar": calendar},
			func(ctx context.Context, c *Client) (*query.MessageDetail, error) { return c.GetCLIMessage(ctx, "42") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			client := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(tc.path, r.URL.Path)
				writeJSONResponse(t, w, tc.body)
			})
			message, err := tc.read(t.Context(), client)
			require.NoError(err)
			require.NotNil(message)
			require.NotNil(message.Calendar)
			require.NotNil(message.Calendar.Status)
			assert.Equal("cancelled", *message.Calendar.Status)
			assert.Nil(message.Calendar.Sequence)
			assert.Nil(message.Calendar.Start)
			assert.Nil(message.Calendar.AllDay)
		})
	}
}
