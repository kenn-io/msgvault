package api

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestCalendarEventsGeneratedResponses(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		master    bool
		list      bool
	}{
		{"timed", `{"id":"fixture","start":{"dateTime":"2027-03-08T10:00:00Z"},"end":{"dateTime":"2027-03-08T11:00:00Z"},"attendees":[{"email":"invited@example.com"},{"displayName":"Guest"}]}`, false, true},
		{"all-day", `{"id":"fixture","status":"confirmed","start":{"date":"2027-03-08"},"end":{"date":"2027-03-09"}}`, false, true},
		{"sparse recurring", `{"id":"fixture","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-08T10:00:00Z"}}`, true, true},
		{"sparse standalone", `{"id":"fixture","status":"cancelled"}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("gcal", "calendar@example.com")
			requirements.NoError(err)
			requirements.NoError(st.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"primary","time_zone":"UTC"}`))
			put := func(providerID, raw string) int64 {
				conversation, err := st.EnsureConversation(src.ID, providerID, "Calendar fixture")
				requirements.NoError(err)
				id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conversation,
					SourceMessageID: providerID, MessageType: "calendar_event", Subject: sql.NullString{String: "", Valid: true}})
				requirements.NoError(err)
				requirements.NoError(st.UpsertMessageRawWithFormat(id, []byte(raw), "gcal_json"))
				return id
			}
			if tc.master {
				put("series", `{"id":"series","status":"confirmed","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
			}
			id := put("fixture", tc.raw)
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Store: st, Logger: testLogger()})
			t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
			get := func(path string) []byte {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set("X-Api-Key", "owner")
				w := httptest.NewRecorder()
				srv.Router().ServeHTTP(w, req)
				requirements.Equal(http.StatusOK, w.Code, w.Body.String())
				return w.Body.Bytes()
			}
			raw := get(fmt.Sprintf("/api/v1/calendar/events/evt:%d", id))
			var occurrence generated.CalendarOccurrence
			requirements.NoError(json.Unmarshal(raw, &occurrence))
			requirements.NoError(occurrence.Validate(), string(raw))
			encoded, err := json.Marshal(occurrence)
			requirements.NoError(err)
			assertions.JSONEq(string(raw), string(encoded), "required empty and zero properties survive re-encoding")
			paths := [][]string{{"attendees"}, {"subject"}, {"status"}, {"start"}, {"end"}, {"original", "Date"}, {"original", "Instant"}}
			if len(occurrence.Attendees) > 0 {
				paths = append(paths, []string{"attendees", "Email"}, []string{"attendees", "DisplayName"})
			}
			if tc.name != "timed" {
				paths = nil // Presence validation is shared across these response types.
			}
			for _, path := range paths {
				for _, explicitNull := range []bool{false, true} {
					var body map[string]any
					requirements.NoError(json.Unmarshal(raw, &body))
					parent := body
					if len(path) > 1 {
						nested := body[path[0]]
						if path[0] == "attendees" {
							attendees, ok := nested.([]any)
							requirements.True(ok)
							requirements.NotEmpty(attendees)
							nested = attendees[0]
						}
						var ok bool
						parent, ok = nested.(map[string]any)
						requirements.True(ok)
					}
					field := path[len(path)-1]
					if explicitNull {
						parent[field] = nil
					} else {
						delete(parent, field)
					}
					invalidJSON, err := json.Marshal(body)
					requirements.NoError(err)
					var invalid generated.CalendarOccurrence
					requirements.NoError(json.Unmarshal(invalidJSON, &invalid))
					requirements.Error(invalid.Validate(), "required %v, null=%t", path, explicitNull)
				}
			}
			if tc.list {
				var page generated.CalendarOccurrencePage
				pageJSON := get("/api/v1/calendar/events?from=2027-03-08T00:00:00Z&to=2027-03-09T00:00:00Z&include_cancelled=true")
				requirements.NoError(json.Unmarshal(pageJSON, &page))
				requirements.NotEmpty(page.Items)
				requirements.NoError(page.Validate())
				if tc.name == "timed" {
					for _, field := range []string{"items", "unexpandable"} {
						for _, explicitNull := range []bool{false, true} {
							var body map[string]any
							requirements.NoError(json.Unmarshal(pageJSON, &body))
							if explicitNull {
								body[field] = nil
							} else {
								delete(body, field)
							}
							invalidJSON, err := json.Marshal(body)
							requirements.NoError(err)
							var invalid generated.CalendarOccurrencePage
							requirements.NoError(json.Unmarshal(invalidJSON, &invalid))
							requirements.Error(invalid.Validate(), "required %s, null=%t", field, explicitNull)
						}
					}
					var empty generated.CalendarOccurrencePage
					requirements.NoError(json.Unmarshal(get("/api/v1/calendar/events?from=2028-03-08T00:00:00Z&to=2028-03-09T00:00:00Z"), &empty))
					assertions.Empty(empty.Items)
					assertions.Empty(empty.Unexpandable)
					requirements.NoError(empty.Validate(), "required collections allow present empty arrays")
				}
			}
		})
	}
}
