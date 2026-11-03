package calevent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeGoogle(t *testing.T, raw, metadata string, zone string) Event {
	t.Helper()
	event, ok, err := Decode("gcal_json", []byte(raw), []byte(metadata), zone)
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
 ]}`, `{}`, "")
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

	event := decodeGoogle(t, `{"id":"series-example_20270302T170000Z","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-02T09:00:00-08:00"},"start":{"dateTime":"2027-03-02T10:00:00-08:00","timeZone":"America/Los_Angeles"},"end":{"dateTime":"2027-03-02T11:00:00-08:00"},"iCalUID":"uid@example.com","sequence":3}`, `{"calendar_id":"copy@example.com"}`, "")
	assertions.Equal("series-example_20270302T170000Z", event.ProviderEventID)
	assertions.Equal("series-example", event.SeriesProviderID)
	assertions.Equal("exception", event.Kind)
	requirements.NotNil(event.Original)
	assertions.False(event.Original.AllDay)
	assertions.Equal("20270302T170000Z", event.Original.Key())
	requirements.NotNil(event.Start)
	assertions.Equal("2027-03-02T18:00:00Z", event.Start.Format(time.RFC3339))
}

func TestAllDayExceptionOriginalDate(t *testing.T) {
	for _, zone := range []string{"America/Los_Angeles", "Asia/Tokyo"} {
		t.Run(zone, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			event := decodeGoogle(t, `{"id":"series-example_20270314","recurringEventId":"series-example","originalStartTime":{"date":"2027-03-14"},"start":{"date":"2027-03-15"},"end":{"date":"2027-03-17"}}`, `{}`, zone)
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

	event := decodeGoogle(t, `{"id":"series-example_20270302T170000Z","status":"cancelled","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-02T17:00:00Z"}}`, `{}`, "")
	assertions.Equal("cancelled", event.Status)
	assertions.Equal("exception", event.Kind)
	assertions.Nil(event.Start)
	assertions.Nil(event.End)
	requirements.NotNil(event.Original)
	assertions.Equal("20270302T170000Z", event.Original.Key())
}

func TestGoogleCalendarZoneFromSourceContext(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	event := decodeGoogle(t, `{"id":"event-example","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`, `{"time_zone":"Asia/Tokyo"}`, "America/Los_Angeles")
	assertions.Equal("America/Los_Angeles", event.CalendarTimeZone)
	assertions.Empty(event.EventTimeZone)
	requirements.NotNil(event.Start)
	requirements.NotNil(event.End)
	assertions.Equal("2027-03-14T08:00:00Z", event.Start.Format(time.RFC3339))
	assertions.Equal("2027-03-15T07:00:00Z", event.End.Format(time.RFC3339))
}

func TestGoogleMetadataCancellationOverridesRaw(t *testing.T) {
	assertions := assert.New(t)

	event := decodeGoogle(t, `{"id":"event-example","status":"confirmed","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"}}`, `{"status":"cancelled"}`, "")
	assertions.Equal("cancelled", event.Status)
}

func TestGoogleSeriesAndSingle(t *testing.T) {
	assertions := assert.New(t)

	event := decodeGoogle(t, `{"id":"series-example","recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"],"transparency":"opaque","visibility":"private","eventType":"default","location":"Example Room"}`, `{}`, "")
	assertions.Equal("series", event.Kind)
	assertions.Equal("series-example", event.SeriesProviderID)
	assertions.Equal([]string{"RRULE:FREQ=WEEKLY;COUNT=6"}, event.Recurrence)
	event = decodeGoogle(t, `{"id":"single-example"}`, `{}`, "")
	assertions.Equal("single", event.Kind)
	assertions.Empty(event.SeriesProviderID)
}

func TestDecodeUnknownAndInvalidRaw(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	_, ok, err := Decode("unknown", []byte(`{}`), nil, "")
	requirements.NoError(err)
	assertions.False(ok)
	_, ok, err = Decode("gcal_json", []byte(`{`), nil, "")
	require.Error(t, err)
	assertions.False(ok)
}

func TestGoogleInvalidMetadataKeepsRaw(t *testing.T) {
	assertions := assert.New(t)

	event := decodeGoogle(t, `{"id":"event-example","status":"confirmed"}`, `{`, "")
	assertions.Equal("confirmed", event.Status)
}

func TestGoogleSeparateOrganizer(t *testing.T) {
	event := decodeGoogle(t, `{"id":"event-example","organizer":{"email":" ORGANIZER@EXAMPLE.COM ","displayName":"Organizer","self":true},"attendees":[{"email":"guest@example.com","responseStatus":"accepted"}]}`, `{}`, "")
	assert.Equal(t, []Attendee{{Email: "guest@example.com", Role: "required", RSVP: "accepted"}, {Email: "organizer@example.com", DisplayName: "Organizer", Role: "organizer", RSVP: "unknown", IsCopySelf: true}}, event.Attendees)
}
