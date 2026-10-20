package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestCalendarOccurrencePagesMatchFullRead(t *testing.T) {
	for _, budget := range []bool{false, true} {
		t.Run(fmt.Sprintf("budget=%t", budget), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			calendarFixture(t, st, "s", `{"id":"s","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=30"]}`)
			var moved int64
			if budget {
				calendarFixture(t, st, "early", `{"id":"early","start":{"dateTime":"2027-03-01T09:00:00Z"},"end":{"dateTime":"2027-03-01T10:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=1"]}`)
				for i := range 9 {
					calendarFixture(t, st, fmt.Sprintf("long-%d", i), fmt.Sprintf(`{"id":"long-%d","start":{"dateTime":"1800-01-01T10:00:00Z"},"end":{"dateTime":"1800-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`, i))
				}
				start := time.Date(2027, 3, 1, 8, 0, 0, 0, time.UTC).AddDate(0, 0, -49943)
				calendarFixture(t, st, "finishing", fmt.Sprintf(`{"id":"finishing","start":{"dateTime":%q},"end":{"dateTime":%q},"recurrence":["RRULE:FREQ=DAILY;COUNT=49944"]}`, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339)))
				calendarFixture(t, st, "later", `{"id":"later","start":{"dateTime":"2027-03-25T10:00:00Z"},"end":{"dateTime":"2027-03-25T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=1"]}`)
				// 13 rows + 31 steps for s + 2 for early + 9*50001 for long
				// + 49945 for finishing exhaust 500000 before later is read.
			} else {
				_, moved = calendarFixture(t, st, "moved", `{"id":"moved","recurringEventId":"s","originalStartTime":{"dateTime":"2027-03-03T10:00:00Z"},"start":{"dateTime":"2027-03-02T12:00:00Z"},"end":{"dateTime":"2027-03-02T13:00:00Z"}}`)
				calendarFixture(t, st, "all-day", `{"id":"all-day","start":{"date":"2027-03-10","timeZone":"America/Los_Angeles"},"end":{"date":"2027-03-11","timeZone":"America/Los_Angeles"}}`)
			}
			from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
			query := CalendarOccurrenceQuery{From: from, To: from.AddDate(0, 1, 0), Limit: new(500)}
			full, err := st.ListCalendarOccurrencesContext(t.Context(), query)
			requirements.NoError(err)
			requirements.Empty(full.NextCursor)
			if !budget {
				var ids []int64
				for _, occurrence := range full.Occurrences {
					ids = append(ids, occurrence.MessageID)
				}
				assertions.Contains(ids, moved)
			}
			query.Limit = new(5)
			var paged []CalendarOccurrence
			for {
				page, err := st.ListCalendarOccurrencesContext(t.Context(), query)
				requirements.NoError(err)
				if query.Cursor == "" {
					assertions.Equal(full.Unexpandable, page.Unexpandable)
					if !budget {
						requirements.Len(page.Occurrences, 5)
						assertions.Equal("2027-03-05T10:00:00Z", page.Occurrences[4].Start.Format(time.RFC3339))
					}
				} else {
					assertions.Empty(page.Unexpandable)
				}
				paged = append(paged, page.Occurrences...)
				if page.NextCursor == "" {
					break
				}
				requirements.NotEqual(query.Cursor, page.NextCursor)
				query.Cursor = page.NextCursor
			}
			assertions.Equal(full.Occurrences, paged)
		})
	}
}

