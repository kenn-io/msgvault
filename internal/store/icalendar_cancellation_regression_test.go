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

func TestICalendarDamagedSparseCancellationKeepsReadableOccurrences(t *testing.T) {
	for _, recurrence := range []string{"", "\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2"} {
		for _, metadata := range []string{`{"ics_version":2,"status":"cancelled"}`, `{"ics_version":1,"ics_stamp":"broken"}`, ""} {
			t.Run(fmt.Sprintf("series=%t/metadata=%s", recurrence != "", metadata), func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				source := icsSource(t, st)
				exception, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "SEQUENCE:3\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H"))
				requirements.NoError(err)
				cancel := calendarDocument("CANCEL", "SEQUENCE:2\r\nSTATUS:CANCELLED"+recurrence)
				tombstone, err := st.PersistICalendarContext(t.Context(), source, cancel)
				requirements.NoError(err)
				other := strings.Replace(string(calendarDocument("REQUEST", "DTSTART:20270303T160000Z\r\nDURATION:PT1H\r\nSUMMARY:Other event")), "family@example.com", "unrelated@example.com", 1)
				sibling, err := st.PersistICalendarContext(t.Context(), source, []byte(other))
				requirements.NoError(err)
				_, err = st.db.Exec(`UPDATE messages SET metadata=`+st.dialect.JSONBindExpr()+` WHERE id=?`, sql.NullString{String: metadata, Valid: metadata != ""}, tombstone.MessageIDs[0])
				requirements.NoError(err)

				from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
				page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
				requirements.NoError(err)
				requirements.Len(page.Occurrences, 2)
				assertions.Equal(exception.MessageIDs[0], page.Occurrences[0].MessageID)
				assertions.Equal("cancelled", page.Occurrences[0].Status)
				assertions.Equal("2027-03-03T14:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
				assertions.Equal(sibling.MessageIDs[0], page.Occurrences[1].MessageID)
				assertions.Equal("confirmed", page.Occurrences[1].Status)
				assertions.Equal([]CalendarUnexpandable{{MessageID: tombstone.MessageIDs[0], Reason: "event data could not be decoded"}}, page.Unexpandable)
				var status string
				requirements.NoError(st.db.QueryRow(`SELECT status FROM calendar_events WHERE message_id=?`, tombstone.MessageIDs[0]).Scan(&status))
				assertions.Equal("cancelled", status)
				stored, err := st.GetMessageRaw(tombstone.MessageIDs[0])
				requirements.NoError(err)
				assertions.Equal(cancel, stored)
				for _, load := range []func() ([]ActivityCandidate, error){
					func() ([]ActivityCandidate, error) {
						return st.LoadActivityCandidatesByIDContext(t.Context(), exception.MessageIDs)
					},
					func() ([]ActivityCandidate, error) { return st.LoadQueuedActivityCandidatesContext(t.Context(), 100) },
					func() ([]ActivityCandidate, error) { return st.ScanAllActivityCandidatesContext(t.Context(), 0, 100) },
				} {
					candidates, err := load()
					requirements.NoError(err)
					var found bool
					for _, candidate := range candidates {
						if candidate.MessageID == exception.MessageIDs[0] || candidate.MessageID == tombstone.MessageIDs[0] {
							assertions.False(candidate.Eligible)
						}
						found = found || candidate.MessageID == exception.MessageIDs[0]
					}
					assertions.True(found)
				}
			})
		}
	}
}

func TestICalendarHistoricalMasterDoesNotCancelActivity(t *testing.T) {
	for _, mode := range []string{"merged", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			history, err := st.GetOrCreateSource("ics", "history-example")
			requirements.NoError(err)
			cancelled, err := st.PersistICalendarContext(t.Context(), history.ID, calendarDocument("CANCEL", "SEQUENCE:2\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2"))
			requirements.NoError(err)
			active, err := st.PersistICalendarContext(t.Context(), source, calendarFamilyDocument())
			requirements.NoError(err)
			if mode == "merged" {
				_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: source})
				requirements.NoError(err)
				var archived bool
				requirements.NoError(st.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM source_merge_archive_only_messages WHERE message_id=?)`, cancelled.MessageIDs[0]).Scan(&archived))
				requirements.True(archived)
			} else {
				// Place a deleted master in the same source without replacing its live identity.
				_, err = st.db.Exec(`UPDATE messages SET source_id=?, source_message_id='deleted-master', deleted_at=CURRENT_TIMESTAMP WHERE id=?`, source, cancelled.MessageIDs[0])
				requirements.NoError(err)
			}
			candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), active.MessageIDs[1:])
			requirements.NoError(err)
			requirements.Len(candidates, 1)
			assertions.True(candidates[0].Eligible)
			from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal("confirmed", page.Occurrences[0].Status)
		})
	}
}

func TestICalendarSparseMasterCancellationOfDetachedException(t *testing.T) {
	for _, bounds := range []string{"", "\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H"} {
		for _, cancelFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("bounded=%t/cancel-first=%t", bounds != "", cancelFirst), func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				source := icsSource(t, st)
				exception := calendarDocument("REQUEST", "SEQUENCE:3\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H")
				cancel := calendarDocument("CANCEL", "SEQUENCE:2\r\nSTATUS:CANCELLED"+bounds)
				docs := [][]byte{exception, cancel}
				if cancelFirst {
					docs = [][]byte{cancel, exception}
				}
				var exceptionID int64
				for i, doc := range docs {
					sum, err := st.PersistICalendarContext(t.Context(), source, doc)
					requirements.NoError(err)
					if (i == 0) != cancelFirst {
						exceptionID = sum.MessageIDs[0]
					}
				}
				from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
				page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
				requirements.NoError(err)
				requirements.Len(page.Occurrences, 1)
				assertions.Equal(exceptionID, page.Occurrences[0].MessageID)
				assertions.Equal("cancelled", page.Occurrences[0].Status)
				candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), []int64{exceptionID})
				requirements.NoError(err)
				requirements.Len(candidates, 1)
				assertions.False(candidates[0].Eligible)
			})
		}
	}
}

func TestICalendarRecoveredMasterSupersedesStaleException(t *testing.T) {
	docs := [][]byte{
		calendarDocument("REQUEST", "SEQUENCE:0\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nDURATION:PT1H"),
		calendarDocument("CANCEL", "SEQUENCE:2\r\nSTATUS:CANCELLED"),
		calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2"),
	}
	for _, order := range [][3]int{{0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source := icsSource(t, st)
			for _, i := range order {
				_, err := st.PersistICalendarContext(t.Context(), source, docs[i])
				requirements.NoError(err)
			}
			from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal("2027-03-03T12:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
			assertions.Equal("cancelled", page.Occurrences[0].Status)
		})
	}
}
