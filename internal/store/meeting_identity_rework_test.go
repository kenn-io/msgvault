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

func TestMeetingIdentityMovedCopyReverseDiscovery(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, original := calendarFixture(t, st, "original", linkSeriesRaw)
	_, moved := calendarFixture(t, st, "moved", linkSeriesRaw, "other@example.org")
	calendarFixture(t, st, "exception", `{"id":"exception","iCalUID":"weekly@example.org","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`, "other@example.org")
	_, note := projectionFixture(t, st, "moved-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"weekly@example.org","startsAt":"2027-03-14T12:00:00Z"}}`)
	forward, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{TranscriptIDs: []int64{note}})
	require.NoError(t, err)
	require.Equal(t, identityRef(original)+"@20270314T100000Z", forward.ByTranscript[note].OccurrenceRef)
	for _, id := range []int64{original, moved} {
		ref := identityRef(id) + "@20270314T100000Z"
		reverse, err := st.ResolveMeetingIdentitiesContext(t.Context(), MeetingIdentityRequest{OccurrenceRefs: []string{ref}})
		require.NoError(t, err)
		assert.Equal(t, []int64{note}, reverse.ByOccurrence[ref].LinkedTranscriptIDs)
	}
}

func TestMeetingIdentityGranolaSourcesReadOnce(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	req := MeetingIdentityRequest{}
	for i := range 20 {
		key := fmt.Sprintf("event-%d", i)
		_, event := calendarFixture(t, st, key, singleLinkRaw(key, key+"@example.org"))
		_, note := projectionFixture(t, st, fmt.Sprintf("note-%d", i), "granola_json", fmt.Sprintf(`{"owner":{"email":"calendar@example.com"},"calendar_event":{"calendar_event_id":%q}}`, key))
		req.OccurrenceRefs = append(req.OccurrenceRefs, identityRef(event))
		req.TranscriptIDs = append(req.TranscriptIDs, note)
	}
	for _, mode := range []string{"both", "events", "notes"} {
		t.Run(mode, func(t *testing.T) {
			request := req
			if mode == "events" {
				request.TranscriptIDs = nil
			}
			if mode == "notes" {
				request.OccurrenceRefs = nil
			}
			sourceReads := 0
			observed := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, query string, _ []driver.NamedValue) error {
				if strings.Contains(query, "SELECT id,sync_config FROM sources") {
					sourceReads++
				}
				return nil
			}})
			got, err := observed.ResolveMeetingIdentitiesContext(t.Context(), request)
			require.NoError(t, err)
			for i, ref := range req.OccurrenceRefs {
				assert.Equal(t, []int64{req.TranscriptIDs[i]}, got.ByOccurrence[ref].LinkedTranscriptIDs)
			}
			assert.Equal(t, 1, sourceReads)
		})
	}
}

func TestAddressParticipantLookupKeepsQueryLogging(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	id, err := st.EnsureParticipant("new@example.org", "New", "example.org")
	require.NoError(t, err)
	_, err = st.db.Exec(`UPDATE participants SET phone_number=? WHERE id=?`, "+12025550123", id)
	require.NoError(t, err)
	ConfigureSQLLogging(SQLLogOptions{FullTrace: true})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })
	logs := captureSlog(t)
	ctx := WithRequestID(t.Context(), "participant-lookup")
	got, err := st.EmailParticipantContext(ctx, "new@example.org")
	require.NoError(t, err)
	assert.Equal(t, id, got)
	got, err = st.PhoneParticipantContext(ctx, "+12025550123")
	require.NoError(t, err)
	assert.Equal(t, id, got)
	assert.Equal(t, 2, strings.Count(logs.String(), `"request_id":"participant-lookup"`))
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
