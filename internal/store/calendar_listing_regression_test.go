package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListCalendarOccurrencesICSEvidenceReadFailure(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2"))
	requirements.NoError(err)
	// Break the evidence read while leaving typed event queries usable.
	_, err = st.db.Exec(`ALTER TABLE sources RENAME COLUMN sync_config TO unavailable_sync_config`)
	requirements.NoError(err)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.ErrorContains(err, "read ICS recurrence evidence")
	assert.Empty(t, page.Unexpandable)
}

func TestICalendarDamagedExceptionMetadataKeepsTypedState(t *testing.T) {
	for _, tc := range []struct{ name, method, bounds, status, start string }{
		{"sparse cancellation", "CANCEL", "", "cancelled", "2027-03-03T12:00:00Z"},
		{"bounded cancellation", "CANCEL", "\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H", "cancelled", "2027-03-03T14:00:00Z"},
		{"moved", "REQUEST", "\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H", "confirmed", "2027-03-03T14:00:00Z"},
	} {
		for _, metadata := range []string{"", `{"ics_version":2,"ics_superseded":true,"status":"cancelled"}`, `{"ics_version":1,"ics_stamp":"broken"}`} {
			t.Run(tc.name+"/"+metadata, func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				source := icsSource(t, st)
				master, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2"))
				requirements.NoError(err)
				exception, err := st.PersistICalendarContext(t.Context(), source, calendarDocument(tc.method, "RECURRENCE-ID:20270303T120000Z"+tc.bounds))
				requirements.NoError(err)
				id := exception.MessageIDs[0]
				before := calendarRows(t, st, id)[0].Event
				_, err = st.db.Exec(`UPDATE messages SET metadata=`+st.dialect.JSONBindExpr()+` WHERE id=?`, sql.NullString{String: metadata, Valid: metadata != ""}, id)
				requirements.NoError(err)
				for _, rebuild := range []bool{false, true} {
					if rebuild {
						requirements.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error {
							return st.refreshCalendarEventProjectionWith(t.Context(), tx, id)
						}))
					}
					assertions.Equal(before, calendarRows(t, st, id)[0].Event)
					from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
					page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
					requirements.NoError(err)
					if assertions.Len(page.Occurrences, 1) {
						assertions.Equal(id, page.Occurrences[0].MessageID)
						assertions.Equal(master.MessageIDs[0], page.Occurrences[0].SeriesMessageID)
						assertions.Equal(tc.status, page.Occurrences[0].Status)
						assertions.Equal(tc.start, page.Occurrences[0].Start.Format(time.RFC3339))
					}
					assertions.Equal([]CalendarUnexpandable{{MessageID: id, Reason: "event data could not be decoded"}}, page.Unexpandable)
					candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
					requirements.NoError(err)
					requirements.Len(candidates, 1)
					assertions.Equal(tc.status != "cancelled", candidates[0].Eligible)
				}
			})
		}
	}
}

func TestListCalendarOccurrencesMalformedICSMetadata(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	bad, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	requirements.NoError(err)
	requirements.Len(bad.MessageIDs, 1)
	goodRaw := strings.ReplaceAll(string(calendarDocument("REQUEST", "DTSTART:20270302T160000Z")), "family@example.com", "readable@example.com")
	good, err := st.PersistICalendarContext(t.Context(), source, []byte(goodRaw))
	requirements.NoError(err)
	requirements.Len(good.MessageIDs, 1)
	_, err = st.db.Exec(`UPDATE messages SET metadata=? WHERE id=?`, `{"ics_version":"invalid"}`, bad.MessageIDs[0])
	requirements.NoError(err)
	requirements.NoError(st.withTxContext(t.Context(), func(tx *loggedTx) error {
		return st.refreshCalendarEventProjectionWith(t.Context(), tx, bad.MessageIDs[0])
	}))
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	assertions.Equal(bad.MessageIDs[0], page.Occurrences[0].MessageID)
	assertions.Equal(good.MessageIDs[0], page.Occurrences[1].MessageID)
	assertions.Equal([]CalendarUnexpandable{{MessageID: bad.MessageIDs[0], Reason: "event data could not be decoded"}}, page.Unexpandable)
}

func TestListCalendarOccurrencesICSVerificationRequestBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		series, year int
		wantError    bool
	}{
		{"verification alone", 20, 2027, true},
		{"verification and expansion", 9, 2100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			for i := range tc.series {
				raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTIMEZONE\r\nTZID:Etc/UTC\r\n" +
					"BEGIN:STANDARD\r\nDTSTART:20270101T000000\r\nTZOFFSETFROM:+0000\r\nTZOFFSETTO:+0000\r\nEND:STANDARD\r\n" +
					"END:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:" + fmt.Sprintf("large-%d@example.com", i) + "\r\n" +
					"DTSTART;TZID=Etc/UTC:20270101T090000\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=50000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
				summary, err := st.PersistICalendarContext(t.Context(), source, []byte(raw))
				requirements.NoError(err)
				requirements.Len(summary.MessageIDs, 1)
			}
			from := time.Date(tc.year, 1, 1, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			if tc.wantError {
				assertions.ErrorIs(err, ErrQueryTooBroad)
				return
			}
			requirements.NoError(err)
			assertions.Len(page.Occurrences, 1)
			requirements.Len(page.Unexpandable, 8)
			assertions.Equal("request work limit reached; long-running series count their steps from their start", page.Unexpandable[0].Reason)
		})
	}
}
