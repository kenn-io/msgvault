package query

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveExploreMeetingsSelectsOnlyExactMeetingEntries(t *testing.T) {
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.test")
	emailID := b.AddMessage(MessageOpt{SourceID: source, Subject: "Mail"})
	meetingID := b.AddMessage(MessageOpt{SourceID: source, Subject: "Meeting", MessageType: "meeting_transcript"})
	b.AddMessage(MessageOpt{SourceID: source, Subject: "Calendar", MessageType: "calendar_event"})
	b.AddMessage(MessageOpt{SourceID: source, Subject: "Chat", MessageType: "google_chat", ConversationType: "chat"})
	b.AddMessage(MessageOpt{SourceID: source, Subject: "Note", MessageType: "meeting_note"})
	b.AddMessage(MessageOpt{SourceID: source, Subject: "Minutes", MessageType: "meeting_minutes"})
	engine := b.BuildEngine()

	result, err := engine.ResolveExploreMeetings(t.Context(), ExploreSelectionRequest{
		Explore: ExploreRequest{},
	}, 100)

	require.NoError(t, err)
	assertions.Equal(int64(6), result.SelectedCount)
	assertions.Equal(int64(1), result.MeetingCount)
	assertions.Equal([]int64{meetingID}, result.MessageIDs)
	assertions.NotContains(result.MessageIDs, emailID)
	assertions.NotEmpty(result.CacheRevision)
	assertions.False(result.LimitExceeded)
}

func TestResolveExploreMeetingsPreservesNilAndEmptySelectionSemantics(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.test")
	first := b.AddMessage(MessageOpt{SourceID: source, Subject: "First", MessageType: "meeting_transcript"})
	second := b.AddMessage(MessageOpt{SourceID: source, Subject: "Second", MessageType: "meeting_transcript"})
	engine := b.BuildEngine()

	all, err := engine.ResolveExploreMeetings(t.Context(), ExploreSelectionRequest{Explore: ExploreRequest{}}, 1)
	requirements.NoError(err)
	assertions.Equal(int64(2), all.SelectedCount)
	assertions.Equal(int64(2), all.MeetingCount)
	assertions.Equal([]int64{first}, all.MessageIDs)
	assertions.True(all.LimitExceeded)

	none, err := engine.ResolveExploreMeetings(t.Context(), ExploreSelectionRequest{
		Explore: ExploreRequest{}, IncludedKeys: []string{},
	}, 100)
	requirements.NoError(err)
	assertions.Zero(none.SelectedCount)
	assertions.Zero(none.MeetingCount)
	assertions.Empty(none.MessageIDs)
	assertions.False(none.LimitExceeded)

	excluded, err := engine.ResolveExploreMeetings(t.Context(), ExploreSelectionRequest{
		Explore: ExploreRequest{}, ExcludedKeys: []string{"source:1:message:msg1"},
	}, 100)
	requirements.NoError(err)
	assertions.Equal([]int64{second}, excluded.MessageIDs)
}

func TestResolveExploreMeetingsDoesNotApplyExplorePageSize(t *testing.T) {
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.test")
	want := make([]int64, 55)
	for index := range want {
		want[index] = b.AddMessage(MessageOpt{
			SourceID: source, Subject: fmt.Sprintf("Meeting %02d", index), MessageType: "meeting_transcript",
		})
	}
	engine := b.BuildEngine()

	result, err := engine.ResolveExploreMeetings(t.Context(), ExploreSelectionRequest{
		Explore: ExploreRequest{},
	}, 100)

	require.NoError(t, err)
	assertions.Equal(int64(len(want)), result.SelectedCount)
	assertions.Equal(int64(len(want)), result.MeetingCount)
	assertions.Equal(want, result.MessageIDs)
	assertions.False(result.LimitExceeded)
}

func TestResolveExploreMeetingsHonorsRepeatedParticipantAndDomainGroups(t *testing.T) {
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.test")
	alice := b.AddParticipant("alice@alpha.example", "alpha.example", "Alice")
	bob := b.AddParticipant("bob@beta.example", "beta.example", "Bob")
	carol := b.AddParticipant("carol@gamma.example", "gamma.example", "Carol")
	wanted := b.AddMessage(MessageOpt{SourceID: source, Subject: "Wanted", MessageType: "meeting_transcript"})
	b.AddRecipient(wanted, alice, "to", "Alice")
	b.AddRecipient(wanted, bob, "to", "Bob")
	unwanted := b.AddMessage(MessageOpt{SourceID: source, Subject: "Unwanted", MessageType: "meeting_transcript"})
	b.AddRecipient(unwanted, alice, "to", "Alice")
	b.AddRecipient(unwanted, carol, "to", "Carol")
	engine := b.BuildEngine()

	result, err := engine.ResolveExploreMeetings(t.Context(), ExploreSelectionRequest{
		Explore: ExploreRequest{Context: Context{
			ParticipantIDs:              []int64{alice},
			AdditionalParticipantGroups: [][]int64{{bob}},
			Domains:                     []string{"alpha.example"},
			AdditionalDomainGroups:      [][]string{{"beta.example"}},
		}},
	}, 100)

	require.NoError(t, err)
	assert.Equal(t, int64(1), result.SelectedCount)
	assert.Equal(t, []int64{wanted}, result.MessageIDs)
}

func TestResolveExploreMeetingsUsesResolvedSearchCandidateAuthority(t *testing.T) {
	assertions := assert.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("archive@example.test")
	first := b.AddMessage(MessageOpt{SourceID: source, Subject: "First", MessageType: "meeting_transcript"})
	second := b.AddMessage(MessageOpt{SourceID: source, Subject: "Second", MessageType: "meeting_transcript"})
	generation := int64(17)
	engine := b.BuildEngine()

	result, err := engine.ResolveExploreMeetings(context.Background(), ExploreSelectionRequest{
		Explore: ExploreRequest{Search: SearchSpec{
			Mode: SearchSemantic, Query: "launch", CandidateMessageIDs: []int64{second},
			VectorGeneration: &generation,
		}},
	}, 100)

	require.NoError(t, err)
	assertions.Equal([]int64{second}, result.MessageIDs)
	assertions.NotContains(result.MessageIDs, first)
	assertions.Equal(SearchProvenance{VectorGeneration: &generation}, result.SearchProvenance)
}
