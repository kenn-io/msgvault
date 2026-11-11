package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
)

func replyTestInvite(sequence int, stamp, partstat, extra string) []byte {
	return calendarDocument("REQUEST", fmt.Sprintf("SEQUENCE:%d\r\nDTSTAMP:%s\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Meeting\r\nORGANIZER:mailto:organizer@example.com\r\nATTENDEE;PARTSTAT=%s:mailto:guest@example.org\r\nATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:other@example.org\r\n%s", sequence, stamp, partstat, extra))
}

func replyTestReply(stamp, status, attendee, organizer, extra string) []byte {
	return calendarDocument("REPLY", fmt.Sprintf("SEQUENCE:%d\r\nDTSTAMP:%s\r\nORGANIZER:mailto:%s\r\nATTENDEE;PARTSTAT=%s:mailto:%s\r\n%s", 1, stamp, organizer, status, attendee, extra))
}

func TestEmailCalendarReplyOrderAndRebuild(t *testing.T) {
	request := replyTestInvite(1, "20270301T100000Z", "NEEDS-ACTION", "")
	accepted := replyTestReply("20270301T110000Z", "ACCEPTED", "guest@example.org", "organizer@example.com", "")
	declined := replyTestReply("20270301T120000Z", "DECLINED", "guest@example.org", "organizer@example.com", "")
	other := replyTestReply("20270301T110000Z", "TENTATIVE", "other@example.org", "organizer@example.com", "")
	updated := replyTestInvite(1, "20270301T130000Z", "NEEDS-ACTION", "")
	informative := replyTestInvite(1, "20270301T130000Z", "TENTATIVE", "")
	newSequence := replyTestInvite(2, "20270301T130000Z", "NEEDS-ACTION", "")
	foreign := replyTestReply("20270301T110000Z", "ACCEPTED", "guest@example.org", "foreign@example.com", "")
	absent := replyTestReply("20270301T110000Z", "ACCEPTED", "missing@example.org", "organizer@example.com", "")
	tied := replyTestReply("20270301T110000Z", "DECLINED", "guest@example.org", "organizer@example.com", "")
	cancel := calendarDocument("CANCEL", "SEQUENCE:2\r\nDTSTAMP:20270301T140000Z\r\nORGANIZER:mailto:organizer@example.com")
	for _, tc := range []struct {
		name         string
		docs         [][]byte
		guest, other string
	}{
		{"reply first", [][]byte{accepted, request}, "accepted", "needs_action"},
		{"newest first", [][]byte{request, declined, accepted}, "declined", "needs_action"},
		{"duplicate and two attendees", [][]byte{request, accepted, accepted, other}, "accepted", "tentative"},
		{"same sequence snapshot", [][]byte{request, accepted, updated}, "accepted", "needs_action"},
		{"newer informative snapshot", [][]byte{request, accepted, informative}, "tentative", "needs_action"},
		{"new sequence", [][]byte{request, accepted, newSequence}, "needs_action", "needs_action"},
		{"foreign organizer", [][]byte{request, foreign}, "needs_action", "needs_action"},
		{"absent attendee", [][]byte{request, absent}, "needs_action", "needs_action"},
		{"equal timestamps", [][]byte{request, tied, accepted}, "declined", "needs_action"},
		{"equal timestamps reversed", [][]byte{request, accepted, tied}, "declined", "needs_action"},
		{"cancelled", [][]byte{request, accepted, cancel}, "accepted", "needs_action"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source, err := st.GetOrCreateSource("mbox", "owner@example.com")
			requirements.NoError(err)
			for i, doc := range tc.docs {
				persistInviteEmailForSource(t, st, source.ID, fmt.Sprintf("reply-%d", i), inviteEmail(doc))
				if tc.name == "reply first" && i == 0 {
					from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
					page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
					requirements.NoError(err)
					assertions.Empty(page.Occurrences)
					assertions.Empty(page.Unexpandable)
				}
			}
			ids := emailEvents(t, st, source.ID)
			requirements.Len(ids, 1)
			check := func() {
				from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
				page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
				requirements.NoError(err)
				requirements.Len(page.Occurrences, 1)
				got := map[string]string{}
				for _, attendee := range page.Occurrences[0].Attendees {
					got[attendee.Email] = attendee.RSVP
				}
				assertions.Equal(tc.guest, got["guest@example.org"])
				assertions.Equal(tc.other, got["other@example.org"])
			}
			check()
			stored, err := st.GetMessageRaw(ids[0])
			requirements.NoError(err)
			_, err = st.db.Exec(`DELETE FROM calendar_events WHERE message_id=?`, ids[0])
			requirements.NoError(err)
			requirements.NoError(st.UpsertMessageRawWithFormat(ids[0], stored, "ics"))
			check()
		})
	}
}

