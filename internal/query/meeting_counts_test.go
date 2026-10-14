package query

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonMeetingCountsDeduplicateSelectedSources(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	b := NewTestDataBuilder(t)
	first := b.AddSourceWithType("first@example.com", "granola")
	second := b.AddSourceWithType("second@example.org", "twenty")
	person := b.AddParticipant("human@example.org", "example.org", "Example Human")
	for _, source := range []int64{first, second} {
		id := b.AddMessage(MessageOpt{SourceID: source, MessageType: "meeting_transcript", MeetingKey: "ical:planning@example.org", SentAt: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)})
		b.AddTo(id, person, "Example Human")
	}
	duplicate := b.AddMessage(MessageOpt{SourceID: first, MessageType: "meeting_transcript", MeetingKey: "ical:planning@example.org", SentAt: time.Date(2027, 3, 14, 10, 1, 0, 0, time.UTC)})
	b.AddTo(duplicate, person, "Example Human")
	calendar := b.AddMessage(MessageOpt{SourceID: first, MessageType: "calendar_event"})
	b.AddTo(calendar, person, "Example Human")
	engine := b.BuildEngine()
	for _, sources := range [][]int64{nil, {first}, {second}, {first, second}} {
		response, err := engine.SearchPeople(t.Context(), PersonSearchRequest{Explore: ExploreRequest{Context: Context{SourceIDs: sources}}})
		requirements.NoError(err)
		requirements.Len(response.Rows, 1)
		assertions.Equal(int64(1), response.Rows[0].MeetingCount)
	}
	result, err := engine.GetPerson(t.Context(), person, Context{After: new(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC))}, nil)
	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.Equal(int64(1), result.MeetingCount, "general filters and legacy detail count keys")
	selection, err := engine.ResolveExploreMeetings(t.Context(), ExploreSelectionRequest{Explore: ExploreRequest{}}, 100)
	requirements.NoError(err)
	assertions.Equal(int64(3), selection.MeetingCount, "Explore retains selected transcript-message cardinality")
}

func TestMixedRoomCanonicalHidesRoomsInIndexedAndLegacyDetail(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("owner@example.org", "granola")
	room := b.AddParticipant("room@resource.calendar.google.com", "resource.calendar.google.com", "Example Room")
	human := b.AddParticipant("human@example.org", "example.org", "Example Human")
	loneRoom := b.AddParticipant("lone@resource.calendar.google.com", "resource.calendar.google.com", "Example Lone Room")
	b.AddParticipantIdentifier(room, "email", "room@resource.calendar.google.com", "Example Room", true)
	b.AddParticipantIdentifier(human, "email", "human@example.org", "Example Human", true)
	b.LinkCluster(room, human)
	message := b.AddMessage(MessageOpt{SourceID: source, MessageType: "meeting_transcript", SentAt: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)})
	b.AddTo(message, room, "Example Room")
	b.AddTo(message, human, "Example Human")
	b.AddTo(message, loneRoom, "Example Lone Room")
	engine := b.BuildEngine()
	for _, context := range []Context{{}, {After: new(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC))}} {
		result, err := engine.GetPerson(t.Context(), room, context, []int64{room, human})
		requirements.NoError(err)
		requirements.NotNil(result)
		assertions.Equal("Example Human", result.DisplayLabel)
		assertions.NotEqual("Example Room", result.DisplayName)
		requirements.Len(result.Identifiers, 1)
		assertions.Equal("human@example.org", result.Identifiers[0].Value)
		result, err = engine.GetPerson(t.Context(), loneRoom, context, nil)
		requirements.NoError(err)
		assertions.Nil(result)
	}
	response, err := engine.SearchPeople(t.Context(), PersonSearchRequest{Query: "resource.calendar.google.com"})
	requirements.NoError(err)
	assertions.Empty(response.Rows)
	completions, err := engine.CompletePeople(t.Context(), PeopleCompletionRequest{Query: "room"})
	requirements.NoError(err)
	assertions.Empty(completions.Rows)
}

