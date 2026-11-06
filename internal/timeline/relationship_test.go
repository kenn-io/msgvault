package timeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type relationshipMeetingReads struct {
	MeetingsSource

	resolutions int
}

func (r *relationshipMeetingReads) ResolveMeetingIdentitiesContext(ctx context.Context, req store.MeetingIdentityRequest) (store.MeetingIdentityResult, error) {
	r.resolutions++
	return r.MeetingsSource.ResolveMeetingIdentitiesContext(ctx, req)
}

type relationshipCalendarReads struct {
	CalendarSource

	details int
}

func (r *relationshipCalendarReads) GetCalendarOccurrenceContext(ctx context.Context, ref string) (store.CalendarOccurrenceDetail, error) {
	r.details++
	return r.CalendarSource.GetCalendarOccurrenceContext(ctx, ref)
}

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
			got, err := Build(t.Context(), f.src, relationshipRequest("alice@example.com"))
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

func TestTimelineRelationshipCallExactlyNow(t *testing.T) {
	for _, at := range []time.Time{
		time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 22, 12, 0, 0, 123000000, time.UTC),
	} {
		t.Run(at.Format(time.RFC3339Nano), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineFixture(t)
			f.src.Clock = func() time.Time { return at }
			id := f.call(at)
			f.call(at.Add(time.Millisecond))
			got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
			requirements.NoError(err)
			requirements.NotNil(got.Person)
			assertions.Equal(1, got.Person.InteractionCount)
			requirements.Len(got.History, 1)
			assertions.Equal(messageRef(id), got.History[0].Ref)
			assertions.Equal(at, got.History[0].Time)
		})
	}
}
func TestRelationshipLinkedTranscriptRetainsPeople(t *testing.T) {
	for _, tc := range []struct {
		name, attendees string
		people          []string
		attendeeCount   int
	}{
		{"no calendar attendees", `[]`, []string{"Alice"}, 0},
		{"different calendar attendee", `[{"email":"other@example.com","displayName":"Other"}]`, []string{"Alice", "Other"}, 1},
		{"same calendar attendee", `[{"email":"alice@example.com","displayName":"Alice"}]`, []string{"Alice"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineFixture(t)
			f.EnsureParticipant("other@example.com", "Other", "example.com")
			event := f.calendar("attended", `{"id":"attended","start":{"dateTime":"2026-10-22T10:00:00Z"},"end":{"dateTime":"2026-10-22T11:00:00Z"},"attendees":`+tc.attendees+`}`)
			note := f.transcript("attended", "", f.now.Add(-time.Hour))
			requirements.NoError(f.Store.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: event}))
			f.project()
			got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
			requirements.NoError(err)
			requirements.Len(got.History, 1)
			meeting := got.History[0].Meeting
			requirements.NotNil(meeting)
			assertions.Equal([]int64{note}, meeting.LinkedTranscriptIDs)
			names := []string{}
			for _, person := range meeting.People {
				names = append(names, person.Name)
			}
			assertions.ElementsMatch(tc.people, names)
			assertions.Equal(len(tc.people), meeting.PeopleCount)
			assertions.Len(meeting.Attendees, tc.attendeeCount)
		})
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

