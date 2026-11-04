package timeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/store"
)

func relationshipRequest(ref string) Request {
	return Request{With: ref, Scope: Scope{Owner: true}, Format: "json", Budget: 65536}
}

type blockingTimelineCalendar struct {
	CalendarSource

	reads int
}

func (s *blockingTimelineCalendar) ListCalendarOccurrencesContext(ctx context.Context, _ store.CalendarOccurrenceQuery) (store.CalendarOccurrencePage, error) {
	s.reads++
	<-ctx.Done()
	return store.CalendarOccurrencePage{}, ctx.Err()
}

type delayedTimelineCalendar struct {
	CalendarSource

	deadline time.Time
	delay    time.Duration
}

func (s *delayedTimelineCalendar) ListCalendarOccurrencesContext(ctx context.Context, _ store.CalendarOccurrenceQuery) (store.CalendarOccurrencePage, error) {
	s.deadline, _ = ctx.Deadline()
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return store.CalendarOccurrencePage{}, ctx.Err()
	case <-timer.C:
		return store.CalendarOccurrencePage{}, nil
	}
}

type delayedTimelineInventory struct {
	deadline time.Time
	delay    time.Duration
}

func (s *delayedTimelineInventory) Build(ctx context.Context, _ time.Time) (Freshness, error) {
	s.deadline, _ = ctx.Deadline()
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return Freshness{}, ctx.Err()
	case <-timer.C:
		return Freshness{}, nil
	}
}
func TestRelationshipCallOnlyContact(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	at := f.now.Add(-time.Hour)
	f.call(at)
	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirements.NoError(err)
	requirements.NotNil(got.Person)
	assertions.Equal(1, got.Person.InteractionCount)
	assertions.Equal([]string{"call"}, got.Person.Channels)
	assertions.WithinDuration(at, *got.Person.LastAt, time.Microsecond)
	assertions.Zero(got.Person.InitiatedByContact)
	assertions.Zero(got.Person.InitiatedByOwner)
	requirements.Len(got.History, 1)
	assertions.Equal("call", got.History[0].Kind)
}

func TestRelationshipDayPrecisionCallKeepsCalendarDate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	f.src.Location = time.FixedZone("America/Vancouver", -7*60*60)
	date := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	f.dayCall(date)

	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirements.NoError(err)
	requirements.NotNil(got.Person)
	requirements.NotNil(got.Person.FirstAt)
	requirements.NotNil(got.Person.LastAt)
	assertions.Equal("day", got.Person.FirstAtPrecision)
	assertions.Equal("day", got.Person.LastAtPrecision)
	assertions.Equal(date, got.Person.FirstAt.UTC())
	assertions.Equal(date, got.Person.LastAt.UTC())
	assertions.Contains(Markdown(got), "first 2026-10-22 · last 2026-10-22 · ↓ 2026-10-22")
	requirements.Len(got.History, 1)
	assertions.Equal("2026-10-22", got.History[0].Call.Date)
}

func TestMergeContactOrdersMixedPrecisionByTimelineCivilDate(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	zone := time.FixedZone("west", -7*60*60)
	day := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	message := time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			person := &PersonHeader{}
			c := composer{loc: zone, t: &Timeline{Person: person}}
			call := store.ContactAggregate{FirstAt: &day, LastAt: &day, FirstAtPrecision: "day", LastAtPrecision: "day"}
			mail := store.ContactAggregate{FirstAt: &message, LastAt: &message}
			if reverse {
				c.mergeContact(mail, false)
				c.mergeContact(call, true)
			} else {
				c.mergeContact(call, true)
				c.mergeContact(mail, false)
			}
			requirements.NotNil(person.FirstAt)
			requirements.NotNil(person.LastAt)
			assertions.Equal(message, *person.FirstAt)
			assertions.Equal(day, *person.LastAt)
			assertions.Empty(person.FirstAtPrecision)
			assertions.Equal("day", person.LastAtPrecision)
		})
	}
}

