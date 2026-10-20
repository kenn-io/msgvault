package store

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

func TestICalendarCancellationArrivalOrdersKeepDetails(t *testing.T) {
	invite := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Meeting\r\nATTENDEE:mailto:guest@example.com")
	cancel := calendarDocument("CANCEL", "SEQUENCE:2\r\nDTSTAMP:20270301T130000Z\r\nSTATUS:CANCELLED")
	for _, cancelFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "invite-first", true: "cancel-first"}[cancelFirst], func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			docs := [][]byte{invite, cancel}
			if cancelFirst {
				docs = [][]byte{cancel, invite}
			}
			var sum ICalendarImportSummary
			var err error
			for _, doc := range docs {
				sum, err = st.PersistICalendarContext(t.Context(), source, doc)
				requirements.NoError(err)
			}
			id := sum.MessageIDs[0]
			event := calendarRows(t, st, id)[0].Event
			assertions.Equal("cancelled", event.Status)
			requirements.NotNil(event.Start)
			assertions.Equal("2027-03-02T12:00:00Z", event.Start.Format(time.RFC3339))
			requirements.Len(event.Attendees, 1)
			assertions.Equal("guest@example.com", event.Attendees[0].Email)
			var title string
			requirements.NoError(st.db.QueryRow(`SELECT subject FROM messages WHERE id=?`, id).Scan(&title))
			assertions.Equal("Meeting", title)
			metadata, err := st.GetMessageMetadata(id)
			requirements.NoError(err)
			meta, err := calevent.ParseICSMetadata([]byte(metadata.String))
			requirements.NoError(err)
			assertions.Equal(int64(2), meta.Sequence)
			assertions.Equal("2027-03-01T13:00:00Z", meta.Stamp.Format(time.RFC3339))
			from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			assertions.Len(page.Occurrences, 1)
		})
	}
}

func TestICalendarSupersededExceptionActivity(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	sum, err := st.PersistICalendarContext(t.Context(), source, calendarFamilyDocument())
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTART:20270302T140000Z\r\nRRULE:FREQ=DAILY;COUNT=2"))
	requirements.NoError(err)
	candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), sum.MessageIDs)
	requirements.NoError(err)
	requirements.Len(candidates, 2)
	assertions.True(candidates[0].Eligible)
	assertions.False(candidates[1].Eligible)
}

func TestICalendarSnapshotRevivesUnchangedException(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	_, err := st.PersistICalendarContext(t.Context(), source, calendarFamilyDocument())
	requirements.NoError(err)
	master := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTART:20270302T140000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2")
	_, err = st.PersistICalendarContext(t.Context(), source, master)
	requirements.NoError(err)
	snapshot := strings.Replace(string(master), "END:VCALENDAR", "BEGIN:VEVENT\r\nUID:family@example.com\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H\r\nSUMMARY:Moved occurrence\r\nEND:VEVENT\r\nEND:VCALENDAR", 1)
	_, err = st.PersistICalendarContext(t.Context(), source, []byte(snapshot))
	requirements.NoError(err)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
	requirements.NoError(err)
	assertions.Len(page.Occurrences, 3)
}

func TestICalendarArchiveOnlyCancelledMasterDoesNotSuppressException(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	master, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:2\r\nSTATUS:CANCELLED\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2"))
	requirements.NoError(err)
	activeException, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:3\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T180000Z\r\nDURATION:PT1H"))
	requirements.NoError(err)
	requirements.Len(master.MessageIDs, 1)
	requirements.Len(activeException.MessageIDs, 1)
	_, err = st.db.Exec(`INSERT INTO source_merge_archive_only_messages(message_id,original_source_message_id) VALUES (?,?)`, master.MessageIDs[0], "archived-calendar-master")
	requirements.NoError(err)

	candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), activeException.MessageIDs)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.True(candidates[0].Eligible)

	from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("confirmed", page.Occurrences[0].Status)
	assertions.Equal(time.Date(2027, 3, 3, 18, 0, 0, 0, time.UTC), page.Occurrences[0].Start)
}

