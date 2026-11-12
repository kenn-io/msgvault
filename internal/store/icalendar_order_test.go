package store

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

func TestICalendarRevisionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"sequence order", testICalendarCancelledDetailsRevisionOrder},
		{"cancelled details stamp", testICalendarCancelledDetailsStamp},
		{"equal and newer revisions", testICalendarEqualRevisionIgnored},
		{"cancellation tie", testICalendarCancellationWinsRevisionTie},
	} {
		t.Run(tc.name, tc.run)
	}
}

func testICalendarCancelledDetailsRevisionOrder(t *testing.T) {
	initial := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Initial\r\nORGANIZER:mailto:organizer@example.com")
	updated := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTART:20270302T140000Z\r\nSUMMARY:Updated\r\nORGANIZER:mailto:organizer@example.com")
	cancel := calendarDocument("CANCEL", "SEQUENCE:3\r\nSTATUS:CANCELLED\r\nORGANIZER:mailto:organizer@example.com")
	for _, mode := range []string{"file", "email"} {
		for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
			if mode == "email" && order != [3]int{2, 1, 0} {
				continue
			}
			t.Run(fmt.Sprintf("%s-%d%d%d", mode, order[0], order[1], order[2]), func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				source := icsSource(t, st)
				if mode == "email" {
					account, err := st.GetOrCreateSource("mbox", "owner@example.com")
					requirements.NoError(err)
					source = account.ID
				}
				items := [][]byte{initial, updated, cancel}
				for _, i := range order {
					if mode == "email" {
						persistInviteEmailForSource(t, st, source, fmt.Sprintf("mail-%d", i), inviteEmail(items[i]))
					} else {
						_, err := st.PersistICalendarContext(t.Context(), source, items[i])
						requirements.NoError(err)
					}
				}
				ids := emailEvents(t, st, source)
				requirements.Len(ids, 1)
				row := calendarRows(t, st, ids[0])[0]
				assertions.Equal("cancelled", row.Event.Status)
				assertions.Equal("Updated", row.Subject)
				requirements.NotNil(row.Event.Start)
				assertions.Equal("2027-03-02T14:00:00Z", row.Event.Start.Format(time.RFC3339))
				stored, err := st.GetMessageRaw(ids[0])
				requirements.NoError(err)
				assertions.Equal(updated, stored)
				if mode == "file" && order == [3]int{0, 1, 2} {
					t.Run("projection rebuild and restoration", func(t *testing.T) {
						_, err := st.db.ExecContext(t.Context(), `DELETE FROM calendar_events WHERE message_id=?`, ids[0])
						require.NoError(t, err)
						require.NoError(t, st.UpsertMessageRawWithFormat(ids[0], stored, "ics"))
						assert.Equal(t, row.Event, calendarRows(t, st, ids[0])[0].Event)
						restored := calendarDocument("REQUEST", "SEQUENCE:4\r\nDTSTART:20270302T150000Z\r\nSUMMARY:Restored\r\nORGANIZER:mailto:organizer@example.com")
						_, err = st.PersistICalendarContext(t.Context(), source, restored)
						require.NoError(t, err)
						row := calendarRows(t, st, ids[0])[0]
						assert.Equal(t, "confirmed", row.Event.Status)
						assert.Equal(t, "Restored", row.Subject)
					})
				}
			})
		}
	}
}

func TestICalendarCancellationRetainsAttendees(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	first := calendarDocument("CANCEL", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDTEND:20270302T130000Z\r\nSUMMARY:Cancelled meeting\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:guest@example.org")
	later := calendarDocument("CANCEL", "SEQUENCE:2\r\nDTSTART:20270302T120000Z\r\nDTEND:20270302T130000Z\r\nSUMMARY:Cancelled meeting")
	for _, raw := range [][]byte{first, later} {
		_, err := st.PersistICalendarContext(t.Context(), source, raw)
		requirements.NoError(err)
	}
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	row := calendarRows(t, st, ids[0])[0]
	requirements.Equal("Cancelled meeting", row.Subject)
	requirements.Equal("cancelled", row.Event.Status)
	requirements.NotNil(row.Event.Start)
	requirements.NotNil(row.Event.End)
	requirements.Equal("2027-03-02T12:00:00Z", row.Event.Start.Format(time.RFC3339))
	requirements.Equal("2027-03-02T13:00:00Z", row.Event.End.Format(time.RFC3339))
	requirements.Equal([]calevent.Attendee{{Email: "guest@example.org", Role: "required", RSVP: "accepted"}}, row.Event.Attendees)
	var recipient string
	requirements.NoError(st.db.QueryRowContext(t.Context(), `SELECT email_address FROM message_recipients WHERE message_id=? AND recipient_type='to'`, ids[0]).Scan(&recipient))
	requirements.Equal("guest@example.org", recipient)
	stored, err := st.GetMessageRaw(ids[0])
	requirements.NoError(err)
	requirements.Equal(first, stored)
	metadata, err := st.GetMessageMetadata(ids[0])
	requirements.NoError(err)
	var revision calevent.ICSMetadata
	requirements.NoError(json.Unmarshal([]byte(metadata.String), &revision))
	requirements.Equal(int64(2), revision.Sequence)
	requirements.Equal("cancelled", revision.Status)
}

func testICalendarCancelledDetailsStamp(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	for _, fields := range []string{
		"SEQUENCE:2\r\nDTSTAMP:20270301T100000Z\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Initial",
		"SEQUENCE:3\r\nSTATUS:CANCELLED",
		"SEQUENCE:2\r\nDTSTAMP:20270301T110000Z\r\nDTSTART:20270302T140000Z\r\nSUMMARY:Latest",
		"SEQUENCE:2\r\nDTSTAMP:20270301T090000Z\r\nDTSTART:20270302T100000Z\r\nSUMMARY:Older",
	} {
		raw := strings.Replace(string(calendarDocument("", fields)), "METHOD:\r\n", "", 1)
		_, err := st.PersistICalendarContext(t.Context(), source, []byte(raw))
		requirements.NoError(err)
	}
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	row := calendarRows(t, st, ids[0])[0]
	assertions.Equal("Latest", row.Subject)
	assertions.Equal("cancelled", row.Event.Status)
}
