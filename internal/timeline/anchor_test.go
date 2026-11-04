package timeline

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func anchorRequest(ref string) Request {
	return Request{Around: ref, Scope: Scope{Owner: true}, Format: "json", Budget: 65536}
}

type anchorBurstRead struct {
	query store.BurstQuery
	page  store.BurstPage
}

type anchorBurstSnapshot struct {
	ActivitySource

	reads      []anchorBurstRead
	restricted bool
	sourceIDs  []int64
}

func (s anchorBurstSnapshot) ListActivityBurstsContext(ctx context.Context, q store.BurstQuery) (store.BurstPage, error) {
	if sourcescope.Restricted(ctx) != s.restricted || !slices.Equal(sourcescope.IDs(ctx), s.sourceIDs) {
		return s.ActivitySource.ListActivityBurstsContext(ctx, q)
	}
	for _, read := range s.reads {
		if reflect.DeepEqual(read.query, q) {
			return read.page, nil
		}
	}
	return s.ActivitySource.ListActivityBurstsContext(ctx, q)
}

// These tests exercise repeated anchor probes and mutation visibility, rather
// than requiring six real SQL reads to finish within one request deadline.
// Capture the actual authorized reader for each Build; delegate every other
// method so display, latest-anchor and projection-state reads remain live.
func snapshotAnchorBurstReads(ctx context.Context, t *testing.T, src Sources, req Request, anchorID int64) Sources {
	t.Helper()
	require.Empty(t, req.With)
	require.False(t, req.Conversation)
	display, err := src.Activity.MessageDisplayContext(ctx, []int64{anchorID})
	require.NoError(t, err)
	require.NotNil(t, display[anchorID].OccurredAt)
	at := display[anchorID].OccurredAt.In(src.Location)
	filters := src.Filters
	filters.IncludeNoise = req.IncludeNoise
	snapshot := anchorBurstSnapshot{ActivitySource: src.Activity, restricted: sourcescope.Restricted(ctx), sourceIDs: slices.Clone(sourcescope.IDs(ctx))}
	for _, days := range []int{1, 2, 4, 8, 16, 31} {
		from := at.AddDate(0, 0, -days)
		to := at.AddDate(0, 0, days).Add(time.Nanosecond)
		q := store.BurstQuery{After: &from, Before: &to, Filters: filters, Limit: 5000}
		page, err := src.Activity.ListActivityBurstsContext(ctx, q)
		require.NoError(t, err)
		snapshot.reads = append(snapshot.reads, anchorBurstRead{query: q, page: page})
	}
	src.Activity = snapshot
	return src
}

func TestAnchorNeighboursChronologicalWithProbeGrowth(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	var anchor int64
	for _, day := range []int{-20, -8, -3, 0, 4, 9, 22} {
		id := f.add(f.now.AddDate(0, 0, day), false)
		if day == 0 {
			anchor = id
		}
	}
	f.project()
	req := anchorRequest(messageRef(anchor))
	req.Before, req.After = 3, 3
	src := snapshotAnchorBurstReads(t.Context(), t, f.src, req, anchor)
	got, err := Build(t.Context(), src, req)
	requirements.NoError(err)
	requirements.Len(got.History, 7)
	for i, e := range got.History {
		if i > 0 {
			assertions.True(e.Time.After(got.History[i-1].Time))
		}
	}
	assertions.True(got.History[3].Anchor)
}
func TestAnchorSlowFreshnessLeavesTimeForHistoryProbe(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	addInConversation := func(key string, at time.Time) int64 {
		conversationID, err := f.Store.EnsureConversation(f.Source.ID, key, "Synthetic")
		requirements.NoError(err)
		return f.add(at, false, func(message *store.Message) { message.ConversationID = conversationID })
	}
	addInConversation("anchor-before", f.now.Add(-time.Hour))
	anchorID := addInConversation("anchor-center", f.now)
	addInConversation("anchor-after", f.now.Add(time.Hour))
	f.project()
	f.src.Inventory = blockingInventory{}
	req := anchorRequest(messageRef(anchorID))
	req.Before, req.After = 1, 1

	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.History, 3)
	assertions.True(got.History[1].Anchor)
}

func TestAnchorSlowCalendarLeavesTimeForHistoryHydration(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	anchorID := f.add(f.now, false)
	f.project()
	calendar := &blockingTimelineCalendar{CalendarSource: f.src.Calendar}
	f.src.Calendar = calendar
	req := anchorRequest(messageRef(anchorID))

	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.History, 1)
	assertions.True(got.History[0].Anchor)
	assertions.Equal(1, calendar.reads)
}

