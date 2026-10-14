package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecisionsSubsetCopy(t *testing.T) {
	requirements := require.New(t)

	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(path)
	requirements.NoError(err)
	defer func() { _ = st.Close() }()
	requirements.NoError(st.InitSchema())
	data, event := projectionFixture(t, st, "event", "gcal_json", `{"id":"event","iCalUID":"event@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	data.Message.MessageType = "calendar_event"
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err = st.UpsertMessage(data.Message)
	requirements.NoError(err)
	noteData, note := projectionFixture(t, st, "note", "granola_json", `{"calendar_event":{"calendar_event_id":"event"}}`)
	noteData.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 11, 0, 0, 0, time.UTC), Valid: true}
	_, err = st.UpsertMessage(noteData.Message)
	requirements.NoError(err)
	ref := fmt.Sprintf("evt:%d", event)
	_, err = st.db.Exec(`INSERT INTO meeting_link_decisions(member_message_id,target_message_id,target_ref,target_logical,decision) VALUES (?, ?, ?, ?, 'split')`, note, event, ref, "ical:event@example.org")
	requirements.NoError(err)
	for _, tc := range []struct {
		name          string
		limit         int
		kept, dropped int64
	}{{"both", 2, 1, 0}, {"member-only", 1, 0, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			dir := t.TempDir()
			result, err := CopySubset(path, dir, tc.limit, false)
			requirements.NoError(err)
			assertions.Equal(tc.kept, result.MeetingLinkDecisions)
			assertions.Equal(tc.dropped, result.OmittedMeetingLinkDecisions)
			dst, err := Open(filepath.Join(dir, "msgvault.db"))
			requirements.NoError(err)
			defer func() { _ = dst.Close() }()
			var count int64
			requirements.NoError(dst.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions`).Scan(&count))
			assertions.Equal(tc.kept, count)
			if tc.kept > 0 {
				var decision, logical string
				requirements.NoError(dst.db.QueryRow(`SELECT decision,target_logical FROM meeting_link_decisions WHERE member_message_id = ? AND target_ref = ?`, note, ref).Scan(&decision, &logical))
				assertions.Equal("split", decision)
				assertions.Equal("ical:event@example.org", logical)
			}
			assertions.Equal([]string{"google_event_id:event"}, linkKeyValues(t, dst, note))
		})
	}
}
