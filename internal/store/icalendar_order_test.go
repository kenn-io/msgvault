package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestICalendarCancelledDetailsRevisionOrder(t *testing.T) {
	initial := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Initial\r\nORGANIZER:mailto:organizer@example.com")
	updated := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTART:20270302T140000Z\r\nSUMMARY:Updated\r\nORGANIZER:mailto:organizer@example.com")
	cancel := calendarDocument("CANCEL", "SEQUENCE:3\r\nSTATUS:CANCELLED\r\nORGANIZER:mailto:organizer@example.com")
	for _, mode := range []string{"file", "email"} {
		for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
			if mode == "email" && order != [3]int{0, 2, 1} && order != [3]int{2, 1, 0} {
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
				_, err = st.db.ExecContext(t.Context(), `DELETE FROM calendar_events WHERE message_id=?`, ids[0])
				requirements.NoError(err)
				requirements.NoError(st.UpsertMessageRawWithFormat(ids[0], stored, "ics"))
				assertions.Equal(row.Event, calendarRows(t, st, ids[0])[0].Event)
				restored := calendarDocument("REQUEST", "SEQUENCE:4\r\nDTSTART:20270302T150000Z\r\nSUMMARY:Restored\r\nORGANIZER:mailto:organizer@example.com")
				if mode == "email" {
					persistInviteEmailForSource(t, st, source, "restored", inviteEmail(restored))
				} else {
					_, err = st.PersistICalendarContext(t.Context(), source, restored)
					requirements.NoError(err)
				}
				row = calendarRows(t, st, ids[0])[0]
				assertions.Equal("confirmed", row.Event.Status)
				assertions.Equal("Restored", row.Subject)
			})
		}
	}
}

func TestICalendarCancelledDetailsStamp(t *testing.T) {
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
