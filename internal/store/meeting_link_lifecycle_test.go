package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecisionsSubsetCopyEffectiveLinks(t *testing.T) {
	requirements := require.New(t)

	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(path)
	requirements.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	requirements.NoError(st.InitSchema())
	target, _ := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("event", "event@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	_, note := projectionFixture(t, st, "note", "granola_json", `{"calendar_event":{"calendar_event_id":"event"}}`)
	_, err = st.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), target)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, time.Date(2027, 3, 14, 11, 0, 0, 0, time.UTC), note)
	requirements.NoError(err)
	requirements.NoError(st.UnlinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", target), note))
	for _, tc := range []struct {
		name          string
		count         int
		kept, dropped int64
	}{{"both", 2, 1, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			dir := t.TempDir()
			result, err := CopySubset(path, dir, tc.count, false)
			requirements.NoError(err)
			assertions.Equal(tc.kept, result.MeetingLinkDecisions)
			assertions.Equal(tc.dropped, result.OmittedMeetingLinkDecisions)
			dst, err := Open(filepath.Join(dir, "msgvault.db"))
			requirements.NoError(err)
			defer func() { _ = dst.Close() }()
			var count int64
			requirements.NoError(dst.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions`).Scan(&count))
			assertions.Equal(tc.kept, count)
			if tc.kept == 1 {
				assertions.Empty(memberLinks(t, dst, OwnerScope(), note))
				assertions.NotEmpty(linkKeyValues(t, dst, note))
			}
		})
	}
}

func TestDecisionTargetConsolidationKeepsOccurrenceSuffix(t *testing.T) {
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	legacy, source := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	raw := strings.Replace(linkSeriesRaw, `"id":"series"`, `"id":"canonical"`, 1)
	survivor, _ := calendarLinkFixture(t, st, "primary@example.com", raw)
	_, note := projectionFixture(t, st, "manual", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d@20270314T100000Z", legacy), note))
	var conv int64
	requirements.NoError(st.db.QueryRow(`SELECT COALESCE(conversation_id,0) FROM messages WHERE id = ?`, legacy).Scan(&conv))
	requirements.NoError(st.MigrateSourceMessageID(source, conv, "series", "canonical"))
	assert.ElementsMatch(t, []OccurrenceRef{{MessageID: survivor, Key: "20270314T100000Z"}}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
}

func TestDecisionsSubsetOmitsMissingOccurrence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(path)
	requirements.NoError(err)
	defer func() { _ = st.Close() }()
	requirements.NoError(st.InitSchema())
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	_, note := projectionFixture(t, st, "manual", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d@20270314T100000Z", master), note))
	_, err = st.db.Exec(`UPDATE meeting_link_decisions SET target_ref = ? WHERE member_message_id = ?`, fmt.Sprintf("evt:%d@20270401T100000Z", master), note)
	requirements.NoError(err)
	result, err := CopySubset(path, t.TempDir(), 2, false)
	requirements.NoError(err)
	assertions.Zero(result.MeetingLinkDecisions)
	assertions.Equal(int64(1), result.OmittedMeetingLinkDecisions)
}

func TestDecisionsSubsetOmitsBareEventAfterItBecomesSeries(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(path)
	requirements.NoError(err)
	defer func() { _ = st.Close() }()
	requirements.NoError(st.InitSchema())
	target, sourceID := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("event", "event@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	_, note := projectionFixture(t, st, "manual", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", target), note))

	data := &MessagePersistData{
		Message:      &Message{SourceID: sourceID, SourceMessageID: "event", MessageType: meetingLinkCalendarType, Subject: sql.NullString{String: "Project planning", Valid: true}},
		Conversation: &ConversationPersistData{SourceConversationID: "event", ConversationType: meetingLinkCalendarType, Title: "Project planning"},
		RawMIME:      []byte(strings.Replace(linkSeriesRaw, `"id":"series"`, `"id":"event"`, 1)),
		RawFormat:    "gcal_json",
		Metadata:     &sql.NullString{String: `{"calendar_id":"primary@example.com"}`, Valid: true},
	}
	_, err = st.PersistMessage(data)
	requirements.NoError(err)

	result, err := CopySubset(path, t.TempDir(), 10, false)
	requirements.NoError(err)
	assertions.Zero(result.MeetingLinkDecisions)
	assertions.Equal(int64(1), result.OmittedMeetingLinkDecisions)
}

func TestDecisionsSubsetKeepsSelectedOrphanAlias(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(path)
	requirements.NoError(err)
	defer func() { _ = st.Close() }()
	requirements.NoError(st.InitSchema())
	orphan, _ := calendarLinkFixture(t, st, "primary@example.com", `{"id":"edited","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "manual-alias", "plaud_json", `{}`)
	requirements.NoError(st.LinkMeetingContext(context.Background(), fmt.Sprintf("evt:%d", orphan), note))
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	for index, id := range []int64{orphan, master, note} {
		_, err := st.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, time.Date(2027, 3, 14, 10+index, 0, 0, 0, time.UTC), id)
		requirements.NoError(err)
	}
	dir := t.TempDir()
	result, err := CopySubset(path, dir, 2, false)
	requirements.NoError(err)
	assertions.Equal(int64(1), result.MeetingLinkDecisions)
	assertions.Zero(result.OmittedMeetingLinkDecisions)
	dst, err := Open(filepath.Join(dir, "msgvault.db"))
	requirements.NoError(err)
	defer func() { _ = dst.Close() }()
	assertions.Equal([]OccurrenceRef{{MessageID: master, Key: "20270314T100000Z"}}, linkTargets(memberLinks(t, dst, OwnerScope(), note)))
}
