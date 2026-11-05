package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingLinksUnlinkRemovedTargetWithSurvivingICSLink(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, removed := calendarFixture(t, st, "removed", singleLinkRaw("removed", "removed@example.org"), "removed@example.com")
	source := icsSource(t, st)
	_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H"))
	requirements.NoError(err)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	survivor := ids[0]
	_, note := projectionFixture(t, st, "note", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: removed}))
	requirements.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: survivor}))
	requirements.NoError(st.RemoveSource(data.Message.SourceID))
	requirements.Equal(fmt.Sprintf("evt:%d", survivor), linkedMeetingRefs(t, st, note)[note])
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: removed}))
	var decision string
	requirements.NoError(st.db.QueryRow(`SELECT decision FROM meeting_link_decisions WHERE member_message_id=? AND target_ical_uid=?`, note, "removed@example.org").Scan(&decision))
	assertions.Equal("split", decision)
	assertions.Equal(fmt.Sprintf("evt:%d", survivor), linkedMeetingRefs(t, st, note)[note])
}