func TestICalendarSparseSeriesCancellationTombstoneCancelsMasterlessException(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	exception, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:1\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T180000Z\r\nDURATION:PT1H"))
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "SEQUENCE:2\r\nSTATUS:CANCELLED"))
	requirements.NoError(err)
	requirements.Len(exception.MessageIDs, 1)

	from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	assertions.Equal("cancelled", page.Occurrences[0].Status)
	assertions.Equal(time.Date(2027, 3, 3, 18, 0, 0, 0, time.UTC), page.Occurrences[0].Start)

	candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), exception.MessageIDs)
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.False(candidates[0].Eligible)
}

func TestICalendarRecoveredMasterSupersedesExceptionByActiveRevision(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	exception, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:0\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T180000Z\r\nDURATION:PT1H"))
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("CANCEL", "SEQUENCE:2\r\nSTATUS:CANCELLED"))
	requirements.NoError(err)
	_, err = st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2"))
	requirements.NoError(err)
	requirements.Len(exception.MessageIDs, 1)

	metadata, err := st.GetMessageMetadata(exception.MessageIDs[0])
	requirements.NoError(err)
	meta, err := calevent.ParseICSMetadata([]byte(metadata.String))
	requirements.NoError(err)
	assertions.True(meta.Superseded)

	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	assertions.Equal([]time.Time{
		time.Date(2027, 3, 2, 12, 0, 0, 0, time.UTC),
		time.Date(2027, 3, 3, 12, 0, 0, 0, time.UTC),
	}, []time.Time{page.Occurrences[0].Start, page.Occurrences[1].Start})
	assertions.Equal([]string{"cancelled", "cancelled"}, []string{page.Occurrences[0].Status, page.Occurrences[1].Status})
}

func TestICalendarStartOnlyCancellationRecoversOlderRecurrence(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)

	cancel := calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H")
	_, err := st.PersistICalendarContext(t.Context(), source, cancel)
	requirements.NoError(err)

	invite := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2")
	result, err := st.PersistICalendarContext(t.Context(), source, invite)
	requirements.NoError(err)
	requirements.Len(result.MessageIDs, 1)

	row := calendarRows(t, st, result.MessageIDs[0])[0]
	assertions.Equal("cancelled", row.Event.Status)
	assertions.Len(row.Event.Recurrence, 1)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	assertions.Equal([]string{"cancelled", "cancelled"}, []string{page.Occurrences[0].Status, page.Occurrences[1].Status})
}

func TestEmailCalendarMixedCancellationKeepsSibling(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	doc := calendarDocument("CANCEL", "STATUS:CANCELLED\r\nDTSTART:20270302T120000Z")
	mixed := strings.Replace(string(doc), "END:VCALENDAR", "BEGIN:VEVENT\r\nUID:guest-removal@example.com\r\nATTENDEE:mailto:guest@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR", 1)
	parent, source := persistInviteEmail(t, st, "mixed", inviteEmail([]byte(mixed)))
	ids := emailEvents(t, st, source)
	assertions.Empty(ids)
	meta, err := st.GetMessageMetadata(parent)
	requirements.NoError(err)
	assertions.Contains(meta.String, "update ignored: ORGANIZER does not match invite_organizer")
	assertions.Contains(meta.String, "guest removal ignored")
}

func TestEmailCalendarOrdinaryIngestDoesNoCalendarQueries(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	parent, source := persistInviteEmail(t, st, "ordinary", []byte("Subject: plain\r\n\r\nBody"))
	raw := []byte("Subject: plain\r\n\r\nBody")
	var queries int
	err := st.withAttributionTxContext(t.Context(), attributionLock{Sources: []int64{source}}, func(tx *loggedTx) error {
		rebind := tx.rebind
		tx.rebind = func(query string) string { queries++; return rebind(query) }
		_, _, err := st.refreshEmailCalendarEventsWith(t.Context(), tx, parent, &raw)
		assertions.Zero(queries)
		return err
	})
	requirements.NoError(err)
	var meta sql.NullString
	requirements.NoError(st.db.QueryRow(`SELECT metadata FROM messages WHERE id=?`, parent).Scan(&meta))
	assertions.NotContains(meta.String, "calendar_invite_errors")
}