func TestCalendarOccurrenceCursorScope(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, _ := calendarFixture(t, st, "first", calendarRaw)
	calendarFixture(t, st, "second", calendarRaw)
	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	query := CalendarOccurrenceQuery{From: from, To: from.AddDate(0, 1, 0), Limit: new(1)}
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{data.Message.SourceID})
	first, err := st.ListCalendarOccurrencesContext(ctx, query)
	requirements.NoError(err)
	requirements.Len(first.Occurrences, 1)
	requirements.NotEmpty(first.NextCursor)
	query.Cursor = first.NextCursor
	second, err := st.ListCalendarOccurrencesContext(ctx, query)
	requirements.NoError(err)
	requirements.Len(second.Occurrences, 1)
	assertions.NotEqual(first.Occurrences[0].Ref, second.Occurrences[0].Ref)
	assertions.Empty(second.NextCursor)
	query.Cursor = first.NextCursor
	_, err = newRFC822IDBackfillBackendStore(t).ListCalendarOccurrencesContext(ctx, query)
	requirements.ErrorIs(err, ErrCalendarOccurrenceInvalid)
	for _, changed := range []context.Context{t.Context(), sourcescope.WithSourceIDs(t.Context(), []int64{})} {
		_, err = st.ListCalendarOccurrencesContext(changed, query)
		requirements.ErrorIs(err, ErrCalendarOccurrenceInvalid)
	}
	offsetQuery := query
	offsetQuery.From = query.From.In(time.FixedZone("offset", -5*3600))
	offsetQuery.To = query.To.In(time.FixedZone("offset", -5*3600))
	_, err = st.ListCalendarOccurrencesContext(ctx, offsetQuery)
	requirements.ErrorIs(err, ErrCalendarOccurrenceInvalid)
	query.To = query.To.Add(time.Hour)
	_, err = st.ListCalendarOccurrencesContext(ctx, query)
	requirements.ErrorIs(err, ErrCalendarOccurrenceInvalid)
	query.To = query.To.Add(-time.Hour)
	decoded, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	requirements.NoError(err)
	var cursor map[string]any
	requirements.NoError(json.Unmarshal(decoded, &cursor))
	cursor["version"] = 2
	decoded, err = json.Marshal(cursor)
	requirements.NoError(err)
	query.Cursor = base64.RawURLEncoding.EncodeToString(decoded)
	_, err = st.ListCalendarOccurrencesContext(ctx, query)
	requirements.ErrorIs(err, ErrCalendarOccurrenceInvalid)
	assert.Contains(t, err.Error(), "unsupported cursor version")
	cursor["future_member"] = true
	decoded, err = json.Marshal(cursor)
	requirements.NoError(err)
	query.Cursor = base64.RawURLEncoding.EncodeToString(decoded)
	_, err = st.ListCalendarOccurrencesContext(ctx, query)
	requirements.ErrorIs(err, ErrCalendarOccurrenceInvalid)
	assertions.Contains(err.Error(), "unsupported cursor version 2")
}

