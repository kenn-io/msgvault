package store

import (
	"fmt"
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
		for _, order := range [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
			t.Run(fmt.Sprintf("%s-%d%d%d", mode, order[0], order[1], order[2]), func(t *testing.T) {
				st := newRFC822IDBackfillBackendStore(t)
				source := icsSource(t, st)
				if mode == "email" {
					account, err := st.GetOrCreateSource("mbox", "owner@example.com")
					require.NoError(t, err)
					source = account.ID
				}
				items := [][]byte{initial, updated, cancel}
				for _, i := range order {
					if mode == "email" {
						persistInviteEmailForSource(t, st, source, fmt.Sprintf("mail-%d", i), inviteEmail(items[i]))
					} else {
						_, err := st.PersistICalendarContext(t.Context(), source, items[i])
						require.NoError(t, err)
					}
				}
				ids := emailEvents(t, st, source)
				require.Len(t, ids, 1)
				row := calendarRows(t, st, ids[0])[0]
				assert.Equal(t, "cancelled", row.Event.Status)
				assert.Equal(t, "Updated", row.Subject)
				require.NotNil(t, row.Event.Start)
				assert.Equal(t, "2027-03-02T14:00:00Z", row.Event.Start.Format(time.RFC3339))
				stored, err := st.GetMessageRaw(ids[0])
				require.NoError(t, err)
				assert.Equal(t, updated, stored)
				_, err = st.db.Exec(`DELETE FROM calendar_events WHERE message_id=?`, ids[0])
				require.NoError(t, err)
				require.NoError(t, st.UpsertMessageRawWithFormat(ids[0], stored, "ics"))
				assert.Equal(t, row.Event, calendarRows(t, st, ids[0])[0].Event)
				if order[0] == 0 && order[1] == 2 {
					if mode == "email" {
						foreign := calendarDocument("REQUEST", "SEQUENCE:4\r\nDTSTART:20270302T160000Z\r\nORGANIZER:mailto:other@example.com")
						persistInviteEmailForSource(t, st, source, "foreign", inviteEmail(foreign))
						assert.Equal(t, row.Event, calendarRows(t, st, ids[0])[0].Event)
					}
					restored := calendarDocument("REQUEST", "SEQUENCE:4\r\nDTSTART:20270302T150000Z\r\nSUMMARY:Restored\r\nORGANIZER:mailto:organizer@example.com")
					if mode == "email" {
						persistInviteEmailForSource(t, st, source, "restored", inviteEmail(restored))
					} else {
						_, err = st.PersistICalendarContext(t.Context(), source, restored)
						require.NoError(t, err)
					}
					row = calendarRows(t, st, ids[0])[0]
					assert.Equal(t, "confirmed", row.Event.Status)
					assert.Equal(t, "Restored", row.Subject)
				}
			})
		}
	}
}

func TestICalendarCancelledDetailsStamp(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	for _, fields := range []string{
		"SEQUENCE:2\r\nDTSTAMP:20270301T100000Z\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Initial",
		"SEQUENCE:3\r\nSTATUS:CANCELLED",
		"SEQUENCE:2\r\nDTSTAMP:20270301T110000Z\r\nDTSTART:20270302T140000Z\r\nSUMMARY:Latest",
		"SEQUENCE:2\r\nDTSTAMP:20270301T090000Z\r\nDTSTART:20270302T100000Z\r\nSUMMARY:Older",
	} {
		_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("", fields))
		require.NoError(t, err)
	}
	ids := emailEvents(t, st, source)
	require.Len(t, ids, 1)
	row := calendarRows(t, st, ids[0])[0]
	assert.Equal(t, "Latest", row.Subject)
	assert.Equal(t, "cancelled", row.Event.Status)
}