func TestRelationshipCallRosterDeduplicatesSavedAliases(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	alias, err := f.Store.EnsureParticipantByPhone("+12025550102", "Alice", "signal")
	requirements.NoError(err)
	_, err = f.Store.LinkParticipants(f.peer, alias)
	requirements.NoError(err)
	f.callWithParticipants(f.now.Add(-time.Hour), []callhistory.Participant{
		{ID: f.peer, Participation: "invited"},
		{ID: alias, Participation: "joined"},
	})

	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirements.NoError(err)
	requirements.Len(got.History, 1)
	call := got.History[0].Call
	requirements.NotNil(call)
	assertions.Equal(1, call.PeopleCount)
	assertions.Len(call.People, 1)
	assertions.Equal(p, call.People[0].PersonID)
	assertions.Equal("joined", call.People[0].Participation)
	assertions.True(call.People[0].Credited)
}

func TestRelationshipDayPrecisionCallTodayBeforeUTCAnchor(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	f.src.Location = time.FixedZone("east", 9*60*60)
	now := time.Date(2026, 10, 22, 15, 30, 0, 0, time.UTC)
	f.src.Clock = func() time.Time { return now }
	p := f.savePerson()
	date := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	f.dayCall(date)

	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirements.NoError(err)
	requirements.NotNil(got.Person)
	assertions.Equal(1, got.Person.InteractionCount)
	requirements.Len(got.History, 1)
	assertions.Equal("2026-10-23", got.History[0].Call.Date)
	assertions.Equal(date, *got.Person.LastAt)
}

func TestRelationshipHistoryOrdersDayPrecisionByCivilDate(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	location, err := time.LoadLocation("America/Los_Angeles")
	requirements.NoError(err)
	f.src.Location = location
	now := time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC)
	f.src.Clock = func() time.Time { return now }
	p := f.savePerson()
	date := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	f.dayCall(date)
	f.add(time.Date(2026, 10, 23, 6, 0, 0, 0, time.UTC), false)
	f.project()

	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirements.NoError(err)
	requirements.Len(got.History, 2)
	assertions.Equal("call", got.History[0].Kind)
	assertions.Equal("2026-10-23", got.History[0].Call.Date)
	assertions.Equal("burst", got.History[1].Kind)

	c := composer{
		t:         &Timeline{},
		loc:       location,
		keys:      map[string]string{},
		positions: map[string]string{},
	}
	entries := []Entry{got.History[0], got.History[1]}
	c.sortAndContinue("history", entries, true)
	requirements.Len(entries, 2)
	assertions.Equal("call", entries[0].Kind)
	assertions.Equal("burst", entries[1].Kind)
	c.positions["history"] = c.keys["history|"+entries[0].Key]
	remaining := c.afterPosition("history", entries)
	requirements.Len(remaining, 1)
	assertions.Equal("burst", remaining[0].Kind)
}

func TestRelationshipSlowFreshnessLeavesTimeForHistoryHydration(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	id := f.add(f.now.Add(-time.Hour), false)
	f.project()
	f.src.Inventory = blockingInventory{}

	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirements.NoError(err)
	requirements.Len(got.History, 1)
	requirements.NotNil(got.History[0].Burst)
	assertions.NotEmpty(got.History[0].AnchorRef)
	assertions.Contains(got.History[0].Burst.LatestMessageIDs, id)
}

func TestRelationshipSlowCalendarLeavesTimeForHistoryHydration(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	id := f.add(f.now.Add(-time.Hour), false)
	f.project()
	calendar := &blockingTimelineCalendar{CalendarSource: f.src.Calendar}
	f.src.Calendar = calendar

	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirements.NoError(err)
	requirements.Len(got.History, 1)
	requirements.NotNil(got.History[0].Burst)
	assertions.Contains(got.History[0].Burst.LatestMessageIDs, id)
	assertions.Equal(1, calendar.reads)
}