func TestListedRefsOpenToSameOccurrence(t *testing.T) {
	masterContent := calevent.Content{Location: "Example room", Description: "Agenda", JoinURL: "https://meet.example.com/master"}
	for _, tc := range []struct {
		name, fields, zero, state             string
		count                                 int
		orphan, missingRaw, metadataCancelled bool
		content                               calevent.Content
	}{
		{count: 0, name: "missing bounds", state: "missing bounds"},
		{count: 2, name: "plain generated"},
		{count: 1, name: "plain single", state: "single"},
		{count: 2, name: "single overridden at own start", state: "single overridden", fields: `"start":{"dateTime":"2027-03-12T19:00:00Z"},"end":{"dateTime":"2027-03-12T20:00:00Z"}`},
		{count: 2, name: "single cancelled at own start", state: "single overridden", fields: `"status":"cancelled"`, content: masterContent},
		{count: 1, name: "single with undecodable event", state: "single damaged"},
		{count: 1, name: "unsupported master", state: "unsupported", fields: `"start":{"date":"2027-03-12"},"end":{"date":"2027-03-13"}`},
		{count: 1, name: "sparse cancellation under unsupported master", state: "unsupported", fields: `"status":"cancelled"`, content: masterContent},
		{count: 1, name: "cancelled with bounds under bad master", state: "unsupported", fields: `"status":"cancelled","start":{"dateTime":"2027-03-04T19:00:00Z"},"end":{"dateTime":"2027-03-04T20:00:00Z"}`},
		{count: 1, name: "series budget", state: "series budget", fields: `"status":"cancelled","start":{"date":"2027-03-12"},"end":{"date":"2027-03-13"}`},
		{count: 1, name: "request budget", state: "request budget", fields: `"start":{"date":"2027-03-12"},"end":{"date":"2027-03-13"}`},
		{count: 2, name: "unresolvable exception", state: "unresolvable", fields: `"status":"cancelled"`},
		{count: 3, name: "fractional original", state: "fractional original", fields: `"start":{"dateTime":"2027-03-12T19:00:00Z"},"end":{"dateTime":"2027-03-12T20:00:00Z"}`},
		{count: 1, name: "fractional master", state: "fractional master", fields: `"start":{"dateTime":"2027-03-12T19:00:00Z"},"end":{"dateTime":"2027-03-12T20:00:00Z"}`},
		{count: 1, name: "archive-only master", state: "archive-only", fields: `"start":{"dateTime":"2027-03-12T19:00:00Z"},"end":{"dateTime":"2027-03-12T20:00:00Z"}`},
		{count: 1, name: "master arrives later", state: "late master", fields: `"start":{"dateTime":"2027-03-09T17:00:00Z"},"end":{"dateTime":"2027-03-09T18:00:00Z"}`},
		{count: 2, name: "retyped master", state: "retyped", fields: `"start":{"date":"2027-03-03"},"end":{"date":"2027-03-04"}`},
		{count: 2, name: "retyped original", state: "retyped original", fields: `"start":{"date":"2027-03-03"},"end":{"date":"2027-03-04"}`},
		{count: 2, name: "cancelled with bounds", fields: `"status":"cancelled","start":{"dateTime":"2027-03-09T17:00:00Z"},"end":{"dateTime":"2027-03-09T18:00:00Z"}`},
		{count: 2, name: "moved", fields: `"start":{"dateTime":"2027-03-12T19:00:00Z"},"end":{"dateTime":"2027-03-12T20:00:00Z"},"location":"Other room"`, content: calevent.Content{Location: "Other room"}},
		{count: 2, name: "converted", fields: `"start":{"date":"2027-03-12"},"end":{"date":"2027-03-14"}`},
		{count: 2, name: "cancelled missing raw", fields: `"status":"cancelled"`, missingRaw: true, content: masterContent},
		{count: 2, name: "cancelled", fields: `"status":"cancelled"`, content: masterContent},
		{count: 2, name: "sparse stub", fields: `"location":"","description":"","hangoutLink":""`, metadataCancelled: true, content: masterContent},
		{count: 2, name: "bounded cleared content", fields: `"location":"","description":"","hangoutLink":"","start":{"dateTime":"2027-03-09T17:00:00Z"},"end":{"dateTime":"2027-03-09T18:00:00Z"}`, metadataCancelled: true},
		{count: 2, name: "cancelled empty content", fields: `"status":"cancelled","location":"","description":"","hangoutLink":"","conferenceData":{}`, content: masterContent},
		{count: 2, name: "cancelled own content", fields: `"status":"cancelled","location":"Other room","description":"Updated agenda","conferenceData":{"entryPoints":[{"entryPointType":"phone","uri":"tel:+12025550100"},{"entryPointType":"video","uri":"https://video.example.com/exception"}]}`, content: calevent.Content{Location: "Other room", Description: "Updated agenda", JoinURL: "https://video.example.com/exception"}},
		{count: 2, name: "preferred hangout link", fields: `"status":"cancelled","hangoutLink":"https://meet.example.com/event","conferenceData":{"entryPoints":[{"entryPointType":"video","uri":"https://video.example.com/other"}]}`, content: calevent.Content{Location: "Example room", Description: "Agenda", JoinURL: "https://meet.example.com/event"}},
		{count: 2, name: "zero master ID", fields: `"status":"cancelled"`, zero: "master", content: masterContent},
		{count: 2, name: "zero exception ID", fields: `"status":"cancelled"`, zero: "exception", content: masterContent},
		{count: 1, name: "orphan", fields: `"start":{"dateTime":"2027-03-12T19:00:00Z"},"end":{"dateTime":"2027-03-12T20:00:00Z"}`, orphan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := `{"id":"weekly","location":"Example room","description":"Agenda","hangoutLink":"https://meet.example.com/master","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"],"attendees":[{"email":"guest@example.com","responseStatus":"accepted"}]}`
			if tc.state == "request budget" {
				for i := range 10 {
					calendarFixture(t, st, fmt.Sprintf("long-%d", i), fmt.Sprintf(`{"id":"long-%d","start":{"dateTime":"1800-01-01T17:00:00Z"},"end":{"dateTime":"1800-01-01T18:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"]}`, i))
				}
			}
			switch tc.state {
			case "missing bounds":
				raw = `{"id":"missing-bounds"}`
			case "single", "single damaged", "single overridden":
				raw = strings.Replace(raw, `,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]`, "", 1)
			case "unsupported":
				raw = strings.ReplaceAll(raw, "FREQ=WEEKLY", "FREQ=HOURLY")
			case "series budget":
				raw = strings.NewReplacer("2027-03-02", "1800-01-01", "FREQ=WEEKLY;COUNT=2", "FREQ=DAILY").Replace(raw)
			case "fractional master":
				raw = strings.ReplaceAll(raw, "17:00:00Z", "17:00:00.5Z")
			}
			if tc.state == "series budget" || tc.state == "request budget" {
				raw = strings.Replace(raw, `"start":{"dateTime":`, `"start":{"timeZone":"America/New_York","dateTime":`, 1)
			}
			_, master := calendarFixture(t, st, "master", raw)
			if tc.state == "single damaged" {
				calendarFixture(t, st, "damaged", `{`)
			}
			exception := int64(-1)
			original := `{"dateTime":"2027-03-09T17:00:00Z"}`
			if tc.state == "single overridden" {
				original = `{"dateTime":"2027-03-02T17:00:00Z"}`
			}
			if tc.state == "fractional original" || tc.state == "unresolvable" {
				original = `{"dateTime":"2027-03-09T17:00:00.5Z"}`
			}
			if tc.state == "retyped" {
				original = `{"date":"2027-03-03"}`
			}
			if tc.fields != "" {
				exceptionRaw := `{"id":"exception","recurringEventId":"weekly","originalStartTime":` + original + `,` + tc.fields + `}`
				_, exception = calendarFixture(t, st, "exception", exceptionRaw)
			}
			if tc.name == "sparse cancellation under unsupported master" {
				_, err := st.db.Exec(`UPDATE messages SET subject = '' WHERE id = ?`, exception)
				requirements.NoError(err)
			}
			switch tc.state {
			case "retyped", "retyped original":
				retyped := `{"id":"weekly","location":"Example room","description":"Agenda","hangoutLink":"https://meet.example.com/master","start":{"date":"2027-03-01","timeZone":"America/New_York"},"end":{"date":"2027-03-02","timeZone":"America/New_York"},"recurrence":["RRULE:FREQ=DAILY;COUNT=1"]}`
				requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(retyped), "gcal_json"))
			case "archive-only":
				_, err := st.db.Exec(`INSERT INTO source_merge_archive_only_messages (message_id) VALUES (?)`, master)
				requirements.NoError(err)
			}
			if tc.state == "late master" {
				tc.orphan = true
			}
			if tc.metadataCancelled {
				// Sync preserves the archived raw snapshot when a cancellation arrives.
				requirements.NoError(st.SetMessageMetadata(exception, sql.NullString{String: `{"calendar_id":"calendar@example.com","status":"cancelled"}`, Valid: true}))
			}
			if tc.missingRaw {
				_, err := st.db.Exec(`DELETE FROM message_raw WHERE message_id = ?`, exception)
				requirements.NoError(err)
			}
			if tc.orphan {
				_, err := st.db.Exec(`DELETE FROM calendar_events WHERE message_id = ?`, master)
				requirements.NoError(err)
			}
			if tc.zero != "" {
				target := master
				if tc.zero == "exception" {
					target = exception
				}
				raw, err := st.GetMessageRawContext(t.Context(), target)
				requirements.NoError(err)
				override := ""
				if st.IsPostgreSQL() {
					override = " OVERRIDING SYSTEM VALUE"
				}
				_, err = st.db.Exec(`INSERT INTO messages (id, source_id, conversation_id, source_message_id, message_type, subject)`+override+` SELECT 0,source_id,conversation_id,'zero-copy',message_type,subject FROM messages WHERE id = ?`, target)
				requirements.NoError(err)
				_, err = st.db.Exec(`DELETE FROM calendar_events WHERE message_id = ?`, target)
				requirements.NoError(err)
				requirements.NoError(st.UpsertMessageRawWithFormat(0, raw, "gcal_json"))
			}
			if tc.zero == "exception" {
				exception = 0
			}
			from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), CalendarOccurrenceQuery{From: from, To: from.AddDate(0, 1, 0)})
			requirements.NoError(err)
			if tc.zero == "master" {
				master = 0
			}
			requirements.Len(page.Occurrences, tc.count)
			if tc.state == "single overridden" {
				_, err := st.GetCalendarOccurrenceContext(t.Context(), fmt.Sprintf("evt:%d", master))
				requirements.NoError(err)
			}
			if tc.state == "unsupported" || tc.state == "series budget" {
				requirements.Len(page.Unexpandable, 1)
				assertions.Equal(master, page.Unexpandable[0].MessageID)
				reason := "unsupported recurrence"
				if tc.state == "series budget" {
					reason = "recurrence generation budget exceeded"
				}
				assertions.Equal(reason, page.Unexpandable[0].Reason)
			}
			for _, unsupported := range page.Unexpandable {
				if unsupported.MessageID == exception || tc.state == "missing bounds" {
					_, err := st.GetCalendarOccurrenceContext(t.Context(), fmt.Sprintf("evt:%d", unsupported.MessageID))
					requirements.ErrorIs(err, ErrCalendarOccurrenceInvalid)
					assertions.Contains(err.Error(), unsupported.Reason)
				}
			}
			_, err = st.GetCalendarOccurrenceContext(t.Context(), fmt.Sprintf("evt:%d@20270330T170000Z", master))
			requirements.Error(err)
			if tc.state == "unsupported" {
				requirements.ErrorIs(err, calrecur.ErrUnsupportedRecurrence)
			}
			if tc.state == "unresolvable" {
				assertions.Equal([]CalendarUnexpandable{{MessageID: exception, Reason: "missing series master and event bounds"}}, page.Unexpandable)
				assertions.Equal(fmt.Sprintf("evt:%d@20270302T170000Z", master), page.Occurrences[0].Ref)
				assertions.Equal(fmt.Sprintf("evt:%d@20270309T170000Z", master), page.Occurrences[1].Ref)
			}
			if tc.state == "fractional original" || tc.state == "fractional master" {
				type occurrence struct{ Ref, Start, End string }
				var got []occurrence
				for _, listed := range page.Occurrences {
					got = append(got, occurrence{listed.Ref, listed.Start.Format(time.RFC3339Nano), listed.End.Format(time.RFC3339Nano)})
				}
				want := []occurrence{{fmt.Sprintf("evt:%d", exception), "2027-03-12T19:00:00Z", "2027-03-12T20:00:00Z"}}
				if tc.state == "fractional original" {
					want = []occurrence{
						{fmt.Sprintf("evt:%d@20270302T170000Z", master), "2027-03-02T17:00:00Z", "2027-03-02T18:00:00Z"},
						{fmt.Sprintf("evt:%d@20270309T170000Z", master), "2027-03-09T17:00:00Z", "2027-03-09T18:00:00Z"},
						{fmt.Sprintf("evt:%d", exception), "2027-03-12T19:00:00Z", "2027-03-12T20:00:00Z"},
					}
				}
				assertions.Equal(want, got)
			}
			var listedIDs []int64
			for _, listed := range page.Occurrences {
				listedIDs = append(listedIDs, listed.MessageID)
				detail, err := st.GetCalendarOccurrenceContext(t.Context(), listed.Ref)
				requirements.NoError(err)
				assertions.Equal(listed, detail.CalendarOccurrence)
				if tc.state == "single damaged" {
					assertions.Zero(listed.SeriesMessageID)
					assertions.Zero(detail.SeriesMessageID)
				}
				if listed.MessageID == exception {
					assertions.Equal(fmt.Sprintf("evt:%d", exception), listed.Ref)
					if tc.name == "sparse cancellation under unsupported master" {
						assertions.Equal(master, listed.SeriesMessageID)
						assertions.Equal("cancelled", listed.Status)
						assertions.Equal("2027-03-09T17:00:00Z", listed.Start.Format(time.RFC3339))
						assertions.Equal("2027-03-09T18:00:00Z", listed.End.Format(time.RFC3339))
						assertions.Equal("Example event", listed.Subject)
						assertions.Equal([]calevent.Attendee{{Email: "guest@example.com", Role: "required", RSVP: "accepted"}}, listed.Attendees)
					}
					if tc.name == "cancelled with bounds under bad master" {
						assertions.Equal(master, listed.SeriesMessageID)
						assertions.Equal("cancelled", listed.Status)
						assertions.Equal("2027-03-04T19:00:00Z", listed.Start.Format(time.RFC3339))
						assertions.Equal("2027-03-04T20:00:00Z", listed.End.Format(time.RFC3339))
					}
					if tc.state == "retyped original" || tc.state == "unsupported" {
						alias, err := st.GetCalendarOccurrenceContext(t.Context(), fmt.Sprintf("evt:%d@20270309T170000Z", master))
						requirements.NoError(err)
						assertions.Equal(detail, alias)
					}
					if tc.state == "series budget" {
						assertions.Equal("cancelled", listed.Status)
					}
					if tc.state == "series budget" || tc.state == "request budget" {
						assertions.Equal("2027-03-12T05:00:00Z", listed.Start.UTC().Format(time.RFC3339))
						assertions.Equal("2027-03-13T05:00:00Z", listed.End.UTC().Format(time.RFC3339))
					}
					if tc.state == "retyped" {
						assertions.Equal("2027-03-03T05:00:00Z", listed.Start.UTC().Format(time.RFC3339))
						assertions.Equal("2027-03-04T05:00:00Z", listed.End.UTC().Format(time.RFC3339))
					}
					if tc.state == "late master" {
						requirements.NoError(st.UpsertMessageRawWithFormat(master, []byte(raw), "gcal_json"))
						reopened, err := st.GetCalendarOccurrenceContext(t.Context(), listed.Ref)
						requirements.NoError(err)
						detail.SeriesMessageID = master
						assertions.Equal(detail, reopened)
					}
				}
				want := masterContent
				if listed.MessageID == exception {
					want = tc.content
					if tc.metadataCancelled {
						assertions.Equal("cancelled", detail.Status)
					}
				}
				assertions.Equal(want, detail.Content)
			}
			if tc.fields != "" && tc.count > 0 && tc.state != "unresolvable" {
				assertions.Contains(listedIDs, exception)
			}
		})
	}
}