func TestAnchorFilteredAnchorStillShown(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	id := f.add(f.now, false)
	f.project()
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE messages SET list_id='list.example.com' WHERE id=?"), id)
	requirements.NoError(err)
	got, err := Build(t.Context(), f.src, anchorRequest(messageRef(id)))
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	assertions.True(got.Anchor.Filtered)
	requirements.Len(got.History, 1)
	assertions.True(got.History[0].Filtered)
	assertions.True(got.History[0].Anchor)
}
func TestAnchorUndatedRefused(t *testing.T) {
	f := newTimelineFixture(t)
	id := f.add(f.now, false, func(m *store.Message) {
		m.SentAt = sql.NullTime{}
		m.InternalDate = sql.NullTime{}
		m.ReceivedAt = sql.NullTime{}
	})
	_, err := Build(t.Context(), f.src, anchorRequest(messageRef(id)))
	assert.ErrorIs(t, err, ErrUndatedAnchor)
}
func TestAnchorDateOnlyStartsAtMidnight(t *testing.T) {
	f := newTimelineFixture(t)
	loc, err := time.LoadLocation("America/Vancouver")
	require.NoError(t, err)
	f.src.Location = loc
	got, err := Build(t.Context(), f.src, anchorRequest("2026-10-22"))
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 10, 22, 0, 0, 0, 0, loc), got.Anchor.Time)
}
func TestAnchorBareConversationUsesLatestEligible(t *testing.T) {
	f := newTimelineFixture(t)
	f.add(f.now.Add(-time.Hour), false)
	f.add(f.now, false)
	f.project()
	got, err := Build(t.Context(), f.src, anchorRequest(fmt.Sprintf("conv:%d", f.ConvID)))
	require.NoError(t, err)
	assert.WithinDuration(t, f.now, got.Anchor.Time, time.Microsecond)
}
func TestAnchorWithPersonNarrows(t *testing.T) {
	requirements := require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	id := f.add(f.now, false)
	other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
	conv, err := f.Store.EnsureConversation(f.Source.ID, "other", "Other")
	requirements.NoError(err)
	otherID := f.add(f.now.Add(time.Hour), false, func(m *store.Message) { m.SenderID = sql.NullInt64{Int64: other, Valid: true}; m.ConversationID = conv })
	requirements.NoError(f.Store.ReplaceMessageRecipients(otherID, "from", []int64{other}, nil))
	f.project()
	req := anchorRequest(messageRef(id))
	req.With = fmt.Sprintf("person:%d", p)
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.History, 1)
}
func TestAnchorEveryEntryAnchorRefRoundTrips(t *testing.T) {
	f := newTimelineFixture(t)
	f.add(f.now, false)
	f.transcript("unlinked", "", f.now.Add(-time.Hour))
	f.calendar("event", singleDayEvent)
	_, err := f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{LocalDate: "2026-10-22", Body: "Synthetic note", Author: "owner"})
	require.NoError(t, err)
	f.project()
	f.call(f.now.Add(-2 * time.Hour))
	bp, be := f.Store.ListActivityBurstsContext(t.Context(), store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Limit: 5000})
	t.Log("bursts", bp, be)
	got, err := Build(t.Context(), f.src, dayRequest())
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(allDayEntries(got)), 5)
	for _, entry := range allDayEntries(got) {
		t.Run(entry.Kind, func(t *testing.T) {
			around, e := Build(t.Context(), f.src, anchorRequest(entry.AnchorRef))
			require.NoError(t, e)
			require.NotNil(t, around.Anchor)
			if entry.Kind != "note" {
				found := false
				for _, v := range around.History {
					found = found || v.Anchor
				}
				assert.True(t, found)
			}
		})
	}
}

func TestAnchorOldestMessageInBurstIsNotFiltered(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	ids := []int64{}
	for i := range 8 {
		ids = append(ids, f.add(f.now.Add(time.Duration(i)*time.Second), false))
	}
	f.project()
	got, err := Build(t.Context(), f.src, anchorRequest(messageRef(ids[0])))
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	assertions.False(got.Anchor.Filtered)
	requirements.Len(got.History, 1)
	assertions.Equal("burst", got.History[0].Kind)
	assertions.True(got.History[0].Anchor)
	assertions.False(got.History[0].Filtered)
}