func TestCuratedURLKeepsRoomBoundPersonVisibleWithoutUnsupportedCompletion(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.org")
	room := b.AddParticipant("room@resource.calendar.google.com", "resource.calendar.google.com", "Example Room")
	message := b.AddMessage(MessageOpt{SourceID: source})
	b.AddTo(message, room, "Example Room")
	b.personContacts = append(b.personContacts, fmt.Sprintf("(%d::BIGINT,'url'::VARCHAR,'https://example.org/human'::VARCHAR)", room))
	engine := b.BuildEngine()
	response, err := engine.SearchPeople(t.Context(), PersonSearchRequest{})
	requirements.NoError(err)
	requirements.Len(response.Rows, 1)
	completion, err := engine.CompletePeople(t.Context(), PeopleCompletionRequest{Query: "example.org"})
	requirements.NoError(err)
	assertions.Empty(completion.Rows, "unsupported contact kinds do not enter typed completions")
}

func TestPersonSummaryUsesCuratedHumanContactFallback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.org")
	unnamed := b.AddParticipant("", "", "")
	b.personContacts = append(b.personContacts,
		fmt.Sprintf("(%d::BIGINT,'email'::VARCHAR,'human@example.org'::VARCHAR)", unnamed))
	message := b.AddMessage(MessageOpt{SourceID: source})
	b.AddTo(message, unnamed, "")
	engine := b.BuildEngine()

	directory, err := engine.SearchPeople(t.Context(), PersonSearchRequest{Page: PageSpec{Limit: 25}})
	requirements.NoError(err)
	requirements.Len(directory.Rows, 1)
	assertions.Equal("human@example.org", directory.Rows[0].DisplayLabel)

	person, err := engine.GetPersonSummary(t.Context(), unnamed, ExploreRequest{}, nil)
	requirements.NoError(err)
	requirements.Len(person.Rows, 1)
	assertions.Equal(directory.Rows[0].DisplayLabel, person.Rows[0].DisplayLabel,
		"contextual summary and directory agree when a current curated human contact is the only label")
}

func TestLegacyFilteredDetailUsesCanonicalHumanFallback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.org")
	canonical := b.AddParticipant("canonical@example.org", "example.org", "")
	alias := b.AddPhoneParticipant("+15550001111", "")
	b.LinkCluster(canonical, alias)
	message := b.AddMessage(MessageOpt{SourceID: source})
	b.AddTo(message, canonical, "")
	engine := b.BuildEngine()

	directory, err := engine.SearchPeople(t.Context(), PersonSearchRequest{})
	requirements.NoError(err)
	requirements.Len(directory.Rows, 1)
	assertions.Equal("canonical@example.org", directory.Rows[0].DisplayLabel)
	unfiltered, err := engine.GetPerson(t.Context(), canonical, Context{}, []int64{canonical, alias})
	requirements.NoError(err)
	requirements.NotNil(unfiltered)
	assertions.Equal(directory.Rows[0].DisplayLabel, unfiltered.DisplayLabel)

	filtered, err := engine.GetPerson(t.Context(), canonical, Context{SourceIDs: []int64{source}}, []int64{canonical, alias})
	requirements.NoError(err)
	requirements.NotNil(filtered)
	assertions.Equal(unfiltered.DisplayLabel, filtered.DisplayLabel,
		"filtered detail follows the unfiltered canonical-participant fallback policy")
}