func TestCalendarOccurrenceRefLookupErrors(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","location":"Example room","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), CalendarOccurrenceQuery{From: from, To: from.AddDate(0, 1, 0)})
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	ref := page.Occurrences[0].Ref
	_, err = st.GetCalendarOccurrenceContext(t.Context(), fmt.Sprintf("evt:%d", master))
	requirements.ErrorIs(err, ErrCalendarOccurrenceNotFound)
	_, err = st.GetCalendarOccurrenceContext(sourcescope.WithSourceIDs(t.Context(), nil), ref)
	requirements.ErrorIs(err, ErrCalendarOccurrenceNotFound)
	_, err = st.GetCalendarOccurrenceContext(t.Context(), fmt.Sprintf("evt:%d@20270303", master))
	requirements.ErrorIs(err, ErrCalendarOccurrenceNotFound)
	for _, ref := range []string{"evt:bad", fmt.Sprintf("evt:%d@20270302T170000.5Z", master)} {
		_, err = st.GetCalendarOccurrenceContext(t.Context(), ref)
		requirements.ErrorIs(err, ErrCalendarOccurrenceInvalid)
	}
	_, err = st.db.Exec(`DELETE FROM message_raw WHERE message_id = ?`, master)
	requirements.NoError(err)
	detail, err := st.GetCalendarOccurrenceContext(t.Context(), ref)
	requirements.NoError(err)
	assertions.Equal(page.Occurrences[0], detail.CalendarOccurrence)
	assertions.Empty(detail.Content)
}

