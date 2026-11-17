package query

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFilteredRelationshipsCountGroupWithOwnerCopyOutsideFilter(t *testing.T) {
	b := NewTestDataBuilder(t)
	firstSource := b.AddSourceWithType("first@example.test", "granola")
	secondSource := b.AddSourceWithType("second@example.test", "zoom")
	owner := b.AddParticipant("owner@example.test", "example.test", "Owner")
	person := b.AddParticipant("alice@example.test", "example.test", "Alice")
	b.AddOwnerParticipant(firstSource, owner)
	b.AddOwnerParticipant(secondSource, owner)
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	ownerCopy := b.AddMessage(MessageOpt{SourceID: firstSource, MessageType: "meeting_transcript", SentAt: now.Add(-time.Hour)})
	selectedCopy := b.AddMessage(MessageOpt{SourceID: secondSource, MessageType: "meeting_transcript", SentAt: now})
	b.AddFrom(ownerCopy, owner, "Owner")
	b.AddTo(ownerCopy, person, "Alice")
	b.AddTo(selectedCopy, person, "Alice")
	b.meetingGroups = fmt.Sprintf("(SELECT %d::BIGINT AS message_id, %d::BIGINT AS group_id UNION ALL SELECT %d, %d)", ownerCopy, ownerCopy, selectedCopy, ownerCopy)
	engine := b.BuildEngine()

	people, err := engine.SearchPeople(t.Context(), PersonSearchRequest{
		Query:   "Alice",
		Explore: ExploreRequest{Context: Context{SourceIDs: []int64{secondSource}}},
	})
	require.NoError(t, err)
	require.Len(t, people.Rows, 1)
	require.Equal(t, int64(1), people.Rows[0].MeetingCount)

	relationships, err := engine.Relationships(t.Context(), RelationshipsRequest{
		Now:     now,
		Limit:   10,
		ShowAll: true,
		Context: Context{SourceIDs: []int64{secondSource}},
	})
	require.NoError(t, err)
	require.Len(t, relationships.Rows, 1)
	require.Equal(t, person, relationships.Rows[0].CanonicalID)
	require.Equal(t, int64(1), relationships.Rows[0].Signals.MeetingCount)
}

func TestRelationshipGroupOwnerEvidenceOutsideSelectedCopy(t *testing.T) {
	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.test")
	owner := b.AddParticipant("owner@example.test", "example.test", "Owner")
	person := b.AddParticipant("alice@example.test", "example.test", "Alice")
	b.AddOwnerParticipant(source, owner)
	ownerTime := time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC)
	selectedTime := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	ownerCopy := b.AddMessage(MessageOpt{SourceID: source, MessageType: "meeting_transcript", SentAt: ownerTime})
	selectedCopy := b.AddMessage(MessageOpt{SourceID: source, MessageType: "meeting_transcript", SentAt: selectedTime})
	b.AddFrom(ownerCopy, owner, "Owner")
	b.AddTo(selectedCopy, person, "Alice")
	b.meetingGroups = fmt.Sprintf("(SELECT %d::BIGINT AS message_id, %d::BIGINT AS group_id UNION ALL SELECT %d, %d)", ownerCopy, ownerCopy, selectedCopy, ownerCopy)
	engine := b.BuildEngine()

	after := selectedTime.Add(-time.Hour)
	for _, filter := range []Context{{}, {After: &after}} {
		result, err := engine.Relationships(t.Context(), RelationshipsRequest{Now: selectedTime.Add(time.Hour), Limit: 10, ShowAll: true, Context: filter})
		require.NoError(t, err)
		require.Len(t, result.Rows, 1)
		require.Equal(t, person, result.Rows[0].CanonicalID)
		require.Equal(t, int64(1), result.Rows[0].Signals.MeetingCount)
	}
	calendar, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{CanonicalID: person, Year: 2026, Timezone: "UTC"})
	require.NoError(t, err)
	var meetings int64
	for _, day := range calendar.Days {
		meetings += day.Meetings
	}
	require.Equal(t, int64(1), meetings)
}

func TestRelationshipGroupIgnoresUnsentOwnerCopy(t *testing.T) {
	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.test")
	owner := b.AddParticipant("owner@example.test", "example.test", "Owner")
	person := b.AddParticipant("alice@example.test", "example.test", "Alice")
	b.AddOwnerParticipant(source, owner)
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	ownerCopy := b.AddMessage(MessageOpt{SourceID: source, MessageType: "meeting_transcript", SentAt: now.Add(-time.Hour)})
	selectedCopy := b.AddMessage(MessageOpt{SourceID: source, MessageType: "meeting_transcript", SentAt: now})
	b.AddFrom(ownerCopy, owner, "Owner")
	b.AddTo(selectedCopy, person, "Alice")
	b.unsentMessages = fmt.Sprintf("(SELECT %d::BIGINT AS message_id)", ownerCopy)
	b.meetingGroups = fmt.Sprintf("(SELECT %d::BIGINT AS message_id, %d::BIGINT AS group_id UNION ALL SELECT %d, %d)", ownerCopy, ownerCopy, selectedCopy, ownerCopy)
	engine := b.BuildEngine()
	for _, filter := range []Context{{}, {SourceIDs: []int64{source}}} {
		result, err := engine.Relationships(t.Context(), RelationshipsRequest{Now: now, Limit: 10, ShowAll: true, Context: filter})
		require.NoError(t, err)
		require.Empty(t, result.Rows)
	}
}
