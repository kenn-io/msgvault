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
	require.NoError(t, err)
	require.Len(t, response.Rows, 4)
	assert.Equal(t, []PeopleCompletion{
		{ParticipantID: exact, DisplayLabel: "Alice", Kind: PeopleCompletionName,
			Value: "Alice", MatchValue: "alice", Source: "observed"},
		{ParticipantID: exact, DisplayLabel: "Alice", Kind: PeopleCompletionUsername,
			Value: "@alice", MatchValue: "alice", Source: "slack"},
		{ParticipantID: prefix, DisplayLabel: "Alice Example", Kind: PeopleCompletionName,
			Value: "Alice Example", MatchValue: "alice example", Source: "observed"},
		{ParticipantID: substring, DisplayLabel: "Malice Stone", Kind: PeopleCompletionName,
			Value: "Malice Stone", MatchValue: "malice stone", Source: "observed"},
	}, response.Rows)
	assert.NotEmpty(t, response.CacheRevision)
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