func TestLegacyFilteredDetailRejectsRoomOnlyMemberFromStaleMixedRollup(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.org")
	room := b.AddParticipant("room@resource.calendar.google.com", "resource.calendar.google.com", "Example Room")
	human := b.AddParticipant("human@example.org", "example.org", "Example Human")
	b.LinkCluster(room, human)
	message := b.AddMessage(MessageOpt{SourceID: source})
	b.AddTo(message, room, "Example Room")
	b.AddTo(message, human, "Example Human")
	engine := b.BuildEngine()
	filteredContext := Context{SourceIDs: []int64{source}}

	// The cache still contains the prior mixed cluster. The current graph passed
	// by the caller represents a split where the room is now a singleton.
	roomOnly, err := engine.GetPerson(t.Context(), room, filteredContext, []int64{room})
	requirements.NoError(err)
	assertions.Nil(roomOnly, "a stale mixed rollup cannot make a current room-only person visible")
	unfilteredRoomOnly, err := engine.GetPerson(t.Context(), room, Context{}, []int64{room})
	requirements.NoError(err)
	assertions.Nil(unfilteredRoomOnly, "unfiltered exact detail also checks current room-only visibility")

	mixed, err := engine.GetPerson(t.Context(), room, filteredContext, []int64{room, human})
	requirements.NoError(err)
	requirements.NotNil(mixed, "a current mixed cluster remains visible before cache refresh")
}

func TestLegacyFilteredDetailHidesRoomOnlyClusterWithAddresslessAliasFromStaleRollup(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.org")
	room := b.AddParticipant("room@resource.calendar.google.com", "resource.calendar.google.com", "Example Room")
	human := b.AddParticipant("human@example.org", "example.org", "Example Human")
	addressless := b.AddParticipant("", "", "Example Alias")
	b.LinkCluster(room, human, addressless)
	meeting := b.AddMessage(MessageOpt{SourceID: source})
	b.AddTo(meeting, room, "Example Room")
	b.AddTo(meeting, human, "Example Human")
	b.AddTo(meeting, addressless, "")
	noContact := b.AddParticipant("", "", "Example Unidentified")
	other := b.AddMessage(MessageOpt{SourceID: source})
	b.AddTo(other, noContact, "Example Unidentified")
	engine := b.BuildEngine()

	// The rollup still has the prior mixed room/human cluster. Current members
	// reflect unlinking the human, leaving only the room and an addressless alias.
	roomOnly, err := engine.GetPerson(t.Context(), room, Context{}, []int64{room, addressless})
	requirements.NoError(err)
	assertions.Nil(roomOnly, "an addressless alias cannot keep a room-only cluster visible")

	mixed, err := engine.GetPerson(t.Context(), room, Context{}, []int64{room, human, addressless})
	requirements.NoError(err)
	requirements.NotNil(mixed, "a current non-room contact keeps the mixed cluster visible")

	unknown, err := engine.GetPerson(t.Context(), noContact, Context{}, nil)
	requirements.NoError(err)
	requirements.NotNil(unknown, "a cluster with no contacts remains visible")
}

func TestRoomFilteringPreservesNonRoomCanonicalDisplayName(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.org")
	canonical := b.AddParticipant("canonical@example.org", "example.org", "")
	alias := b.AddParticipant("alias@example.org", "example.org", "Example Alias")
	b.LinkCluster(canonical, alias)
	message := b.AddMessage(MessageOpt{SourceID: source})
	b.AddTo(message, alias, "Example Alias")
	engine := b.BuildEngine()
	person, err := engine.GetPerson(t.Context(), canonical, Context{}, []int64{canonical, alias})
	requirements.NoError(err)
	requirements.NotNil(person)
	assertions.Equal("Example Alias", person.DisplayLabel)
	assertions.Empty(person.DisplayName, "non-room hydration retains the canonical participant's observed field")
}

func TestRoomCanonicalUnnamedHumanLabelAgreesAcrossDetailFilters(t *testing.T) {
	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.org")
	room := b.AddParticipant("room@resource.calendar.google.com", "resource.calendar.google.com", "Example Room")
	human := b.AddParticipant("human@example.org", "example.org", "")
	b.LinkCluster(room, human)
	message := b.AddMessage(MessageOpt{SourceID: source, SentAt: time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)})
	b.AddTo(message, room, "Example Room")
	b.AddTo(message, human, "")
	engine := b.BuildEngine()
	for _, context := range []Context{{}, {SourceIDs: []int64{source}}, {After: new(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))}} {
		person, err := engine.GetPerson(t.Context(), room, context, []int64{room, human})
		require.NoError(t, err)
		require.NotNil(t, person)
		assert.Equal(t, "human@example.org", person.DisplayLabel)
	}
}