func TestAnchorNonselectedEmailCopyMatchesCollapsedBurst(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	copyID := sql.NullString{String: "<shared@example.test>", Valid: true}
	selected := f.add(f.now.Add(-time.Hour), false, func(m *store.Message) {
		m.RFC822MessageID = copyID
	})
	copyConversation, err := f.Store.EnsureConversation(f.Source.ID, "copy-thread", "Shared message")
	requirements.NoError(err)
	alias := f.add(f.now.Add(-time.Hour), false, func(m *store.Message) {
		m.ConversationID = copyConversation
		m.RFC822MessageID = copyID
	})
	assertions.Less(selected, alias)
	f.project()
	req := anchorRequest(messageRef(alias))

	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	assertions.Equal(messageRef(alias), got.Anchor.Ref)
	assertions.False(got.Anchor.Filtered)
	requirements.Len(got.History, 1)
	assertions.Equal(entryKindBurst, got.History[0].Kind)
	assertions.True(got.History[0].Anchor)
	assertions.False(got.History[0].Filtered)
}

func TestAnchorSecondCallObservationUsesLogicalCall(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	at := f.now.Add(-time.Hour)
	observation := callhistory.Observation{
		RecordID: "first", Version: 1, Provider: "signal", Account: "example", NativeCallID: "canonical-call",
		Kind: "call", Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined",
		OccurredAt: at, Precision: "instant", Participants: []callhistory.Participant{{ID: f.peer, Participation: "joined"}}, Raw: []byte(`{}`),
	}
	source, err := f.Store.GetOrCreateSource(callhistory.SourceType, observation.SourceIdentifier())
	requirements.NoError(err)
	_, err = f.Store.PutCallObservationContext(t.Context(), source.ID, observation)
	requirements.NoError(err)
	observation.RecordID = "second"
	observation.OccurredAt = at.Add(time.Minute)
	secondID, err := f.Store.PutCallObservationContext(t.Context(), source.ID, observation)
	requirements.NoError(err)

	got, err := Build(t.Context(), f.src, anchorRequest(messageRef(secondID)))
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	requirements.Len(got.History, 1)
	assertions.Equal("call", got.History[0].Kind)
	assertions.True(got.History[0].Anchor)
	assertions.False(got.History[0].Filtered)
	assertions.False(got.Anchor.Filtered)
	assertions.Equal(messageRef(secondID), got.Anchor.Ref)
}

func TestAnchorDayPrecisionCallUsesCivilDate(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	loc, err := time.LoadLocation("America/Vancouver")
	requirements.NoError(err)
	f := newTimelineFixture(t)
	f.src.Location = loc
	f.src.Clock = func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, loc) }
	previous := f.add(time.Date(2026, 10, 21, 20, 0, 0, 0, loc), false)
	callID := f.dayCall(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC))
	future := f.add(time.Date(2026, 10, 25, 12, 0, 0, 0, loc), false)
	f.project()
	req := anchorRequest(messageRef(callID))
	req.Before, req.After = 0, 1
	req.BeforeSet, req.AfterSet = true, true

	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	assertions.Equal(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), got.Anchor.Time)
	assertions.Equal("2026-09-21", got.Window.From)
	assertions.Equal("2026-11-22", got.Window.To)
	requirements.Len(got.History, 2)
	assertions.True(got.History[0].Anchor)
	assertions.Equal(messageRef(callID), got.History[0].Ref)
	requirements.NotNil(got.History[1].Burst)
	assertions.Contains(got.History[1].Burst.LatestMessageIDs, future)
	assertions.NotContains(got.History[1].Burst.LatestMessageIDs, previous)
	assertions.Equal("2026-10-25", got.History[1].Time.In(loc).Format(time.DateOnly))
}

func TestAnchorTimestampCountsDayPrecisionCallBeforeSameDate(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	loc, err := time.LoadLocation("America/Vancouver")
	requirements.NoError(err)
	f := newTimelineFixture(t)
	f.src.Location = loc
	f.src.Clock = func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, loc) }
	anchor := f.add(time.Date(2026, 10, 22, 12, 0, 0, 0, loc), false)
	f.dayCall(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC))
	future := f.add(time.Date(2026, 10, 25, 12, 0, 0, 0, loc), false)
	f.project()
	req := anchorRequest(messageRef(anchor))
	req.Before, req.After = 0, 1
	req.BeforeSet, req.AfterSet = true, true

	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	requirements.Len(got.History, 2)
	assertions.True(got.History[0].Anchor)
	requirements.NotNil(got.History[0].Burst)
	assertions.Contains(got.History[0].Burst.LatestMessageIDs, anchor)
	assertions.Equal(entryKindBurst, got.History[1].Kind)
	requirements.NotNil(got.History[1].Burst)
	assertions.Contains(got.History[1].Burst.LatestMessageIDs, future)
	assertions.NotContains(got.History[1].Burst.LatestMessageIDs, anchor)
	assertions.Equal("2026-10-25", got.History[1].Time.In(loc).Format(time.DateOnly))
}

