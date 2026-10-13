package calevent

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeGoogle(t *testing.T, raw, metadata string, sc SourceContext) Event {
	t.Helper()
	event, ok, err := Decode("gcal_json", []byte(raw), []byte(metadata), sc)
	require.NoError(t, err)
	require.True(t, ok)
	return event
}

func TestGoogleRolesRoomsAndCopySelf(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	event := decodeGoogle(t, `{"id":"event-example","organizer":{"email":" ORGANIZER@EXAMPLE.COM "},"attendees":[
  {"email":"organizer@example.com","organizer":true,"responseStatus":"accepted"},
  {"email":"optional@example.com","displayName":"Optional Person","optional":true,"responseStatus":"needsAction"},
  {"email":"resource@example.com","resource":true,"optional":true},
  {"email":"room@c.resource.calendar.google.com"},
  {"email":" COPY@EXAMPLE.COM ","self":true,"responseStatus":"declined"},
  {"email":"required@example.com","responseStatus":"tentative"}
 ]}`, `{}`, SourceContext{CalendarID: "calendar@example.com"})
	assertions.Equal("google", event.Provider)
	assertions.Equal("calendar@example.com", event.Namespace)
	assertions.Equal("organizer@example.com", event.OrganizerEmail)
	assertions.Equal("declined", event.CopySelfRSVP)
	requirements.Len(event.Attendees, 6)
	assertions.Equal([]Attendee{
		{Email: "organizer@example.com", Role: "organizer", RSVP: "accepted"},
		{Email: "optional@example.com", DisplayName: "Optional Person", Role: "optional", RSVP: "needs_action"},
		{Email: "resource@example.com", Role: "room", RSVP: "unknown"},
		{Email: "room@c.resource.calendar.google.com", Role: "room", RSVP: "unknown"},
		{Email: "copy@example.com", Role: "required", RSVP: "declined", IsCopySelf: true},
		{Email: "required@example.com", Role: "required", RSVP: "tentative"},
	}, event.Attendees)
}

func TestGoogleExceptionRawInstanceID(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	event := decodeGoogle(t, `{"id":"series-example_20270302T170000Z","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-02T09:00:00-08:00"},"start":{"dateTime":"2027-03-02T10:00:00-08:00","timeZone":"America/Los_Angeles"},"end":{"dateTime":"2027-03-02T11:00:00-08:00"},"iCalUID":"uid@example.com","sequence":3}`, `{"calendar_id":"copy@example.com"}`, SourceContext{})
	assertions.Equal("series-example_20270302T170000Z", event.ProviderEventID)
	assertions.Equal("series-example", event.SeriesProviderID)
	assertions.Equal("exception", event.Kind)
	requirements.NotNil(event.Original)
	assertions.False(event.Original.AllDay)
	assertions.Equal("20270302T170000Z", event.Original.Key())
	assertions.Equal("copy@example.com", event.Namespace)
	assertions.Equal(int64(3), event.Sequence)
	assertions.True(event.HasBounds)
	requirements.NotNil(event.Start)
	assertions.Equal("2027-03-02T18:00:00Z", event.Start.Format(time.RFC3339))
}

func TestAllDayExceptionOriginalDate(t *testing.T) {
	for _, zone := range []string{"America/Los_Angeles", "Asia/Tokyo"} {
		t.Run(zone, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			event := decodeGoogle(t, `{"id":"series-example_20270314","recurringEventId":"series-example","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-15"},"end":{"date":"2027-03-17"}}`, `{}`, SourceContext{CalendarTimeZone: zone})
			requirements.NotNil(event.Original)
			assertions.True(event.Original.AllDay)
			assertions.Equal("2027-03-14", event.Original.Date)
			assertions.Equal("20270314", event.Original.Key())
			assertions.True(event.AllDay)
			assertions.Equal("2027-03-15", event.StartDate)
			assertions.Equal("2027-03-17", event.EndDate)
			requirements.NotNil(event.Start)
			loc, err := time.LoadLocation(zone)
			requirements.NoError(err)
			assertions.Equal(time.Date(2027, 3, 15, 0, 0, 0, 0, loc).UTC(), *event.Start)
		})
	}
}

