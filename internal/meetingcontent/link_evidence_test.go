package meetingcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDecodeMeetingLinkEvidence(t *testing.T) {
	assert := assert.New(t)
	granola := Decode("granola_json", []byte(`{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"event_20270314T100000Z","scheduled_start_time":"2027-03-14T10:00:00Z"}}`), nil)
	assert.Equal("event_20270314T100000Z", granola.LinkEvidence.GoogleEventID)
	assert.Equal("owner@example.com", granola.LinkEvidence.CalendarOwner)

	twenty := Decode("twenty_json", []byte(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T10:00:00Z"}}`), nil)
	assert.Equal("weekly@example.org", twenty.LinkEvidence.ICalUID)
	assert.Equal(mustTime(t, "2027-03-14T10:00:00Z"), twenty.LinkEvidence.ScheduledAt)
}