func TestCalendarOccurrenceExcludedPullBudget(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, single := calendarFixture(t, st, "seed", calendarRaw)
	exclusions := make([]string, 1000)
	start := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	for i := range exclusions {
		exclusions[i] = start.AddDate(0, 0, i).Format("20060102T150405Z")
	}
	recurrence := "EXDATE:" + strings.Join(exclusions, ",")
	var seriesIDs []int64
	for i := range 700 {
		_, id := calendarFixture(t, st, fmt.Sprintf("excluded-%d", i), fmt.Sprintf(`{"id":"excluded-%d","start":{"dateTime":"2025-01-01T10:00:00Z"},"end":{"dateTime":"2025-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=1000",%q]}`, i, recurrence))
		seriesIDs = append(seriesIDs, id)
	}

	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	query := CalendarOccurrenceQuery{From: from, To: from.AddDate(0, 1, 0)}
	source, err := st.GetOrCreateSource("gcal", "authorized@example.com")
	requirements.NoError(err)
	seed := &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: "authorized", MessageType: "calendar_event"},
		Conversation: &ConversationPersistData{SourceConversationID: "authorized", ConversationType: "calendar"},
		RawMIME:      []byte(calendarRaw), RawFormat: "gcal_json",
	}
	id, err := st.PersistMessage(seed)
	requirements.NoError(err)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	assertions.ElementsMatch([]int64{single, id}, []int64{page.Occurrences[0].MessageID, page.Occurrences[1].MessageID})
	// Each series costs 822 steps, including excluded candidates and DTSTART.
	// After reading 702 rows, 607 series fit in the remaining request budget.
	var expected []CalendarUnexpandable
	for _, seriesID := range seriesIDs[607:] {
		expected = append(expected, CalendarUnexpandable{MessageID: seriesID, Reason: "request work limit reached; long-running series count their steps from their start"})
	}
	assertions.Equal(expected, page.Unexpandable)
	assertions.Empty(page.NextCursor)
	scoped, err := st.ListCalendarOccurrencesContext(sourcescope.WithSourceIDs(t.Context(), []int64{source.ID}), query)
	requirements.NoError(err)
	requirements.Len(scoped.Occurrences, 1)
	assertions.Equal(id, scoped.Occurrences[0].MessageID)
	assertions.Empty(scoped.Unexpandable)
}

