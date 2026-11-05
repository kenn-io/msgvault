package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestCalendarAccountTotalsMatchListing(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	eventID := env.AddMessage(dbtest.MessageOpts{SourceID: 1, ConversationID: 1, MessageType: "calendar_event", Subject: "Invite"})
	sourceID := int64(1)
	filter := MessageFilter{AccountScopes: []search.AccountScope{{SourceID: &sourceID, Unattributed: true}}}
	rows, err := env.Engine.ListMessages(env.Ctx, filter)
	requirements.NoError(err)
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	assertions.Contains(ids, eventID)
	stats, err := env.Engine.GetTotalStats(env.Ctx, StatsOptions{Filter: &filter})
	requirements.NoError(err)
	assertions.Equal(int64(len(rows)), stats.MessageCount)
}

func TestCalendarSearchTotalsIncludeEmailDerivedEvents(t *testing.T) {
	env := newTestEnv(t)
	id := env.AddMessage(dbtest.MessageOpts{SourceID: 1, ConversationID: 1, MessageType: "calendar_event", Subject: "Invite"})
	for _, raw := range []string{"message_type:calendar_event", "message_type:calendar_event | message_type:email subject:Absent"} {
		t.Run(raw, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			opts := DefaultAggregateOptions()
			opts.SearchQuery = raw
			rows, err := env.Engine.SubAggregate(env.Ctx, MessageFilter{}, ViewTime, opts)
			requirements.NoError(err)
			requirements.Len(rows, 1)
			assertions.Equal(int64(1), rows[0].Count)
			stats, err := env.Engine.GetTotalStats(env.Ctx, StatsOptions{SearchQuery: raw})
			requirements.NoError(err)
			assertions.Equal(int64(1), stats.MessageCount, "event %d must count in the total", id)
		})
	}
}

func TestAggregateDeletionSearchChatSource(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	env := newTestEnv(t)
	source := env.AddSource(dbtest.SourceOpts{Type: "google_chat", Identifier: "chat@example.test"})
	conversation := env.AddConversation(dbtest.ConversationOpts{SourceID: source})
	env.AddMessage(dbtest.MessageOpts{SourceID: source, ConversationID: conversation, MessageType: "google_chat", SentAt: "2024-01-15 10:00:00"})
	opts := DefaultAggregateOptions()
	opts.SearchQuery = "message_type:google_chat"
	rows, err := env.Engine.SubAggregate(env.Ctx, MessageFilter{}, ViewTime, opts)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(int64(1), rows[0].Count)
	targets, err := env.Engine.GetDeletionTargetsByAggregateSearch(env.Ctx, opts.SearchQuery, MessageFilter{}, ViewTime, rows[0].Key)
	requirements.NoError(err)
	assertions.Empty(targets, "interactive deletion only supports Gmail and Microsoft mail sources")
}
