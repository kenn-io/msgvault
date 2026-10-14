package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func linkCandidateFixture(t *testing.T, st *Store, sourceKey, eventID, uid string) (int64, int64) {
	t.Helper()
	return calendarLinkFixture(t, st, sourceKey, singleLinkRaw(eventID, uid, "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
}

func TestExactInstanceCandidatesIgnoreUnrelatedExceptions(t *testing.T) {
	for _, allDay := range []bool{false, true} {
		t.Run(fmt.Sprintf("allDay=%t", allDay), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			masterRaw, key := linkSeriesRaw, "20270314T100000Z"
			original := `"dateTime":"2027-03-14T10:00:00Z"`
			bounds := `"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}`
			if allDay {
				masterRaw = `{"id":"series","start":{"date":"2027-03-07"},"end":{"date":"2027-03-08"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`
				key, original = "20270314", `"date":"2027-03-14"`
				bounds = `"start":{"date":"2027-03-15"},"end":{"date":"2027-03-16"}`
			}
			master, _ := calendarLinkFixture(t, st, "primary@example.com", masterRaw)
			for i := range 201 {
				unrelated := strings.Replace(original, "2027-03-14", "2027-03-07", 1)
				calendarLinkFixture(t, st, "primary@example.com", fmt.Sprintf(`{"id":"unrelated-%d","recurringEventId":"series","originalStartTime":{%s},"status":"cancelled"}`, i, unrelated))
			}
			exception, _ := calendarLinkFixture(t, st, "primary@example.com", fmt.Sprintf(`{"id":"opaque-move","recurringEventId":"series","originalStartTime":{%s},%s}`, original, bounds))
			note := granolaLinkFixture(t, st, "series_"+key)
			links := memberLinks(t, st, OwnerScope(), note)
			assertions.Equal([]OccurrenceRef{{MessageID: master, Key: key}}, linkTargets(links))
			ctx, release, err := st.BeginReadSnapshotContext(t.Context())
			requirements.NoError(err)
			defer release()
			rows, broad, err := linkKeyCandidates(ctx, st.snapshotReader(ctx), OwnerScope(), meetingLinkKey{Kind: "google_event_id", Value: "series_" + key, Base: "series", InstanceKey: key})
			requirements.NoError(err)
			assertions.False(broad)
			var ids []int64
			for _, row := range rows {
				ids = append(ids, row.MessageID)
			}
			assertions.ElementsMatch([]int64{master, exception}, ids)
			release()
			directNote := granolaLinkFixture(t, st, "opaque-move")
			assertions.Equal([]OccurrenceRef{{MessageID: master, Key: key}}, linkTargets(memberLinks(t, st, OwnerScope(), directNote)), "an exact opaque exception ID must not load unrelated occurrences")
		})
	}
}

func TestExactInstanceCandidatesRetainRelevantCap(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	for i := range 201 {
		calendarLinkFixture(t, st, fmt.Sprintf("copy-%d@example.org", i), linkSeriesRaw)
	}
	note := granolaLinkFixture(t, st, "series_20270314T100000Z")
	assert.Empty(t, memberLinks(t, st, OwnerScope(), note))
}

func TestMeetingLinkCandidatesScopeBeforeLimit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	for i := range 201 {
		linkCandidateFixture(t, st, fmt.Sprintf("hidden-%d@example.org", i), "shared", "shared@example.org")
	}
	id, source := linkCandidateFixture(t, st, "visible@example.com", "shared", "shared@example.org")
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	requirements.NoError(err)
	defer release()
	predicate, args := linkKeyPredicate(meetingLinkKey{Kind: "google_event_id", Value: "shared"})
	ids, broad, err := boundedLinkEventIDs(ctx, st.snapshotReader(ctx), SourceScope{IDs: []int64{source}}, predicate, args)
	requirements.NoError(err)
	assertions.False(broad)
	assertions.Equal([]int64{id}, ids)
	ids, broad, err = boundedLinkEventIDs(ctx, st.snapshotReader(ctx), SourceScope{}, predicate, args)
	requirements.NoError(err)
	assertions.False(broad)
	assertions.Empty(ids)
}
