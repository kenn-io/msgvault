package daemonclient

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMCPGeneratedCalendarProjectionPreservesNulls(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	client := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/42", r.URL.Path)
		writeJSONResponse(t, w, map[string]any{"id": 42, "source_id": 9, "conversation_id": 7, "subject": "Synthetic cancellation", "from": "organizer@example.test", "to": []string{}, "sent_at": "2026-10-06T12:00:00Z", "snippet": "", "labels": []string{}, "body": "", "attachments": []any{}, "calendar": map[string]any{"status": "cancelled", "sequence": nil, "start": nil, "end": nil, "all_day": nil, "time_zone": nil, "ical_uid": nil}})
	})
	parser, err := client.GeneratedClient()
	require.NoError(err)
	response, err := parser.GetMessageWithResponse(t.Context(), &generated.GetMessageRequestOptions{PathParams: &generated.GetMessagePath{ID: 42}})
	require.NoError(err)
	require.NotNil(response.JSON200)
	require.NotNil(response.JSON200.Calendar)
	raw, err := json.Marshal(response.JSON200.Calendar)
	require.NoError(err)
	var parsed map[string]any
	require.NoError(json.Unmarshal(raw, &parsed))
	for _, field := range []string{"status", "sequence", "start", "end", "all_day", "time_zone", "ical_uid"} {
		assert.Contains(parsed, field, "required nullable fields must survive generated HTTP parsing")
	}
	assert.Equal("cancelled", parsed["status"])
	for _, field := range []string{"sequence", "start", "end", "all_day", "time_zone", "ical_uid"} {
		assert.Nil(parsed[field], "unknown %s must survive generated HTTP parsing", field)
	}
}

func TestMCPGetMessageDaemonCLIProjection(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		read func(context.Context, *Client) (*query.MessageDetail, error)
	}{
		{"CLI", "/api/v1/cli/message", func(ctx context.Context, client *Client) (*query.MessageDetail, error) {
			return client.GetCLIMessage(ctx, "42")
		}},
		{"event-bound", "/api/v1/mcp/events/messages/42", func(ctx context.Context, client *Client) (*query.MessageDetail, error) {
			return client.GetMCPEventMessage(ctx, "synthetic-event-token", 42)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := Require.New(t)
			assert := Assert.New(t)

			client := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(tc.path, r.URL.Path)
				writeJSONResponse(t, w, map[string]any{"id": 42, "source_id": 9, "conversation_id": 7, "source_message_id": "synthetic-cancelled-event", "message_type": "calendar_event", "subject": "Synthetic cancellation", "from": []any{}, "to": []any{}, "cc": []any{}, "bcc": []any{}, "sent_at": "2026-10-06T12:00:00Z", "snippet": "", "labels": []string{}, "body_text": "", "body_html": "", "attachments": []any{}, "is_from_me": true, "calendar": map[string]any{"status": "cancelled", "sequence": nil, "start": nil, "end": nil, "all_day": nil, "time_zone": nil, "ical_uid": nil}})
			})
			message, err := tc.read(t.Context(), client)
			require.NoError(err)
			require.NotNil(message)
			assert.Equal(int64(9), message.SourceID)
			assert.True(message.IsFromMe)
			require.NotNil(message.Calendar, "both CLI and event-bound readers must preserve the typed projection")
			require.NotNil(message.Calendar.Status)
			assert.Equal("cancelled", *message.Calendar.Status)
			assert.Nil(message.Calendar.Sequence)
			assert.Nil(message.Calendar.Start)
			assert.Nil(message.Calendar.AllDay)
		})
	}
}

func TestMCPGetMessageDaemonProjection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		calendar map[string]any
		sparse   bool
	}{
		{"scheduled", map[string]any{"status": "confirmed", "sequence": 0, "start": "2026-10-06T12:00:00Z", "end": "2026-10-06T13:00:00Z", "all_day": false, "time_zone": "Etc/UTC", "ical_uid": "synthetic@example.test"}, false},
		{"sparse cancellation", map[string]any{"status": "cancelled", "sequence": nil, "start": nil, "end": nil, "all_day": nil, "time_zone": nil, "ical_uid": nil}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := Require.New(t)
			assert := Assert.New(t)

			client := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("/api/v1/messages/42", r.URL.Path)
				writeJSONResponse(t, w, map[string]any{"id": 42, "source_id": 9, "conversation_id": 7, "source_message_id": "synthetic-event", "message_type": "calendar_event", "subject": "Synthetic meeting", "from": "organizer@example.test", "to": []string{}, "sent_at": "2026-10-06T12:00:00Z", "snippet": "Synthetic meeting", "labels": []string{}, "body": "Synthetic meeting body", "attachments": []any{}, "is_from_me": true, "calendar": tc.calendar})
			})
			message, err := NewEngineAdapter(client).GetMessage(t.Context(), 42)
			require.NoError(err)
			require.NotNil(message)
			assert.Equal(int64(9), message.SourceID)
			assert.True(message.IsFromMe)
			require.NotNil(message.Calendar, "generated HTTP parsing and daemon adapters must preserve typed calendar state")
			require.NotNil(message.Calendar.Status)
			assert.Equal(tc.calendar["status"], *message.Calendar.Status)
			if tc.sparse {
				assert.Nil(message.Calendar.Sequence)
				assert.Nil(message.Calendar.Start)
				assert.Nil(message.Calendar.End)
				assert.Nil(message.Calendar.AllDay)
				assert.Nil(message.Calendar.TimeZone)
				assert.Nil(message.Calendar.ICalUID)
			} else {
				require.NotNil(message.Calendar.Sequence)
				assert.Equal(int64(0), *message.Calendar.Sequence)
				require.NotNil(message.Calendar.AllDay)
				assert.False(*message.Calendar.AllDay)
				require.NotNil(message.Calendar.Start)
				assert.Equal("2026-10-06T12:00:00Z", *message.Calendar.Start)
				require.NotNil(message.Calendar.End)
				assert.Equal("2026-10-06T13:00:00Z", *message.Calendar.End)
				require.NotNil(message.Calendar.TimeZone)
				assert.Equal("Etc/UTC", *message.Calendar.TimeZone)
				require.NotNil(message.Calendar.ICalUID)
				assert.Equal("synthetic@example.test", *message.Calendar.ICalUID)
			}
		})
	}
}