func TestEmailCalendarReplyArchivedReplayAndSourceIsolation(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	request := inviteEmail(replyTestInvite(1, "20270301T100000Z", "NEEDS-ACTION", ""))
	reply := inviteEmail(replyTestReply("20270301T110000Z", "ACCEPTED", "guest@example.org", "organizer@example.com", ""))
	requestID, source := persistInviteEmail(t, st, "replay-request", request)
	replyID := persistInviteEmailForSource(t, st, source, "replay-reply", reply)
	other, err := st.GetOrCreateSource("mbox", "other@example.com")
	requirements.NoError(err)
	persistInviteEmailForSource(t, st, other.ID, "other-request", request)
	events := emailEvents(t, st, source)
	requirements.Len(events, 1)
	_, err = st.db.Exec(`UPDATE conversations SET metadata=NULL WHERE id=(SELECT conversation_id FROM messages WHERE id=?)`, events[0])
	requirements.NoError(err)
	_, err = st.db.Exec(`DELETE FROM calendar_events WHERE message_id=?`, events[0])
	requirements.NoError(err)
	stored, err := st.GetMessageRaw(events[0])
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(events[0], stored, "ics"))
	for _, id := range []int64{replyID, requestID} {
		item, err := st.ScanArchivedEmailContext(t.Context(), source, id-1, id)
		requirements.NoError(err)
		requirements.NotNil(item)
		parsed, err := mime.Parse(item.RawData)
		requirements.NoError(err)
		changed, err := st.RefreshArchivedEmailCalendarContext(t.Context(), source, item, parsed)
		requirements.NoError(err)
		if id == replyID {
			assertions.True(changed)
		}
	}
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	for _, event := range page.Occurrences {
		for _, attendee := range event.Attendees {
			if attendee.Email == "guest@example.org" {
				if event.SourceID == source {
					assertions.Equal("accepted", attendee.RSVP)
				} else {
					assertions.Equal("needs_action", attendee.RSVP)
				}
			}
		}
	}
}

func TestEmailCalendarInstanceReply(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("mbox", "owner@example.com")
	requirements.NoError(err)
	request := replyTestInvite(1, "20270301T100000Z", "NEEDS-ACTION", "RRULE:FREQ=DAILY;COUNT=2")
	global := replyTestReply("20270301T110000Z", "ACCEPTED", "guest@example.org", "organizer@example.com", "")
	instance := replyTestReply("20270301T120000Z", "DECLINED", "guest@example.org", "organizer@example.com", "RECURRENCE-ID:20270303T120000Z")
	for i, doc := range [][]byte{instance, request, global} {
		persistInviteEmailForSource(t, st, source.ID, fmt.Sprintf("instance-%d", i), inviteEmail(doc))
	}
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	for i, want := range []string{"accepted", "declined"} {
		got := map[string]string{}
		for _, a := range page.Occurrences[i].Attendees {
			got[a.Email] = a.RSVP
		}
		assertions.Equal(want, got["guest@example.org"])
		assertions.Equal("needs_action", got["other@example.org"])
	}
}