func TestGoogleSparseCancelledTombstone(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	event := decodeGoogle(t, `{"id":"series-example_20270302T170000Z","status":"cancelled","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-02T17:00:00Z"}}`, `{}`, SourceContext{})
	assertions.Equal("cancelled", event.Status)
	assertions.Equal("exception", event.Kind)
	assertions.False(event.HasBounds)
	assertions.Nil(event.Start)
	assertions.Nil(event.End)
	requirements.NotNil(event.Original)
	assertions.Equal("20270302T170000Z", event.Original.Key())
}

func TestGoogleJoinURLEntryPointType(t *testing.T) {
	assertions := assert.New(t)

	raw := `{"id":"event-example","conferenceData":{"entryPoints":[{"entryPointType":"phone","uri":"tel:+15550100"},{"type":"video","uri":"https://example.com/wrong"},{"entryPointType":"video","uri":"https://tenant.zoom.us/j/123456789?pwd=secret"},{"entryPointType":"video","uri":"https://meet.google.com/abc-defg-hij"}]}}`
	event := decodeGoogle(t, raw, `{}`, SourceContext{})
	assertions.Equal("https://tenant.zoom.us/j/123456789?pwd=secret", event.JoinURL)
	assertions.Equal("zoom:123456789", event.JoinKey)
	event = decodeGoogle(t, `{"id":"event-example","hangoutLink":"https://meet.google.com/ABC-defg-HIJ?authuser=1","conferenceData":{"entryPoints":[{"entryPointType":"video","uri":"https://example.com/other"}]}}`, `{}`, SourceContext{})
	assertions.Equal("https://meet.google.com/ABC-defg-HIJ?authuser=1", event.JoinURL)
	assertions.Equal("meet:abc-defg-hij", event.JoinKey)
}

func TestGoogleCalendarZoneFromSourceContext(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	event := decodeGoogle(t, `{"id":"event-example","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`, `{"time_zone":"Asia/Tokyo"}`, SourceContext{CalendarID: "calendar@example.com", CalendarTimeZone: "America/Los_Angeles"})
	assertions.Equal("America/Los_Angeles", event.CalendarTimeZone)
	assertions.Empty(event.EventTimeZone)
	assertions.Equal("calendar@example.com", event.CalendarID)
	requirements.NotNil(event.Start)
	requirements.NotNil(event.End)
	assertions.Equal("2027-03-14T08:00:00Z", event.Start.Format(time.RFC3339))
	assertions.Equal("2027-03-15T07:00:00Z", event.End.Format(time.RFC3339))
}

func TestGoogleMetadataCancellationOverridesRaw(t *testing.T) {
	assertions := assert.New(t)

	event := decodeGoogle(t, `{"id":"event-example","status":"confirmed","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"}}`, `{"status":"cancelled"}`, SourceContext{})
	assertions.Equal("cancelled", event.Status)
	assertions.True(event.HasBounds)
}

func TestGoogleSeriesAndSingle(t *testing.T) {
	assertions := assert.New(t)

	event := decodeGoogle(t, `{"id":"series-example","recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"],"transparency":"opaque","visibility":"private","eventType":"default","location":"Example Room"}`, `{}`, SourceContext{})
	assertions.Equal("series", event.Kind)
	assertions.Equal("series-example", event.SeriesProviderID)
	assertions.Equal([]string{"RRULE:FREQ=WEEKLY;COUNT=6"}, event.Recurrence)
	assertions.Equal("opaque", event.Transparency)
	assertions.Equal("private", event.Visibility)
	assertions.Equal("default", event.EventType)
	assertions.Equal("Example Room", event.Location)
	event = decodeGoogle(t, `{"id":"single-example"}`, `{}`, SourceContext{})
	assertions.Equal("single", event.Kind)
	assertions.Empty(event.SeriesProviderID)
}