func TestRoomBoundCuratedHumanLabelAgreesAcrossFilters(t *testing.T) {
	for _, contact := range []struct{ kind, value string }{{"email", "human@example.org"}, {"url", "https://example.org/human"}} {
		t.Run(contact.kind, func(t *testing.T) {
			b := NewTestDataBuilder(t)
			source := b.AddSource("owner@example.org")
			room := b.AddParticipant("room@resource.calendar.google.com", "resource.calendar.google.com", "Example Room")
			message := b.AddMessage(MessageOpt{SourceID: source, SentAt: time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)})
			b.AddTo(message, room, "Example Room")
			b.personContacts = append(b.personContacts, fmt.Sprintf("(%d::BIGINT,'%s'::VARCHAR,'%s'::VARCHAR)", room, contact.kind, contact.value))
			engine := b.BuildEngine()
			for _, context := range []Context{{}, {SourceIDs: []int64{source}}, {After: new(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))}} {
				person, err := engine.GetPerson(t.Context(), room, context, nil)
				require.NoError(t, err)
				require.NotNil(t, person)
				assert.Equal(t, contact.value, person.DisplayLabel)
			}
		})
	}
}

func TestLegacyClusterLabelExcludesInactiveCuratedRoom(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.org")
	room := b.AddParticipant("", "", "Example Room")
	human := b.AddParticipant("human@example.org", "example.org", "")
	b.personContacts = append(b.personContacts, fmt.Sprintf("(%d::BIGINT,'email'::VARCHAR,'room@resource.calendar.google.com'::VARCHAR)", room))
	message := b.AddMessage(MessageOpt{SourceID: source})
	b.AddFrom(message, human, "")
	engine := b.BuildEngine()
	for _, id := range []int64{room, human} {
		person, err := engine.GetPerson(t.Context(), id, Context{}, []int64{room, human})
		requirements.NoError(err)
		requirements.NotNil(person)
		assertions.Equal("human@example.org", person.DisplayLabel)
		assertions.Empty(person.DisplayName)
		assertions.True(person.PartialLabel)
	}
}

func TestLegacyRoomOnlyClusterDoesNotAdmitAddresslessMember(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.org")
	room := b.AddParticipant("room@resource.calendar.google.com", "resource.calendar.google.com", "Example Room")
	addressless := b.AddParticipant("", "", "Example Alias")
	b.LinkCluster(room, addressless)
	message := b.AddMessage(MessageOpt{SourceID: source})
	b.AddFrom(message, room, "")
	b.AddTo(message, addressless, "")
	engine := b.BuildEngine()
	for _, id := range []int64{room, addressless} {
		person, err := engine.GetPerson(t.Context(), id, Context{}, []int64{room, addressless})
		requirements.NoError(err)
		assertions.Nil(person)
	}
}

func TestCuratedRoomCanonicalUsesHumanNameInIndexedAndLegacyDetail(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSource("owner@example.org")
	room := b.AddParticipant("", "", "Example Room")
	human := b.AddParticipant("human@example.org", "example.org", "Example Human")
	b.personContacts = append(b.personContacts, fmt.Sprintf("(%d::BIGINT,'email'::VARCHAR,'room@resource.calendar.google.com'::VARCHAR)", room))
	b.LinkCluster(room, human)
	message := b.AddMessage(MessageOpt{SourceID: source, SentAt: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)})
	b.AddTo(message, room, "")
	b.AddTo(message, human, "")
	engine := b.BuildEngine()
	for _, ctx := range []Context{{}, {After: new(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC))}} {
		person, err := engine.GetPerson(t.Context(), room, ctx, []int64{room, human})
		requirements.NoError(err)
		requirements.NotNil(person)
		assertions.Equal("Example Human", person.DisplayLabel)
		assertions.NotEqual("Example Room", person.DisplayName)
	}
}