func TestRelationshipCuratedProfileFreshnessAndUnavailablePerson(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("DELETE FROM person_participants WHERE person_id=?"), p)
	requirements.NoError(err)
	want := Freshness{AsOf: f.now, Sources: []SourceFreshness{{SourceType: "gcal", Identifier: "calendar@example.com/work", Reason: "stale"}}}
	f.src.Freshness = func(context.Context, time.Time) (Freshness, error) { return want, nil }
	got, err := Build(t.Context(), f.src, relationshipRequest(fmt.Sprintf("person:%d", p)))
	requirements.NoError(err)
	assertions.Equal(want.AsOf, got.Freshness.AsOf)
	assertions.Equal(want.StaleThreshold, got.Freshness.StaleThreshold)
	assertions.Equal(want.Sources, got.Freshness.Sources)
	assertions.Equal([]ProjectionFreshness{{Name: "activity", State: "reconciling"}}, got.Freshness.Projections)
	assertions.ElementsMatch(want.Warnings, got.Freshness.Warnings)
	assertions.Empty(got.History)
	assertions.Contains(got.Gaps, Gap{Section: "history", Code: "no-linked-contacts"})
	f.src.People = failedPersonLookup{PeopleSource: f.src.People, err: errors.New("person reader unavailable")}
	got, err = Build(t.Context(), f.src, relationshipRequest("person:1"))
	requirements.NoError(err)
	assertions.Contains(got.Gaps, Gap{Section: "person", Code: "unavailable"})
}
func TestTimelineRelationshipOnlyCreditsSubjectMeetings(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	first := f.calendar("first", singleDayEvent)
	second := f.calendar("second", strings.NewReplacer("planning", "second", "2026-10-22", "2026-09-22", "alice@example.com", "other@example.com", "Alice", "Other").Replace(singleDayEvent))
	note := f.transcript("shared", "", f.now.Add(-time.Hour))
	requirements.NoError(f.Store.ReplaceMessageRecipients(note, "to", nil, nil))
	requirements.NoError(f.Store.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: first}))
	requirements.NoError(f.Store.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: second}))
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
	requirements.NoError(err)
	requirements.NotNil(got.Person)
	assertions.Equal(1, got.Person.InteractionCount)
	assertions.Zero(got.Person.Monthly[10])
	assertions.Equal(1, got.Person.Monthly[11])
	requirements.Len(got.History, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", first), got.History[0].Ref)
}

func TestTimelineRelationshipCountsLinkedCalendarCopiesOnce(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	first := f.calendar("first", singleDayEvent)
	source, err := f.Store.GetOrCreateSource("gcal", "second-calendar@example.com")
	requirements.NoError(err)
	requirements.NoError(f.Store.UpdateSourceSyncConfig(source.ID, `{"calendar_id":"second-calendar@example.com","time_zone":"UTC"}`))
	second, err := f.Store.PersistMessage(&store.MessagePersistData{
		Metadata:     &sql.NullString{String: `{"ical_uid":"planning@example.com","calendar_id":"second-calendar@example.com"}`, Valid: true},
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "second", MessageType: "calendar_event", Subject: sql.NullString{String: "Planning", Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "second", ConversationType: "calendar", Title: "Planning"},
		RawMIME:      []byte(singleDayEvent), RawFormat: "gcal_json",
	})
	requirements.NoError(err)
	for i, event := range []int64{first, second} {
		note := f.transcript(fmt.Sprintf("note-%d", i), "", f.now.Add(-time.Hour))
		requirements.NoError(f.Store.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: event}))
	}
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
	requirements.NoError(err)
	requirements.NotNil(got.Person)
	assertions.Equal(1, got.Person.InteractionCount)
	assertions.Equal(1, got.Person.Monthly[11])
	requirements.Len(got.History, 1)
	assertions.Len(got.History[0].Meeting.LinkedTranscriptIDs, 2)
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

func TestRelationshipRejectsOwnerAddress(t *testing.T) {
	f := newTimelineFixture(t)
	f.add(f.now.Add(-time.Hour), false)
	f.project()
	_, err := Build(t.Context(), f.src, relationshipRequest("email:owner@example.com"))
	require.ErrorIs(t, err, ErrInvalidRequest)
}

func TestRelationshipHistoryKeepsPastPartOfBurst(t *testing.T) {
	f := newTimelineFixture(t)
	past := f.now.Add(-time.Hour)
	id := f.add(past, false)
	f.add(f.now.Add(time.Minute), false)
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
	require := require.New(t)
	require.NoError(err)
	require.NotNil(got.Person)
	assert := assert.New(t)
	assert.Equal(1, got.Person.InteractionCount)
	require.Len(got.History, 1)
	assert.WithinDuration(past, got.History[0].Time, time.Microsecond)
	assert.Equal(1, got.History[0].Burst.Inbound)
	assert.Equal([]int64{id}, got.History[0].Burst.LatestMessageIDs)
}
