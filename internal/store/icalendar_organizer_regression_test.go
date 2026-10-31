package store

import (
	"database/sql"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmailCalendarCorruptFamilyMetadataKeepsEmailAndUnrelatedInvite(t *testing.T) {
	for _, tc := range []struct {
		name, metadata, organizer string
	}{
		{"null", "", "ORGANIZER:mailto:organizer@example.com\r\n"},
		{"invalid stamp", `{"ics_version":1,"ics_stamp":"broken","invite_organizer":"organizer@example.com"}`, "ORGANIZER:mailto:organizer@example.com\r\n"},
		{"unknown version without organizer", `{"ics_version":2}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			initial := calendarDocument("REQUEST", "ORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Original")
			_, source := persistInviteEmail(t, st, "initial", inviteEmail(initial))
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 1)
			// Keep the accepted projection intact while corrupting its retained metadata.
			_, err := st.db.Exec(`UPDATE messages SET metadata=`+st.dialect.JSONBindExpr()+` WHERE id=?`, sql.NullString{String: tc.metadata, Valid: tc.metadata != ""}, ids[0])
			requirements.NoError(err)

			update := calendarDocument("REQUEST", tc.organizer+"SEQUENCE:1\r\nDTSTART:20270302T180000Z\r\nSUMMARY:Replacement")
			siblings := "BEGIN:VEVENT\r\nUID:family@example.com\r\n" + tc.organizer + "SEQUENCE:1\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T180000Z\r\nEND:VEVENT\r\n" +
				"BEGIN:VEVENT\r\nUID:unrelated@example.com\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270304T100000Z\r\nSUMMARY:Unrelated\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			raw := inviteEmail([]byte(strings.Replace(string(update), "END:VCALENDAR\r\n", siblings, 1)))
			parent, _ := persistInviteEmail(t, st, "update", raw)
			stored, err := st.GetMessageRaw(parent)
			requirements.NoError(err)
			assertions.Equal(raw, stored)
			metadata, err := st.GetMessageMetadata(parent)
			requirements.NoError(err)
			var diagnostics map[string][]string
			requirements.NoError(json.Unmarshal([]byte(metadata.String), &diagnostics))
			requirements.NotEmpty(diagnostics["calendar_invite_errors"])
			assertions.Contains(strings.Join(diagnostics["calendar_invite_errors"], "\n"), "decode retained ICS family metadata")

			retained, err := st.GetMessageRaw(ids[0])
			requirements.NoError(err)
			assertions.Equal(initial, retained)
			retainedMetadata, err := st.GetMessageMetadata(ids[0])
			requirements.NoError(err)
			if tc.metadata == "" {
				assertions.False(retainedMetadata.Valid)
			} else {
				assertions.JSONEq(tc.metadata, retainedMetadata.String)
			}
			var subject, status string
			var start time.Time
			requirements.NoError(st.db.QueryRow(`SELECT m.subject,e.status,e.start_at FROM messages m JOIN calendar_events e ON e.message_id=m.id WHERE m.id=?`, ids[0]).Scan(&subject, &status, &start))
			assertions.Equal("Original", subject)
			assertions.Equal("confirmed", status)
			assertions.Equal("2027-03-02T12:00:00Z", start.UTC().Format(time.RFC3339))
			allIDs := emailEvents(t, st, source)
			requirements.Len(allIDs, 2)
			unrelated := calendarRows(t, st, allIDs[1])
			requirements.Len(unrelated, 1)
			assertions.Equal("Unrelated", unrelated[0].Subject)
			requirements.NotNil(unrelated[0].Event.Start)
			assertions.Equal("2027-03-04T10:00:00Z", unrelated[0].Event.Start.UTC().Format(time.RFC3339))
		})
	}
}

func TestEmailCalendarCorruptExceptionMetadataKeepsEmailAndUnrelatedInvite(t *testing.T) {
	for _, tc := range []struct{ name, metadata string }{
		{"null", ""},
		{"invalid stamp", `{"ics_version":1,"ics_stamp":"broken","invite_organizer":"organizer@example.com"}`},
		{"unknown version", `{"ics_version":2,"invite_organizer":"organizer@example.com"}`},
	} {
		for _, path := range []string{"exception update", "master supersession"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				initial := strings.ReplaceAll(string(calendarFamilyDocument()), "UID:family@example.com\r\n", "UID:family@example.com\r\nORGANIZER:mailto:organizer@example.com\r\n")
				_, source := persistInviteEmail(t, st, "initial", inviteEmail([]byte(initial)))
				ids := emailEvents(t, st, source)
				requirements.Len(ids, 2)
				masterRaw, err := st.GetMessageRaw(ids[0])
				requirements.NoError(err)
				exceptionRaw, err := st.GetMessageRaw(ids[1])
				requirements.NoError(err)
				_, err = st.db.Exec(`UPDATE messages SET metadata=`+st.dialect.JSONBindExpr()+` WHERE id=?`, sql.NullString{String: tc.metadata, Valid: tc.metadata != ""}, ids[1])
				requirements.NoError(err)

				props := "ORGANIZER:mailto:organizer@example.com\r\nSEQUENCE:1\r\nDTSTART:20270303T180000Z\r\nSUMMARY:Replacement\r\n"
				if path == "exception update" {
					props += "RECURRENCE-ID:20270303T120000Z"
				} else {
					props += "RRULE:FREQ=DAILY;COUNT=2"
				}
				update := calendarDocument("REQUEST", props)
				unrelated := "BEGIN:VEVENT\r\nUID:unrelated@example.com\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270304T100000Z\r\nSUMMARY:Unrelated\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
				raw := inviteEmail([]byte(strings.Replace(string(update), "END:VCALENDAR\r\n", unrelated, 1)))
				parent, _ := persistInviteEmail(t, st, "update", raw)
				stored, err := st.GetMessageRaw(parent)
				requirements.NoError(err)
				assertions.Equal(raw, stored)
				metadata, err := st.GetMessageMetadata(parent)
				requirements.NoError(err)
				var diagnostics map[string][]string
				requirements.NoError(json.Unmarshal([]byte(metadata.String), &diagnostics))
				requirements.NotEmpty(diagnostics["calendar_invite_errors"])
				assertions.Contains(strings.Join(diagnostics["calendar_invite_errors"], "\n"), "decode retained ICS")

				retained, err := st.GetMessageRaw(ids[0])
				requirements.NoError(err)
				assertions.Equal(masterRaw, retained)
				retained, err = st.GetMessageRaw(ids[1])
				requirements.NoError(err)
				assertions.Equal(exceptionRaw, retained)
				retainedMetadata, err := st.GetMessageMetadata(ids[1])
				requirements.NoError(err)
				if tc.metadata == "" {
					assertions.False(retainedMetadata.Valid)
				} else {
					assertions.JSONEq(tc.metadata, retainedMetadata.String)
				}
				allIDs := emailEvents(t, st, source)
				requirements.Len(allIDs, 3)
				rows := calendarRows(t, st, allIDs[2])
				requirements.Len(rows, 1)
				assertions.Equal("Unrelated", rows[0].Subject)
				requirements.NotNil(rows[0].Event.Start)
				assertions.Equal("2027-03-04T10:00:00Z", rows[0].Event.Start.UTC().Format(time.RFC3339))
			})
		}
	}
}

func TestEmailCalendarCorruptMasterMetadataAfterExceptionKeepsEmailAndUnrelatedInvite(t *testing.T) {
	for _, tc := range []struct{ name, metadata string }{
		{"null", ""},
		{"invalid stamp", `{"ics_version":1,"ics_stamp":"broken","invite_organizer":"organizer@example.com"}`},
		{"unknown version", `{"ics_version":2,"invite_organizer":"organizer@example.com"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			exception := calendarDocument("REQUEST", "ORGANIZER:mailto:organizer@example.com\r\nSEQUENCE:1\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nSUMMARY:Moved occurrence")
			_, source := persistInviteEmail(t, st, "exception", inviteEmail(exception))
			master := calendarDocument("REQUEST", "ORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270302T120000Z\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Original series")
			persistInviteEmail(t, st, "master", inviteEmail(master))
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 2)
			exceptionMetadata, err := st.GetMessageMetadata(ids[0])
			requirements.NoError(err)
			// The older exception supplies organizer continuity before the master is read.
			_, err = st.db.Exec(`UPDATE messages SET metadata=`+st.dialect.JSONBindExpr()+` WHERE id=?`, sql.NullString{String: tc.metadata, Valid: tc.metadata != ""}, ids[1])
			requirements.NoError(err)

			update := calendarDocument("REQUEST", "ORGANIZER:mailto:organizer@example.com\r\nSEQUENCE:2\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T180000Z\r\nSUMMARY:Replacement")
			unrelated := "BEGIN:VEVENT\r\nUID:unrelated@example.com\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270304T100000Z\r\nSUMMARY:Unrelated\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			raw := inviteEmail([]byte(strings.Replace(string(update), "END:VCALENDAR\r\n", unrelated, 1)))
			parent, _ := persistInviteEmail(t, st, "update", raw)
			stored, err := st.GetMessageRaw(parent)
			requirements.NoError(err)
			assertions.Equal(raw, stored)
			metadata, err := st.GetMessageMetadata(parent)
			requirements.NoError(err)
			var diagnostics map[string][]string
			requirements.NoError(json.Unmarshal([]byte(metadata.String), &diagnostics))
			requirements.Len(diagnostics["calendar_invite_errors"], 1)
			assertions.Contains(diagnostics["calendar_invite_errors"][0], "decode retained ICS master metadata")

			retained, err := st.GetMessageRaw(ids[0])
			requirements.NoError(err)
			assertions.Equal(exception, retained)
			retainedMetadata, err := st.GetMessageMetadata(ids[0])
			requirements.NoError(err)
			assertions.Equal(exceptionMetadata, retainedMetadata)
			retained, err = st.GetMessageRaw(ids[1])
			requirements.NoError(err)
			assertions.Equal(master, retained)
			retainedMetadata, err = st.GetMessageMetadata(ids[1])
			requirements.NoError(err)
			if tc.metadata == "" {
				assertions.False(retainedMetadata.Valid)
			} else {
				assertions.JSONEq(tc.metadata, retainedMetadata.String)
			}
			allIDs := emailEvents(t, st, source)
			requirements.Len(allIDs, 3)
			rows := calendarRows(t, st, allIDs[2])
			requirements.Len(rows, 1)
			assertions.Equal("Unrelated", rows[0].Subject)
			requirements.NotNil(rows[0].Event.Start)
			assertions.Equal("2027-03-04T10:00:00Z", rows[0].Event.Start.UTC().Format(time.RFC3339))
		})
	}
}

