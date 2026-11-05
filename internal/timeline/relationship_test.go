package timeline

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRelationshipUnsavedContactOpenLoops(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%t", pending), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineFixture(t)
			var id int64
			if pending {
				for i := range 12 {
					f.PersistEmail(storetest.EmailSpec{Thread: fmt.Sprintf("other-%d", i), From: "other@example.com", To: []string{storetest.OwnerAddress}, Body: "Can you send the invoice?", SentAt: f.now.Add(-96 * time.Hour)})
				}
				id = f.PersistEmail(storetest.EmailSpec{Thread: "unsaved-loop", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Body: "Can you send the contract?", SentAt: f.now.Add(-48 * time.Hour)})
			} else {
				f.add(f.now, false)
			}
			f.project()
			got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
			requirements.NoError(err)
			requirements.NotNil(got.OpenLoops)
			if pending {
				requirements.Len(got.OpenLoops.Items, 1)
				assertions.Equal(messageRef(id), got.OpenLoops.Items[0].Ref)
			} else {
				assertions.Empty(got.OpenLoops.Items)
				requirements.Len(got.History, 1)
			}
			assertions.False(got.OpenLoops.Truncated)
			requirements.NotNil(got.Person)
			assertions.Zero(got.Person.Person.PersonID)
			assertions.Zero(got.Person.Person.Tier)
		})
	}
}

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
func TestRelationshipEvidenceOlderThanHistoryPage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	want := [12]int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 4, 23}
	for month := range 12 {
		at := time.Date(2025, time.Month(11+month), 5, 10, 0, 0, 0, time.UTC)
		f.add(at, false)
	}
	for day := range 25 {
		f.add(f.now.AddDate(0, 0, day-24), day%2 == 0)
	}
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirements.NoError(err)
	requirements.NotNil(got.Person)
	assertions.Equal(37, got.Person.InteractionCount)
	assertions.Equal(want, got.Person.Monthly)
	assertions.Equal("2025-11-05", got.Person.FirstAt.Format(time.DateOnly))
	assertions.Equal([]string{"email"}, got.Person.Channels)
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
func TestRelationshipTranscriptLinkedToTwoMeetings(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	first := f.calendar("first", singleDayEvent)
	second := f.calendar("second", strings.NewReplacer("planning", "second", "2026-10-22", "2026-09-22").Replace(singleDayEvent))
	for _, key := range []string{"one", "two"} {
		note := f.transcript(key, "", f.now.Add(-time.Hour))
		requirements.NoError(f.Store.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: first}))
		requirements.NoError(f.Store.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: second}))
	}
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
	requirements.NoError(err)
	requirements.Len(got.History, 2)
	assertions.ElementsMatch([]string{fmt.Sprintf("evt:%d", first), fmt.Sprintf("evt:%d", second)}, []string{got.History[0].Ref, got.History[1].Ref})
	assertions.Equal(2, got.Person.InteractionCount)
	assertions.Equal(1, got.Person.Monthly[10])
	assertions.Equal(1, got.Person.Monthly[11])
}
