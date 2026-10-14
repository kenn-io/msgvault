package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
)

func TestMeetingLinksResolveMovedExceptionEffectiveType(t *testing.T) {
	for _, tc := range []struct{ name, master, original, bounds, key, zone, start, end string }{
		{
			"all-day-to-timed", `"start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"}`, `"date":"2027-03-14"`,
			`"start":{"dateTime":"2027-06-01T09:00:00Z"},"end":{"dateTime":"2027-06-01T10:00:00Z"}`,
			"20270314", "UTC", "2027-06-01T09:00:00Z", "2027-06-01T10:00:00Z",
		},
		{
			"timed-to-all-day-UTC", `"start":{"dateTime":"2027-03-07T09:00:00-08:00","timeZone":"America/Los_Angeles"},"end":{"dateTime":"2027-03-07T10:00:00-08:00"}`, `"dateTime":"2027-03-14T16:00:00Z"`,
			`"start":{"date":"2027-06-01"},"end":{"date":"2027-06-02"}`,
			"20270314T160000Z", "UTC", "2027-06-01T00:00:00Z", "2027-06-02T00:00:00Z",
		},
		{
			"timed-to-all-day-Honolulu", `"start":{"dateTime":"2027-03-07T09:00:00-08:00","timeZone":"America/Los_Angeles"},"end":{"dateTime":"2027-03-07T10:00:00-08:00"}`, `"dateTime":"2027-03-14T16:00:00Z"`,
			`"start":{"date":"2027-06-01"},"end":{"date":"2027-06-02"}`,
			"20270314T160000Z", "Pacific/Honolulu", "2027-06-01T10:00:00Z", "2027-06-02T10:00:00Z",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			master, source := calendarLinkFixture(t, st, "primary@example.com", fmt.Sprintf(`{"id":"series","iCalUID":"converted@example.org",%s,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`, tc.master))
			requirements.NoError(st.UpdateSourceSyncConfig(source, fmt.Sprintf(`{"time_zone":%q}`, tc.zone)))
			calendarLinkFixture(t, st, "primary@example.com", fmt.Sprintf(`{"id":"converted","recurringEventId":"series","iCalUID":"converted@example.org","originalStartTime":{%s},%s}`, tc.original, tc.bounds))
			ref := OccurrenceRef{MessageID: master, Key: tc.key}
			ctx, release, err := st.BeginReadSnapshotContext(t.Context())
			requirements.NoError(err)
			budget := calrecur.RequestReadBudget
			target, err := resolveLinkOccurrence(ctx, st.snapshotReader(ctx), OwnerScope(), ref, &budget)
			release()
			requirements.NoError(err)
			assertions.Equal(tc.key, target.Original.Key())
			assertions.Equal(tc.start, target.Start.UTC().Format(time.RFC3339))
			assertions.Equal(tc.end, target.End.UTC().Format(time.RFC3339))
			_, note := projectionFixture(t, st, "converted-note", "twenty_json", fmt.Sprintf(`{"calendar_event":{"iCalUid":"converted@example.org","startsAt":%q}}`, tc.start))
			assertions.Equal([]OccurrenceRef{ref}, linkTargets(memberLinks(t, st, OwnerScope(), note)))
			forward, err := st.SuggestMeetingLinksContext(t.Context(), OwnerScope(), fmt.Sprintf("msg:%d", note), 10)
			requirements.NoError(err)
			requirements.Len(forward, 1)
			assertions.Equal(FormatOccurrenceRef(ref), forward[0].Ref)
			reverse, err := st.SuggestMeetingLinksContext(t.Context(), OwnerScope(), FormatOccurrenceRef(ref), 10)
			requirements.NoError(err)
			requirements.Len(reverse, 1)
			assertions.Equal(note, reverse[0].MemberMessageID)
			if tc.key != "20270314" {
				ctx, release, err = st.BeginReadSnapshotContext(t.Context())
				requirements.NoError(err)
				budget = calrecur.RequestReadBudget
				sibling, siblingErr := resolveLinkOccurrence(ctx, st.snapshotReader(ctx), OwnerScope(), OccurrenceRef{MessageID: master, Key: "20270321T160000Z"}, &budget)
				release()
				requirements.NoError(siblingErr)
				assertions.Equal("2027-03-21T16:00:00Z", sibling.Start.UTC().Format(time.RFC3339), "timed recurrence retains the master's event timezone")
			}
		})
	}
}