func TestRelationshipSnapshotSharesCollectionDeadlineAcrossCalendarAndFreshness(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	id := f.add(f.now.Add(-time.Hour), false)
	f.project()
	calendar := &delayedTimelineCalendar{CalendarSource: f.src.Calendar, delay: 25 * time.Millisecond}
	inventory := &delayedTimelineInventory{delay: 25 * time.Millisecond}
	f.src.Calendar = calendar
	f.src.Inventory = inventory
	ctx, release, err := f.Store.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()

	got, err := Build(ctx, f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirements.NoError(err)
	requirements.Len(got.History, 1)
	requirements.NotNil(got.History[0].Burst)
	assertions.Contains(got.History[0].Burst.LatestMessageIDs, id)
	requirements.False(calendar.deadline.IsZero())
	requirements.False(inventory.deadline.IsZero())
	assertions.WithinDuration(calendar.deadline, inventory.deadline, 5*time.Millisecond,
		"freshness must use the remaining collection budget after the calendar read")
}
func TestRelationshipMeetingOnlyContactDeduplicatesNotes(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	f.calendar("event", singleDayEvent)
	for _, key := range []string{"one", "two"} {
		id := f.transcript(key, "planning@example.com", f.now.Add(-time.Hour))
		requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.peer}, nil))
	}
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirements.NoError(err)
	requirements.NotNil(got.Person)
	assertions.Equal(1, got.Person.InteractionCount)
	assertions.Equal([]string{"meeting"}, got.Person.Channels)
	assertions.Equal(10, got.Person.FirstAt.Hour())
}

func TestRelationshipCountsEveryExactOccurrenceOnceAcrossTranscripts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	one := f.calendar("linked-one", singleDayEvent)
	twoRaw := strings.NewReplacer(
		`"id":"planning"`, `"id":"linked-two"`,
		`"iCalUID":"planning@example.com"`, `"iCalUID":"other@example.com"`,
		"2026-10-22T10:00:00Z", "2026-10-21T10:00:00Z",
		"2026-10-22T11:00:00Z", "2026-10-21T11:00:00Z",
	).Replace(singleDayEvent)
	two := f.calendar("linked-two", twoRaw)
	notes := []int64{
		f.transcript("linked-notes-one", "planning@example.com", f.now.Add(-time.Hour)),
		f.transcript("linked-notes-two", "planning@example.com", f.now.Add(-30*time.Minute)),
	}
	for _, note := range notes {
		for _, event := range []int64{one, two} {
			requirements.NoError(f.Store.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: event}))
		}
	}
	f.project()

	got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
	requirements.NoError(err)
	requirements.NotNil(got.Person)
	assertions.Equal(2, got.Person.InteractionCount)
	assertions.Equal([]string{"meeting"}, got.Person.Channels)
	requirements.Len(got.History, 2)
	refs := []string{got.History[0].Ref, got.History[1].Ref}
	assertions.ElementsMatch([]string{fmt.Sprintf("evt:%d", one), fmt.Sprintf("evt:%d", two)}, refs)
	for _, entry := range got.History {
		assertions.Equal("meeting", entry.Kind)
		assertions.ElementsMatch(notes, entry.Meeting.LinkedTranscriptIDs)
	}
}