func TestEmailCalendarRejectedSiblingsDoNotPreserveExceptions(t *testing.T) {
	for _, mode := range []string{"rejected exception", "rejected master replay"} {
		t.Run(mode, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			initial := strings.ReplaceAll(string(calendarFamilyDocument()), "UID:family@example.com\r\n", "UID:family@example.com\r\nORGANIZER:mailto:organizer@example.com\r\n")
			persistInviteEmail(t, st, "initial", inviteEmail([]byte(initial)))
			master := string(calendarDocument("REQUEST", "ORGANIZER:mailto:organizer@example.com\r\nSEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2"))
			exception := strings.Split(initial, "BEGIN:VEVENT")[2]
			if mode == "rejected master replay" {
				persistInviteEmail(t, st, "supersede", inviteEmail([]byte(master)))
				master = strings.ReplaceAll(master, "organizer@example.com", "other@example.com")
			} else {
				exception = strings.ReplaceAll(exception, "organizer@example.com", "other@example.com")
			}
			mixed := strings.Replace(master, "END:VCALENDAR\r\n", "BEGIN:VEVENT"+exception, 1)
			parent, _ := persistInviteEmail(t, st, "mixed", inviteEmail([]byte(mixed)))
			metadata, err := st.GetMessageMetadata(parent)
			requirements.NoError(err)
			assertions.Contains(metadata.String, "ORGANIZER does not match invite_organizer")
			from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal("2027-03-03T12:00:00Z", page.Occurrences[0].Start.Format(time.RFC3339))
		})
	}
}