func TestCalendarOccurrenceHiddenSourcesDoNotConsumeBudget(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, _ := calendarFixture(t, st, "seed", calendarRaw)
	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	query := CalendarOccurrenceQuery{From: from, To: from.AddDate(0, 1, 0)}
	// Sources outside the grant are removed before breadth probes and expansion.
	bulkCalendarCandidates(t, st, data.Message.SourceID, data.Message.ConversationID, "series", maxCalendarSeries+1)
	hidden := sourcescope.WithSourceIDs(t.Context(), []int64{data.Message.SourceID + 1})
	page, err := st.ListCalendarOccurrencesContext(hidden, query)
	requirements.NoError(err)
	assert.Empty(t, page.Occurrences)
}

func TestCalendarOccurrenceInvalidQueries(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	from := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, q := range []CalendarOccurrenceQuery{{From: from, To: from}, {From: from, To: from.AddDate(2, 0, 0)}, {From: from, To: from.AddDate(0, 1, 0), Limit: new(-1)}, {From: from, To: from.AddDate(0, 1, 0), Limit: new(501)}} {
		_, err := st.ListCalendarOccurrencesContext(t.Context(), q)
		requirements.ErrorIs(err, ErrCalendarOccurrenceInvalid)
	}
}

func TestListCalendarOccurrencesClearedSeriesKeepsFractionalNeighbor(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, standalone := calendarFixture(t, st, "standalone", `{"id":"series-example","start":{"dateTime":"2027-03-09T17:00:00Z"},"end":{"dateTime":"2027-03-09T18:00:00Z"}}`)
	calendarFixture(t, st, "exception", `{"id":"exception-example","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00.5Z"},"start":{"dateTime":"2027-04-02T17:00:00Z"},"end":{"dateTime":"2027-04-02T18:00:00Z"}}`)
	from := time.Date(2027, 3, 9, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), CalendarOccurrenceQuery{From: from, To: from.AddDate(0, 0, 1)})
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	requirements.Equal(standalone, page.Occurrences[0].MessageID)
}
