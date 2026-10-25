package client

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestCalendarOccurrenceGeneratedEmptyStrings(t *testing.T) {
	start := time.Date(2026, time.October, 18, 14, 0, 0, 0, time.UTC)
	base := generated.CalendarOccurrence{
		Ref: "evt:1", MessageID: 1, SeriesMessageID: 1, SourceID: 2,
		Subject: "Meeting", Start: start, End: start.Add(time.Hour),
		StartDate: "2026-10-18", EndDate: "2026-10-19", Status: "confirmed",
		Attendees: []generated.Attendee{{Email: "guest@example.com", DisplayName: "Guest", Role: "required", Rsvp: "accepted"}},
	}
	for _, name := range []string{"timed event", "untitled event", "unnamed attendee", "attendee without email", "all empty"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			occurrence := base
			occurrence.Attendees = append([]generated.Attendee(nil), base.Attendees...)
			if name == "timed event" || name == "all empty" {
				occurrence.StartDate, occurrence.EndDate = "", ""
			}
			if name == "untitled event" || name == "all empty" {
				occurrence.Subject = ""
			}
			if name == "unnamed attendee" || name == "all empty" {
				occurrence.Attendees[0].DisplayName = ""
			}
			if name == "attendee without email" || name == "all empty" {
				occurrence.Attendees[0].Email = ""
			}
			require.NoError(occurrence.Validate())
			raw, err := json.Marshal(occurrence)
			require.NoError(err)
			var fields map[string]jsonv1.RawMessage
			require.NoError(jsonv1.Unmarshal(raw, &fields))
			for field, want := range map[string]string{"start_date": occurrence.StartDate, "end_date": occurrence.EndDate, "subject": occurrence.Subject} {
				require.Contains(fields, field)
				var got string
				require.NoError(jsonv1.Unmarshal(fields[field], &got))
				assert.Equal(want, got)
			}
			var attendees []map[string]jsonv1.RawMessage
			require.NoError(jsonv1.Unmarshal(fields["attendees"], &attendees))
			require.Len(attendees, 1)
			require.Contains(attendees[0], "display_name")
			var displayName string
			require.NoError(jsonv1.Unmarshal(attendees[0]["display_name"], &displayName))
			assert.Equal(occurrence.Attendees[0].DisplayName, displayName)
			require.Contains(attendees[0], "email")
			var email string
			require.NoError(jsonv1.Unmarshal(attendees[0]["email"], &email))
			assert.Equal(occurrence.Attendees[0].Email, email)
		})
	}
	for _, field := range []string{"Status", "Start", "End"} {
		t.Run("missing "+field, func(t *testing.T) {
			occurrence := base
			switch field {
			case "Status":
				occurrence.Status = ""
			case "Start":
				occurrence.Start = time.Time{}
			case "End":
				occurrence.End = time.Time{}
			}
			require.ErrorContains(t, occurrence.Validate(), field+" is required")
		})
	}
}
