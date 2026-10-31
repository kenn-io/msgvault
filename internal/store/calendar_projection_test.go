package store_test

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func calendarProjectionFixture(t *testing.T, messageType, metadata string) (*storetest.Fixture, int64) {
	t.Helper()
	f := storetest.New(t)
	if messageType == "calendar_event" {
		source, err := f.Store.GetOrCreateSource("gcal", "calendar@example.test/primary")
		Require.NoError(t, err)
		conv, err := f.Store.EnsureConversationWithType(source.ID, "event:projection", "calendar", "Synthetic event")
		Require.NoError(t, err)
		f.Source, f.ConvID = source, conv
	}
	message := f.NewMessage().Build()
	message.MessageType = messageType
	stored := sql.NullString{String: metadata, Valid: metadata != ""}
	id, err := f.Store.PersistMessage(&store.MessagePersistData{Message: message, Metadata: &stored, BodyText: sql.NullString{String: "Synthetic body", Valid: true}})
	Require.NoError(t, err)
	return f, id
}

func TestGetMessageReturnsCalendarProjection(t *testing.T) {
	text := func(s string) *string { return &s }
	flag := func(b bool) *bool { return &b }
	number := func(n int64) *int64 { return &n }
	for _, tc := range []struct {
		name, messageType, metadata string
		want                        *store.CalendarProjection
	}{
		{"email has none", "email", "", nil},
		{"timed event", "calendar_event", `{"status":"confirmed","sequence":3,"start":"2026-10-06T12:00:00Z","end":"2026-10-06T13:00:00Z","all_day":false,"time_zone":"Etc/UTC","ical_uid":"event@example.test","html_link":"https://calendar.example.test/e"}`,
			&store.CalendarProjection{Status: text("confirmed"), Sequence: number(3), Start: text("2026-10-06T12:00:00Z"), End: text("2026-10-06T13:00:00Z"), AllDay: flag(false), TimeZone: text("Etc/UTC"), ICalUID: text("event@example.test")}},
		{"all-day event", "calendar_event", `{"status":"confirmed","start":"2026-10-06","end":"2026-10-07","all_day":true}`,
			&store.CalendarProjection{Status: text("confirmed"), Start: text("2026-10-06"), End: text("2026-10-07"), AllDay: flag(true)}},
		// A cancellation seen before any details carries no start, so all_day
		// is unknown rather than false.
		{"sparse cancellation", "calendar_event", `{"status":"cancelled","all_day":false}`, &store.CalendarProjection{Status: text("cancelled")}},
		// PostgreSQL stores metadata as JSON, so the unreadable case uses a
		// wrong field type rather than invalid JSON.
		{"unreadable metadata", "calendar_event", `{"status":5}`, &store.CalendarProjection{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, id := calendarProjectionFixture(t, tc.messageType, tc.metadata)
			msg, err := f.Store.GetMessageContext(t.Context(), id)
			Require.NoError(t, err)
			Require.NotNil(t, msg)
			Assert.Equal(t, tc.want, msg.Calendar)
		})
	}
}
