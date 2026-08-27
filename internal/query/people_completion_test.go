package query

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPeopleCompletionRanksExactPrefixSubstringAndTypedSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("archive@example.test", "gmail")
	exact := b.AddParticipant("exact@example.test", "example.test", "Alice")
	prefix := b.AddParticipant("prefix@example.test", "example.test", "Alice Example")
	substring := b.AddParticipant("substring@example.test", "example.test", "Malice Stone")
	b.AddParticipantIdentifier(exact, "slack", "alice", "@alice", true)
	when := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	for index, participantID := range []int64{exact, prefix, substring} {
		message := b.AddMessage(MessageOpt{
			SourceID: source, ConversationID: int64(100 + index),
			Subject: "completion fixture", SentAt: when.Add(time.Duration(index) * time.Minute),
		})
		b.AddFrom(message, participantID, "")
	}
	engine := b.BuildEngine()

	response, err := engine.CompletePeople(context.Background(), PeopleCompletionRequest{
		Query: "alice", Limit: 8,
	})
	require.NoError(err)
	require.Len(response.Rows, 4)
	assert.Equal([]PeopleCompletion{
		{ParticipantID: exact, DisplayLabel: "Alice", Kind: PeopleCompletionName,
			Value: "Alice", MatchValue: "alice", Source: "observed", LastAt: when},
		{ParticipantID: exact, DisplayLabel: "Alice", Kind: PeopleCompletionUsername,
			Value: "@alice", MatchValue: "alice", Source: "slack", LastAt: when},
		{ParticipantID: prefix, DisplayLabel: "Alice Example", Kind: PeopleCompletionName,
			Value: "Alice Example", MatchValue: "alice example", Source: "observed",
			LastAt: when.Add(time.Minute)},
		{ParticipantID: substring, DisplayLabel: "Malice Stone", Kind: PeopleCompletionName,
			Value: "Malice Stone", MatchValue: "malice stone", Source: "observed",
			LastAt: when.Add(2 * time.Minute)},
	}, response.Rows)
	assert.NotEmpty(response.CacheRevision)
}

func TestPeopleCompletionMatchesPhoneDigitsAcrossLinkedAliases(t *testing.T) {
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("archive@example.test", "sms")
	primary := b.AddParticipant("alice@example.test", "example.test", "Alice Example")
	alias := b.AddPhoneParticipant("+12025550123", "")
	b.LinkCluster(primary, alias)
	message := b.AddMessage(MessageOpt{
		SourceID: source, ConversationID: 200, Subject: "phone fixture",
		SentAt: time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC), MessageType: "sms",
	})
	b.AddFrom(message, alias, "")
	engine := b.BuildEngine()

	response, err := engine.CompletePeople(context.Background(), PeopleCompletionRequest{
		Query: "(202) 555-0123", Limit: 8,
	})
	require.NoError(t, err)
	require.Len(t, response.Rows, 1)
	assert.Equal(t, PeopleCompletion{
		ParticipantID: primary, DisplayLabel: "Alice Example", Kind: PeopleCompletionPhone,
		Value: "+12025550123", MatchValue: "+12025550123", Source: "observed",
		LastAt: time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC),
	}, response.Rows[0])
}

func TestPeopleCompletionMatchesAtPrefixedUsername(t *testing.T) {
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("archive@example.test", "slack")
	participantID := b.AddParticipant("alice@example.test", "example.test", "Alice")
	b.AddParticipantIdentifier(participantID, "slack", "alice", "@alice", true)
	when := time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC)
	message := b.AddMessage(MessageOpt{
		SourceID: source, ConversationID: 201, Subject: "username fixture", SentAt: when,
	})
	b.AddFrom(message, participantID, "")
	engine := b.BuildEngine()

	response, err := engine.CompletePeople(t.Context(), PeopleCompletionRequest{
		Query: "@alice", Limit: 8,
	})
	require.NoError(t, err)
	require.Len(t, response.Rows, 1)
	assert.Equal(t, PeopleCompletion{
		ParticipantID: participantID, DisplayLabel: "Alice", Kind: PeopleCompletionUsername,
		Value: "@alice", MatchValue: "alice", Source: "slack", LastAt: when,
	}, response.Rows[0])
}

func TestPeopleCompletionValidatesQueryAndLimit(t *testing.T) {
	engine := NewTestDataBuilder(t).BuildEngine()

	tests := []PeopleCompletionRequest{
		{Query: "   ", Limit: 8},
		{Query: strings.Repeat("x", 257), Limit: 8},
		{Query: "alice", Limit: -1},
		{Query: "alice", Limit: 21},
	}
	for _, request := range tests {
		_, err := engine.CompletePeople(context.Background(), request)
		assert.ErrorIs(t, err, ErrInvalidExploreRequest, request)
	}
}

func TestPeopleCompletionUsesTemperatureBeforeRecencyForEqualTextMatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	b := NewTestDataBuilder(t)
	sourceID := b.AddSourceWithType("owner@example.test", "gmail")
	ownerID := b.AddParticipant("owner@example.test", "example.test", "Owner")
	hotID := b.AddParticipant("hot@example.test", "example.test", "Alex Hot")
	coldID := b.AddParticipant("cold@example.test", "example.test", "Alex Cold")
	b.AddOwnerParticipant(sourceID, ownerID)
	for index := range 4 {
		messageID := b.AddMessage(MessageOpt{
			SourceID: sourceID, MessageType: "email", IsFromMe: true,
			SentAt: time.Date(2026, time.January, 1+index, 12, 0, 0, 0, time.UTC),
		})
		b.AddFrom(messageID, ownerID, "Owner")
		b.AddTo(messageID, hotID, "Alex Hot")
	}
	newerColdMessage := b.AddMessage(MessageOpt{
		SourceID: sourceID, MessageType: "email", IsFromMe: true,
		SentAt: time.Date(2026, time.February, 1, 12, 0, 0, 0, time.UTC),
	})
	b.AddFrom(newerColdMessage, ownerID, "Owner")
	b.AddTo(newerColdMessage, coldID, "Alex Cold")
	engine := b.BuildEngine()

	response, err := engine.CompletePeople(t.Context(), PeopleCompletionRequest{
		Query: "alex", Limit: 8,
	})
	require.NoError(err)
	require.Len(response.Rows, 2)
	assert.Equal(hotID, response.Rows[0].ParticipantID,
		"temperature breaks equal-quality matches before newer activity")
	assert.Equal(coldID, response.Rows[1].ParticipantID)
}
