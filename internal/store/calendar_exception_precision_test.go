package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestCalendarFractionalOriginalKeepsWholeSecondCancellation(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "master", `{"id":"weekly","start":{"dateTime":"2027-03-02T17:00:00Z"},"end":{"dateTime":"2027-03-02T18:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`)
	_, cancelled := calendarFixture(t, st, "cancelled", `{"id":"cancelled","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00Z"},"status":"cancelled"}`)
	_, fractional := calendarFixture(t, st, "fractional", `{"id":"fractional","recurringEventId":"weekly","originalStartTime":{"dateTime":"2027-03-09T17:00:00.123456Z"},"start":{"dateTime":"2027-03-09T19:00:00Z"},"end":{"dateTime":"2027-03-09T20:00:00Z"}}`)
	from := time.Date(2027, 3, 9, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 2)
	assertions.Equal(cancelled, page.Occurrences[0].MessageID)
	assertions.Equal("cancelled", page.Occurrences[0].Status)
	assertions.Equal(identityRef(master)+"@20270309T170000Z", page.Occurrences[0].Ref)
	assertions.Equal(fractional, page.Occurrences[1].MessageID)
	assertions.Equal("confirmed", page.Occurrences[1].Status)
	assertions.Equal(identityRef(fractional), page.Occurrences[1].Ref)
	assertions.Equal(from.Add(19*time.Hour), page.Occurrences[1].Start)
	assertions.Equal(from.Add(20*time.Hour), page.Occurrences[1].End)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	rows, err := meetingEventRows(ctx, st.meetingReader(ctx), "e.message_id=?", []any{master}, &budget)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	expanded, err := expandMeetingSeries(ctx, st.meetingReader(ctx), rows[0], from, from.AddDate(0, 0, 1), &budget)
	requirements.NoError(err)
	byRef := map[string]linkOccurrence{}
	for _, occurrence := range expanded {
		byRef[formatMeetingOccurrence(occurrence.Ref)] = occurrence
	}
	cancelledOccurrence := byRef[identityRef(master)+"@20270309T170000Z"]
	assertions.Equal(cancelled, cancelledOccurrence.Row.MessageID)
	assertions.Equal("cancelled", cancelledOccurrence.Row.Event.Status)
	fractionalOccurrence := byRef[identityRef(fractional)]
	assertions.Equal(meetingIdentity{EventMessageID: fractional, OccurrenceKey: "20270309T170000Z"}, fractionalOccurrence.Identity)
	assertions.Equal(from.Add(19*time.Hour), fractionalOccurrence.Start)
	assertions.Equal(from.Add(20*time.Hour), fractionalOccurrence.End)
}

func TestCalendarExceptionAliasQueryKeepsOriginalPrecision(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := func(id, series, fraction, hour string) string {
		return fmt.Sprintf(`{"id":%q,"recurringEventId":%q,"originalStartTime":{"dateTime":"2027-03-09T17:00:00%sZ"},"start":{"dateTime":"2027-03-09T%s:00:00Z"},"end":{"dateTime":"2027-03-09T23:00:00Z"}}`, id, series, fraction, hour)
	}
	_, whole := calendarFixture(t, st, "whole", raw("whole", "weekly", "", "17"))
	_, wholeAlias := calendarFixture(t, st, "whole-alias", raw("whole-alias", "weekly", "", "18"))
	_, fraction := calendarFixture(t, st, "fraction", raw("fraction", "weekly", ".123456", "19"))
	_, fractionAlias := calendarFixture(t, st, "fraction-alias", raw("fraction-alias", "weekly", ".123456", "20"))
	_, otherSeries := calendarFixture(t, st, "other-series", raw("other-series", "other-weekly", "", "21"))
	_, otherSource := calendarFixture(t, st, "other-source", raw("other-source", "weekly", ".123456", "21"), "other@example.com")
	seeds := calendarRows(t, st, whole, fraction)
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	aliases, err := queryCalendarExceptionAliases(ctx, st.meetingReader(ctx), seeds, 20)
	requirements.NoError(err)
	var ids []int64
	for _, row := range aliases {
		ids = append(ids, row.MessageID)
	}
	assertions.ElementsMatch([]int64{whole, wholeAlias, fraction, fractionAlias}, ids)
	assertions.NotContains(ids, otherSeries)
	assertions.NotContains(ids, otherSource)
}

func TestMeetingExceptionAliasesKeepOriginalPrecisionAndBoundaries(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	var ids []int64
	for _, tc := range []struct{ key, series, original, source string }{
		{"whole", "weekly", "2027-03-09T17:00:00Z", "calendar@example.com"},
		{"fraction", "weekly", "2027-03-09T17:00:00.123456Z", "calendar@example.com"},
		{"nearby-fraction", "weekly", "2027-03-09T17:00:00.123457Z", "calendar@example.com"},
		{"other-series", "other-weekly", "2027-03-09T17:00:00Z", "calendar@example.com"},
		{"other-source", "weekly", "2027-03-09T17:00:00Z", "other@example.com"},
	} {
		raw := fmt.Sprintf(`{"id":%q,"recurringEventId":%q,"originalStartTime":{"dateTime":%q},"start":{"dateTime":"2027-03-09T19:00:00Z"},"end":{"dateTime":"2027-03-09T20:00:00Z"}}`, tc.key, tc.series, tc.original)
		_, id := calendarFixture(t, st, tc.key, raw, tc.source)
		ids = append(ids, id)
	}
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	budget := calrecur.RequestReadBudget
	var occurrences []linkOccurrence
	for _, id := range ids {
		occurrence, err := resolveMeetingOccurrence(ctx, st.meetingReader(ctx), OccurrenceRef{MessageID: id}, &budget)
		requirements.NoError(err)
		occurrences = append(occurrences, occurrence)
	}
	unique := uniqueMeetingOccurrences(occurrences)
	var got []int64
	for _, occurrence := range unique {
		got = append(got, occurrence.Ref.MessageID)
		assertions.Empty(occurrence.Ref.Key, "orphan exceptions retain message references")
	}
	assertions.ElementsMatch(ids, got)
}
