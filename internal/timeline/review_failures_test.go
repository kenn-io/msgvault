package timeline

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// Inject only unavailable archive reads; the real composer, store, fitting and
// rendering still determine the observable partial response.
type anchorReadFailure struct {
	ActivitySource

	latest, bursts, display error
	failAfter               int
	burstReads              *int
}

func (s anchorReadFailure) LatestEligibleMessageContext(ctx context.Context, id int64) (int64, error) {
	if s.latest != nil {
		return 0, s.latest
	}
	return s.ActivitySource.LatestEligibleMessageContext(ctx, id)
}
func (s anchorReadFailure) ListActivityBurstsContext(ctx context.Context, q store.BurstQuery) (store.BurstPage, error) {
	if s.burstReads != nil {
		*s.burstReads++
	}
	if s.bursts != nil && (s.burstReads == nil || *s.burstReads > s.failAfter) {
		return store.BurstPage{}, s.bursts
	}
	return s.ActivitySource.ListActivityBurstsContext(ctx, q)
}
func (s anchorReadFailure) MessageDisplayContext(ctx context.Context, ids []int64) (map[int64]store.MessageDisplay, error) {
	if s.display != nil {
		return nil, s.display
	}
	return s.ActivitySource.MessageDisplayContext(ctx, ids)
}

type anchorCalendarFailure struct {
	CalendarSource

	err error
}

func (s anchorCalendarFailure) GetCalendarOccurrenceContext(context.Context, string) (store.CalendarOccurrenceDetail, error) {
	return store.CalendarOccurrenceDetail{}, s.err
}

func TestAnchorReadFailuresPreservePartialEnvelope(t *testing.T) {
	for _, kind := range []string{"calendar timeout", "calendar canceled", "calendar unavailable", "conversation", "burst", "display"} {
		t.Run(kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newTimelineFixture(t)
			id := f.add(f.now, false)
			f.project()
			ref := messageRef(id)
			gap := Gap{Section: "anchor", Code: "unavailable"}
			failure := errors.New("archive read unavailable")
			switch kind {
			case "calendar timeout", "calendar canceled", "calendar unavailable":
				event := f.calendar("review-anchor", singleDayEvent)
				ref = fmt.Sprintf("evt:%d", event)
				_, err := f.src.Calendar.GetCalendarOccurrenceContext(t.Context(), ref)
				requirements.NoError(err)
				switch kind {
				case "calendar timeout":
					failure = context.DeadlineExceeded
					gap.Code, gap.Retryable = "timeout", true
				case "calendar canceled":
					failure = context.Canceled
					gap.Code, gap.Retryable = "timeout", true
				}
				f.src.Calendar = anchorCalendarFailure{CalendarSource: f.src.Calendar, err: failure}
			case "conversation":
				ref = fmt.Sprintf("conv:%d", f.ConvID)
				f.src.Activity = anchorReadFailure{ActivitySource: f.src.Activity, latest: failure}
			case "burst":
				ref = fmt.Sprintf("burst:%d:%d:%s", f.Source.ID, f.ConvID, f.now.Format(time.DateOnly))
				f.src.Activity = anchorReadFailure{ActivitySource: f.src.Activity, bursts: failure}
			case "display":
				f.src.Activity = anchorReadFailure{ActivitySource: f.src.Activity, display: failure}
			}
			got, err := Build(t.Context(), f.src, anchorRequest(ref))
			requirements.NoError(err)
			requirements.NotNil(got)
			assertions.Nil(got.Anchor, "failed resolution must not invent a resolved anchor")
			assertions.Empty(got.History)
			assertions.Equal(f.now, got.Now)
			assertions.Contains(got.Gaps, gap)
		})
	}
}

func TestAnchorSuccessfulMissingReferenceRemainsInputError(t *testing.T) {
	for _, ref := range []string{"msg:999999", "conv:999999", "evt:999999", "burst:999999:999999:2026-10-22"} {
		t.Run(ref, func(t *testing.T) {
			f := newTimelineFixture(t)
			got, err := Build(t.Context(), f.src, anchorRequest(ref))
			require.ErrorIs(t, err, ErrUnknownReference)
			assert.Nil(t, got)
		})
	}
}

func TestOlderSummarySameNamePeopleHaveStableIdentityOrder(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	view := &Timeline{Mode: ModeDay, Timezone: "UTC", Now: now, Window: Window{From: "2026-10-19", To: "2026-10-22"}, Omitted: Omitted{Budget: map[string]any{}}}
	c := composer{t: view, loc: time.UTC, keys: map[string]string{}, positions: map[string]string{}, entryDates: map[string]string{}}
	entries := []Entry{}
	for id := int64(10); id > 0; id-- {
		entries = append(entries, Entry{Kind: entryKindBurst, Key: fmt.Sprintf("burst-%d", id), Time: now.AddDate(0, 0, -3), Burst: &BurstEntry{People: []Person{{Name: "Same", PersonID: id, ParticipantID: 100 + id, Tier: int(id%2) + 1, TierSource: "observed"}}}})
	}
	c.groupDays(entries)

	assertions := assert.New(t)
	requirements := require.New(t)
	fitted, err := Fit(view, "json", 65536)
	requirements.NoError(err)
	requirements.Len(fitted.Days, 1)
	summary := fitted.Days[0].Summary
	requirements.NotNil(summary)
	ids := []int64{}
	for _, person := range summary.People {
		ids = append(ids, person.PersonID)
	}
	assertions.Equal([]int64{1, 2, 3, 4, 5, 6, 7, 8}, ids)
	assertions.Equal(10, summary.PeopleCount)
	assertions.True(summary.PeopleTruncated)
	encoded, err := json.Marshal(fitted)
	requirements.NoError(err)
	var decoded Timeline
	requirements.NoError(json.Unmarshal(encoded, &decoded))
	wireIDs := []int64{}
	for _, person := range decoded.Days[0].Summary.People {
		wireIDs = append(wireIDs, person.PersonID)
	}
	assertions.Equal([]int64{1, 2, 3, 4, 5, 6, 7, 8}, wireIDs)
	markdown, err := Fit(view, "markdown", 65536)
	requirements.NoError(err)
	assertions.Equal(fitted.Days, markdown.Days)
	assertions.Contains(Markdown(markdown), "Same T2, Same T1, Same T2, Same T1, Same T2, Same T1, Same T2, Same T1, +2")
}

func TestAnchorWiderProbeFailureKeepsNeighbours(t *testing.T) {
	f := newTimelineFixture(t)
	anchor := f.add(f.now, false)
	f.add(f.now.Add(-time.Hour), false)
	f.project()
	reads := 0
	f.src.Activity = anchorReadFailure{ActivitySource: f.src.Activity, bursts: errors.New("archive read unavailable"), failAfter: 1, burstReads: &reads}
	got, err := Build(t.Context(), f.src, anchorRequest(messageRef(anchor)))
	require.NoError(t, err)
	assert.Equal(t, 2, reads)
	require.Len(t, got.History, 1)
	require.NotNil(t, got.History[0].Burst)
	assert.Equal(t, 2, got.History[0].Burst.Inbound)
	assert.Contains(t, got.Gaps, Gap{Section: "history", Code: "unavailable"})
}
