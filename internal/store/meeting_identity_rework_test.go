package store

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingIdentityFractionalAliasReverseDiscovery(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"old","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00.123456Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`
	_, old := calendarFixture(t, st, "old", raw)
	_, note := projectionFixture(t, st, "undated", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), note, OccurrenceRef{MessageID: old}))
	_, latest := calendarFixture(t, st, "new", strings.ReplaceAll(raw, `"old"`, `"new"`))
	_, other := calendarFixture(t, st, "other", strings.ReplaceAll(strings.ReplaceAll(raw, `"old"`, `"other"`), ".123456Z", ".654321Z"))
	_, otherNote := projectionFixture(t, st, "other-note", "plaud_json", `{}`)
	require.NoError(st.LinkMeetingContext(t.Context(), otherNote, OccurrenceRef{MessageID: other}))
	from := time.Date(2027, 3, 14, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	require.NoError(err)
	refs := []string{}
	for _, o := range page.Occurrences {
		refs = append(refs, o.Ref)
	}
	require.ElementsMatch([]string{identityRef(latest), identityRef(other)}, refs)
	forward, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	require.NoError(err)
	require.Equal(identityRef(latest), forward.ByTranscript[note].OccurrenceRef)
	reverse, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: refs})
	require.NoError(err)
	assert.Equal([]int64{note}, reverse.ByOccurrence[identityRef(latest)].LinkedTranscriptIDs)
	assert.Equal([]int64{otherNote}, reverse.ByOccurrence[identityRef(other)].LinkedTranscriptIDs)
}

func TestMeetingIdentityBatchReadBound(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	req := MeetingIdentityRequest{}
	for i := range 20 {
		_, id := calendarFixture(t, st, fmt.Sprintf("event-%d", i), singleLinkRaw(fmt.Sprintf("event-%d", i), fmt.Sprintf("event-%d@example.org", i)))
		_, note := identityNote(t, st, fmt.Sprintf("note-%d", i), fmt.Sprintf("event-%d@example.org", i))
		req.OccurrenceRefs = append(req.OccurrenceRefs, identityRef(id))
		req.TranscriptIDs = append(req.TranscriptIDs, note)
	}
	for _, mode := range []string{"both", "events", "notes"} {
		t.Run(mode, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			request := req
			if mode == "events" {
				request.TranscriptIDs = nil
			}
			if mode == "notes" {
				request.OccurrenceRefs = nil
			}
			reads := 0
			observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, _ string, _ []driver.NamedValue) error { reads++; return nil }})
			got, err := observed.ResolveMeetingIdentitiesContext(t.Context(), request)
			require.NoError(err)
			require.Len(got.ByTranscript, 20)
			for i, ref := range req.OccurrenceRefs {
				assert.Equal([]int64{req.TranscriptIDs[i]}, got.ByOccurrence[ref].LinkedTranscriptIDs)
			}
			assert.LessOrEqual(reads, 35)
			t.Logf("20 linked events: %d reads", reads)
		})
	}
}

func TestMeetingIdentityRecurringUIDReadBound(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "series", linkSeriesRaw)
	_, note := identityNote(t, st, "matching", "weekly@example.org")
	for i := range 220 {
		raw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":%q}}`, time.Date(2028, 1, 1, 10, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format(time.RFC3339))
		projectionFixture(t, st, fmt.Sprintf("other-%d", i), "twenty_json", raw)
	}
	reads := 0
	observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, _ string, _ []driver.NamedValue) error { reads++; return nil }})
	ref := identityRef(series) + "@20270314T100000Z"
	got, err := observed.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
	require.NoError(err)
	assert.Equal([]int64{note}, got.ByOccurrence[ref].LinkedTranscriptIDs)
	assert.Len(got.ByTranscript, 1)
	assert.LessOrEqual(reads, 45)
	t.Logf("one occurrence with 220 unrelated UID notes: %d reads", reads)
}

func TestMeetingIdentityUnknownICSVersionStaysSeparate(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	_, err := st.PersistICalendarContext(t.Context(), source, calendarDocument("REQUEST", "DTSTART:20270314T100000Z\r\nDURATION:PT1H"))
	require.NoError(err)
	events := emailEvents(t, st, source)
	require.Len(events, 1)
	_, note := identityNote(t, st, "note", "family@example.com")
	_, err = st.db.Exec(`UPDATE messages SET metadata=? WHERE id=?`, `{"ics_version":999}`, events[0])
	require.NoError(err)
	got, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{identityRef(events[0])}, TranscriptIDs: []int64{note}})
	require.NoError(err)
	assert.Empty(got.ByTranscript[note].ExactOccurrenceRefs)
	assert.Empty(got.ByOccurrence[identityRef(events[0])].LinkedTranscriptIDs)
}

func TestCalendarRawUpsertAdvancesObservationOnce(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "event", calendarRaw)
	var before, after, eventOrder int64
	require.NoError(st.db.QueryRow(`SELECT observation_order FROM calendar_event_observation_clock WHERE singleton=1`).Scan(&before))
	require.NoError(st.UpsertMessageRawWithFormatContext(t.Context(), event, []byte(calendarRaw), "gcal_json"))
	require.NoError(st.db.QueryRow(`SELECT observation_order FROM calendar_event_observation_clock WHERE singleton=1`).Scan(&after))
	require.NoError(st.db.QueryRow(`SELECT provider_observation_order FROM calendar_events WHERE message_id=?`, event).Scan(&eventOrder))
	assert.Equal(before+1, after)
	assert.Equal(after, eventOrder)
}