func TestAnchorCalendarOccurrenceExcludedByTierFilterRemainsFilteredAnchor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	event := f.calendar("tier-filtered-anchor", singleDayEvent)
	person := f.savePerson()
	f.tier(person, 1)
	f.project()
	req := anchorRequest(fmt.Sprintf("evt:%d", event))
	req.Tiers = []int{2}

	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	requirements.True(got.Anchor.Filtered)
	requirements.Len(got.History, 1)
	assertions.Equal("meeting", got.History[0].Kind)
	assertions.True(got.History[0].Anchor)
	assertions.True(got.History[0].Filtered)
	assertions.Equal(fmt.Sprintf("evt:%d", event), got.History[0].Ref)
	assertions.Empty(got.History[0].Meeting.Attendees)
}

func TestBuildDayUsesFirstExistingInstantForSkippedMidnight(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	loc, err := time.LoadLocation("America/Sao_Paulo")
	requirements.NoError(err)
	f := newTimelineFixture(t)
	f.src.Location = loc
	f.src.Clock = func() time.Time { return time.Date(2018, 11, 4, 12, 0, 0, 0, loc) }
	f.calendar("skipped-midnight-event", `{"id":"skipped-midnight-event","iCalUID":"skip-midnight@example.org","summary":"Synthetic all-day","start":{"date":"2018-11-04"},"end":{"date":"2018-11-05"}}`)
	callAt := time.Date(2018, 11, 4, 23, 30, 0, 0, loc)
	f.call(callAt)
	_, err = f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{LocalDate: "2018-11-04", Body: "Synthetic date note", Author: "owner"})
	requirements.NoError(err)

	req := dayRequest()
	req.Date = "2018-11-04"
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.Days, 1)
	assertions.Equal("2018-11-04", got.Days[0].Date)
	requirements.Len(got.Days[0].Calls, 1, "the final local hour remains inside the selected day")
	assertions.WithinDuration(callAt, got.Days[0].Calls[0].Time, time.Microsecond)
	requirements.Len(got.Days[0].Meetings, 1)
	assertions.Equal(time.Date(2018, 11, 4, 1, 0, 0, 0, loc), got.Days[0].Meetings[0].Meeting.Start)
	assertions.Equal(time.Date(2018, 11, 5, 0, 0, 0, 0, loc), got.Days[0].Meetings[0].Meeting.End)
	requirements.Len(got.Days[0].Notes, 1, "notes use the requested civil date once")
	assertions.Equal("2018-11-04", got.Days[0].Notes[0].Ref)

	anchor, err := ParseAnchor("2018-11-04", loc)
	requirements.NoError(err)
	assertions.Equal(time.Date(2018, 11, 4, 1, 0, 0, 0, loc), anchor.Time)
}

func TestAnchorMissingSourcesProduceUnavailableGaps(t *testing.T) {
	for _, ref := range []string{"2026-10-22", "msg:1"} {
		t.Run(ref, func(t *testing.T) {
			f := newTimelineFixture(t)
			f.src.Activity = nil
			f.src.Calls = nil
			f.src.Calendar = nil
			f.src.Notes = nil
			got, err := Build(t.Context(), f.src, anchorRequest(ref))
			require.NoError(t, err)
			for _, section := range []string{"history", "calls", "meetings", "notes"} {
				assert.Contains(t, got.Gaps, Gap{Section: section, Code: "unavailable"})
			}
			require.NotEmpty(t, got.Freshness.Sources)
		})
	}
}

func TestAnchorMixedListBurstStaysAdmitted(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	id := f.add(f.now, false)
	f.add(f.now.Add(time.Second), false)
	f.project()
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE messages SET list_id='list.example.com' WHERE id=?"), id)
	requirements.NoError(err)
	got, err := Build(t.Context(), f.src, anchorRequest(messageRef(id)))
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	assertions.False(got.Anchor.Filtered)
	requirements.Len(got.History, 1)
	assertions.Equal("burst", got.History[0].Kind)
	assertions.True(got.History[0].Anchor)
	assertions.False(got.History[0].Filtered)
}
