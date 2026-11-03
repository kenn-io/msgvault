package timeline

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func relationshipRequest(ref string) Request {
	return Request{With: ref, Scope: Scope{Owner: true}, Format: "json", Budget: 65536}
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