func TestRelationshipCountsOnlyOccurrencesAttendedBySubject(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	attended := f.calendar("attended", singleDayEvent)
	unattendedRaw := strings.NewReplacer(
		`"id":"planning"`, `"id":"unattended"`,
		`"iCalUID":"planning@example.com"`, `"iCalUID":"other@example.com"`,
		"2026-10-22T10:00:00Z", "2026-10-21T10:00:00Z",
		"2026-10-22T11:00:00Z", "2026-10-21T11:00:00Z",
		"alice@example.com", "bob@example.test",
		"Alice", "Bob",
	).Replace(singleDayEvent)
	unattended := f.calendar("unattended", unattendedRaw)
	note := f.transcript("linked-notes", "planning@example.com", f.now.Add(-time.Hour))
	for _, event := range []int64{attended, unattended} {
		requirements.NoError(f.Store.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: event}))
	}
	f.project()

	got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
	requirements.NoError(err)
	requirements.NotNil(got.Person)
	assertions.Equal(1, got.Person.InteractionCount)
	assertions.Equal("2026-10-22", got.Person.FirstAt.Format(time.DateOnly))
	assertions.Equal("2026-10-22", got.Person.LastAt.Format(time.DateOnly))
	requirements.Len(got.History, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", attended), got.History[0].Ref)
}

func TestRelationshipEvidenceOlderThanHistoryPage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	want := [12]int{}
	for month := range 12 {
		at := time.Date(2025, time.Month(11+month), 5, 10, 0, 0, 0, time.UTC)
		f.add(at, false)
		want[month] = 1
	}
	for day := range 25 {
		f.add(f.now.AddDate(0, 0, day-24), day%2 == 0)
		index := 11 - ((f.now.Year()-atForDay(f.now, day).Year())*12 + int(f.now.Month()-atForDay(f.now, day).Month()))
		want[index]++
	}
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirements.NoError(err)
	requirements.NotNil(got.Person)
	assertions.Equal(37, got.Person.InteractionCount)
	assertions.Equal(want, got.Person.Monthly)
	assertions.Equal("2025-11-05", got.Person.FirstAt.Format(time.DateOnly))
	assertions.Equal([]string{"email"}, got.Person.Channels)
	requirements.Len(got.History, 20)
	assertions.NotEmpty(got.Cursor)
}
func TestRelationshipUnsavedClusterHasNoTierNoLoops(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	f.add(f.now, false)
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
	requirements.NoError(err)
	requirements.NotNil(got.Person)
	assertions.Zero(got.Person.Person.PersonID)
	assertions.Zero(got.Person.Person.Tier)
	assertions.Nil(got.OpenLoops)
	assertions.Contains(got.Gaps, Gap{Section: "loops", Code: "no-person"})
	requirements.Len(got.History, 1)
}
func TestRelationshipHistoryLastTwentyAnyAge(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	for day := range 24 {
		f.add(time.Date(2019, 1, 1, 12, 0, 0, 0, time.UTC).AddDate(0, 0, day), false)
	}
	f.project()
	req := relationshipRequest(fmt.Sprintf("person:%d", p))
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.History, 20)
	assertions.Equal("2019-01-24", got.History[0].Time.Format(time.DateOnly))
	requirements.NotEmpty(got.Cursor)
	req.Cursor = got.Cursor
	next, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(next.History, 4)
	assertions.Empty(next.Cursor)
}

func atForDay(now time.Time, day int) time.Time { return now.AddDate(0, 0, day-24) }

func TestRelationshipCuratedProfileIncludesFreshness(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("DELETE FROM person_participants WHERE person_id=?"), p)
	requirements.NoError(err)
	want, err := f.src.Inventory.Build(t.Context(), f.now)
	requirements.NoError(err)
	requirements.NotEmpty(want.Sources)
	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirements.NoError(err)
	assertions.Equal(want, got.Freshness)
	assertions.Empty(got.History)
	assertions.Contains(got.Gaps, Gap{Section: "history", Code: "no-linked-contacts"})
}
func TestRelationshipMissingSourcesProduceUnavailableGaps(t *testing.T) {
	f := newTimelineFixture(t)
	p := f.savePerson()
	f.add(f.now, false)
	f.project()
	f.src.Activity = nil
	f.src.Calls = nil
	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	require.NoError(t, err)
	for _, section := range []string{"contact", "history", "calls"} {
		assert.Contains(t, got.Gaps, Gap{Section: section, Code: "unavailable"})
	}
	require.NotEmpty(t, got.Freshness.Sources)
}
