package timeline

import (
	"fmt"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func relationshipRequest(ref string) Request {
	return Request{With: ref, Scope: Scope{Owner: true}, Format: "json", Budget: 65536}
}
func TestRelationshipCallOnlyContact(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	at := f.now.Add(-time.Hour)
	f.call(at)
	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	require.NoError(err)
	require.NotNil(got.Person)
	assert.Equal(1, got.Person.InteractionCount)
	assert.Equal([]string{"call"}, got.Person.Channels)
	assert.WithinDuration(at, *got.Person.LastAt, time.Microsecond)
	assert.Zero(got.Person.InitiatedByContact)
	assert.Zero(got.Person.InitiatedByOwner)
	require.Len(got.History, 1)
	assert.Equal("call", got.History[0].Kind)
}
func TestRelationshipMeetingOnlyContactDeduplicatesNotes(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	f.calendar("event", singleDayEvent)
	for _, key := range []string{"one", "two"} {
		id := f.transcript(key, "planning@example.com", f.now.Add(-time.Hour))
		require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.peer}, nil))
	}
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	require.NoError(err)
	require.NotNil(got.Person)
	assert.Equal(1, got.Person.InteractionCount)
	assert.Equal([]string{"meeting"}, got.Person.Channels)
	assert.Equal(10, got.Person.FirstAt.Hour())
}
func TestRelationshipEvidenceOlderThanHistoryPage(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
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
	require.NoError(err)
	require.NotNil(got.Person)
	assert.Equal(37, got.Person.InteractionCount)
	assert.Equal(want, got.Person.Monthly)
	assert.Equal("2025-11-05", got.Person.FirstAt.Format(time.DateOnly))
	assert.Equal([]string{"email"}, got.Person.Channels)
	require.Len(got.History, 20)
	assert.NotEmpty(got.Cursor)
}
func TestRelationshipUnsavedClusterHasNoTierNoLoops(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	f := newTimelineFixture(t)
	f.add(f.now, false)
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
	require.NoError(err)
	require.NotNil(got.Person)
	assert.Zero(got.Person.Person.PersonID)
	assert.Zero(got.Person.Person.Tier)
	assert.Nil(got.OpenLoops)
	assert.Contains(got.Gaps, Gap{Section: "loops", Code: "no-person"})
	require.Len(got.History, 1)
}
func TestRelationshipCuratedProfileWithoutParticipants(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("DELETE FROM person_participants WHERE person_id=?"), p)
	require.NoError(err)
	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	require.NoError(err)
	assert.Empty(got.History)
	assert.Contains(got.Gaps, Gap{Section: "history", Code: "no-linked-contacts"})
}
func TestRelationshipHistoryLastTwentyAnyAge(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	for day := range 24 {
		f.add(time.Date(2019, 1, 1, 12, 0, 0, 0, time.UTC).AddDate(0, 0, day), false)
	}
	f.project()
	req := relationshipRequest(fmt.Sprintf("person:%d", p))
	got, err := Build(t.Context(), f.src, req)
	require.NoError(err)
	require.Len(got.History, 20)
	assert.Equal("2019-01-24", got.History[0].Time.Format(time.DateOnly))
	require.NotEmpty(got.Cursor)
	req.Cursor = got.Cursor
	next, err := Build(t.Context(), f.src, req)
	require.NoError(err)
	require.Len(next.History, 4)
	assert.Empty(next.Cursor)
}

func atForDay(now time.Time, day int) time.Time { return now.AddDate(0, 0, day-24) }

func TestRelationshipCuratedProfileIncludesFreshness(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("DELETE FROM person_participants WHERE person_id=?"), p)
	require.NoError(err)
	want, err := f.src.Inventory.Build(t.Context(), f.now)
	require.NoError(err)
	require.NotEmpty(want.Sources)
	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	require.NoError(err)
	assert.Equal(want, got.Freshness)
	assert.Empty(got.History)
	assert.Contains(got.Gaps, Gap{Section: "history", Code: "no-linked-contacts"})
}
func TestRelationshipMissingSourcesProduceUnavailableGaps(t *testing.T) {
	f := newTimelineFixture(t)
	p := f.savePerson()
	f.add(f.now, false)
	f.project()
	f.src.Activity = nil
	f.src.Calls = nil
	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirepkg.NoError(t, err)
	for _, section := range []string{"contact", "history", "calls"} {
		assertpkg.Contains(t, got.Gaps, Gap{Section: section, Code: "unavailable"})
	}
	requirepkg.NotEmpty(t, got.Freshness.Sources)
}
