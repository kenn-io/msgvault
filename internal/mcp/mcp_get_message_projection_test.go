package mcp

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPGetMessageEventReadProjection(t *testing.T) {
	for _, tc := range []struct {
		name, messageType, metadata string
		wantCalendar                map[string]any
	}{
		{"email", "email", "", nil},
		{"timed calendar", "calendar_event", `{"status":"confirmed","sequence":3,"start":"2026-10-06T12:00:00Z","end":"2026-10-06T13:00:00Z","all_day":false,"time_zone":"Etc/UTC","ical_uid":"event@example.test"}`, map[string]any{"status": "confirmed", "sequence": json.Number("3"), "start": "2026-10-06T12:00:00Z", "end": "2026-10-06T13:00:00Z", "all_day": false, "time_zone": "Etc/UTC", "ical_uid": "event@example.test"}},
		{"all day", "calendar_event", `{"status":"confirmed","sequence":0,"start":"2026-10-06","end":"2026-10-07","all_day":true}`, map[string]any{"status": "confirmed", "sequence": json.Number("0"), "start": "2026-10-06", "end": "2026-10-07", "all_day": true, "time_zone": nil, "ical_uid": nil}},
		{"sparse cancellation", "calendar_event", `{"status":"cancelled","all_day":false}`, map[string]any{"status": "cancelled", "sequence": nil, "start": nil, "end": nil, "all_day": nil, "time_zone": nil, "ical_uid": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := Require.New(t)
			assert := Assert.New(t)

			f := storetest.New(t)
			if tc.messageType == "calendar_event" {
				source, err := f.Store.GetOrCreateSource("gcal", "calendar@example.test/primary")
				require.NoError(err)
				conv, err := f.Store.EnsureConversationWithType(source.ID, "event:projection", "calendar", "Synthetic projected event")
				require.NoError(err)
				f.Source, f.ConvID = source, conv
			}
			message := f.NewMessage().WithIsFromMe(true).Build()
			message.MessageType = tc.messageType
			metadata := sql.NullString{String: tc.metadata, Valid: tc.metadata != ""}
			id, err := f.Store.PersistMessage(&store.MessagePersistData{Message: message, Metadata: &metadata, BodyText: sql.NullString{String: "Synthetic archive body", Valid: true}})
			require.NoError(err)
			h := newTestHandlers(query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL()))
			raw := runTool[json.RawMessage](t, "get_message", h.getMessage, map[string]any{"id": float64(id)})
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			var response map[string]any
			require.NoError(decoder.Decode(&response))
			assert.Equal(json.Number(strconv.FormatInt(f.Source.ID, 10)), response["source_id"])
			assert.Equal(true, response["is_from_me"])
			if tc.wantCalendar == nil {
				assert.NotContains(response, "calendar")
				return
			}
			assert.Equal(tc.wantCalendar, response["calendar"])
		})
	}
}