func TestDecodeUnknownAndInvalidRaw(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	_, ok, err := Decode("unknown", []byte(`{}`), nil, SourceContext{})
	requirements.NoError(err)
	assertions.False(ok)
	_, ok, err = Decode("gcal_json", []byte(`{`), nil, SourceContext{})
	require.Error(t, err)
	assertions.False(ok)
}

func TestDecodeRegistry(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	format := "synthetic_" + t.Name()
	Register(format, func(raw, metadata []byte, sc SourceContext) (Event, bool, error) {
		return Event{Provider: "example", ProviderEventID: string(raw), Status: string(metadata), CalendarID: sc.CalendarID}, true, nil
	})
	got, ok, err := Decode(format, []byte("example-id"), []byte("confirmed"), SourceContext{CalendarID: "calendar@example.com"})
	requirements.NoError(err)
	requirements.True(ok)
	assertions.Equal("example", got.Provider)
	assertions.Equal("example-id", got.ProviderEventID)
	assertions.Equal("confirmed", got.Status)
	assertions.Equal("calendar@example.com", got.CalendarID)
}

func TestGoogleInvalidMetadataKeepsRaw(t *testing.T) {
	assertions := assert.New(t)

	event := decodeGoogle(t, `{"id":"event-example","status":"confirmed"}`, `{`, SourceContext{CalendarID: "calendar@example.com"})
	assertions.Equal("confirmed", event.Status)
	assertions.Equal("calendar@example.com", event.Namespace)
}

func TestGoogleExceptionIdentityFallback(t *testing.T) {
	assertions := assert.New(t)

	for _, tt := range []struct {
		id, want string
		allDay   bool
	}{
		{"series-example_20270302T170000Z", "20270302T170000Z", false},
		{"series-example_20270314", "20270314", true},
	} {
		t.Run(tt.id, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			event := decodeGoogle(t, `{"id":"`+tt.id+`","recurringEventId":"series-example","status":"cancelled"}`, `{}`, SourceContext{})
			requirements.NotNil(event.Original)
			assertions.Equal(tt.want, event.Original.Key())
			assertions.Equal(tt.allDay, event.AllDay)
			assertions.False(event.HasBounds)
		})
	}
	event := decodeGoogle(t, `{"id":"different-example_20270314","recurringEventId":"series-example"}`, `{}`, SourceContext{})
	assertions.Nil(event.Original)
}

func TestDecodeRawLimit(t *testing.T) {
	assertions := assert.New(t)

	raw := append([]byte(`{"id":"event-example"}`), bytes.Repeat([]byte(" "), 64<<20)...)
	_, ok, err := Decode("gcal_json", raw, nil, SourceContext{})
	require.Error(t, err)
	assertions.False(ok)
}

func TestGoogleDescriptionCapped(t *testing.T) {
	for _, tt := range []struct {
		description, want string
		truncated         bool
	}{
		{"Example description", "Example description", false},
		{strings.Repeat("a", 4096), strings.Repeat("a", 4096), false},
		{strings.Repeat("a", 5000), strings.Repeat("a", 4096), true},
		{strings.Repeat("a", 4094) + "🌲end", strings.Repeat("a", 4094), true},
	} {
		t.Run(fmt.Sprintf("bytes_%d", len(tt.description)), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			raw, err := json.Marshal(map[string]string{"id": "event-example", "description": tt.description})
			requirements.NoError(err)
			event := decodeGoogle(t, string(raw), `{}`, SourceContext{})
			assertions.Equal(tt.want, event.Description)
			assertions.Equal(tt.truncated, event.DescriptionTruncated)
			assertions.True(utf8.ValidString(event.Description))
			assertions.LessOrEqual(len(event.Description), 4096)
		})
	}
}
