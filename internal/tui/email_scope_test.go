package tui

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestEmailModeMixedTypeSQLiteResults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	tdb.SeedStandardDataSet()
	const term = "zzemailmodescope"
	typedEmailID := tdb.AddMessage(dbtest.MessageOpts{Subject: term + " typed", MessageType: "email"})
	legacyEmailID := tdb.AddMessage(dbtest.MessageOpts{Subject: term + " legacy", MessageType: "email"})
	meetingID := tdb.AddMessage(dbtest.MessageOpts{Subject: term + " meeting", MessageType: "meeting_transcript"})
	textID := tdb.AddMessage(dbtest.MessageOpts{Subject: term + " text", MessageType: "sms"})
	_, err := tdb.DB.Exec(`UPDATE messages SET message_type = '' WHERE id = ?`, legacyEmailID)
	require.NoError(err)

	model := New(query.NewSQLiteEngine(tdb.DB), Options{DataDir: t.TempDir(), Version: "test"})
	model.level = levelMessageList
	model.allMessages = true

	listMsg, ok := model.loadMessages()().(messagesLoadedMsg)
	require.True(ok)
	require.NoError(listMsg.err)
	listIDs := make([]int64, 0, len(listMsg.messages))
	for _, message := range listMsg.messages {
		listIDs = append(listIDs, message.ID)
	}
	assert.Contains(listIDs, typedEmailID)
	assert.Contains(listIDs, legacyEmailID)
	assert.NotContains(listIDs, meetingID)
	assert.NotContains(listIDs, textID)

	searchMsg, ok := model.loadSearch(term)().(searchResultsMsg)
	require.True(ok)
	require.NoError(searchMsg.err)
	searchIDs := make([]int64, 0, len(searchMsg.messages))
	for _, message := range searchMsg.messages {
		searchIDs = append(searchIDs, message.ID)
	}
	assert.ElementsMatch([]int64{typedEmailID, legacyEmailID}, searchIDs)

	model.level = levelAggregates
	model.viewType = query.ViewTime
	model.timeGranularity = query.TimeYear
	model.searchQuery = "message_type:meeting_transcript " + term
	aggregateMsg, ok := model.loadData()().(dataLoadedMsg)
	require.True(ok)
	require.NoError(aggregateMsg.err)
	assert.Empty(aggregateMsg.rows)
	require.NotNil(aggregateMsg.filteredStats)
	assert.Zero(aggregateMsg.filteredStats.MessageCount)
}

func TestEmailModeScopesMessageQueries(t *testing.T) {
	t.Run("message list", func(t *testing.T) {
		var captured query.MessageFilter
		engine := newMockEngine(MockConfig{})
		engine.ListMessagesFunc = func(_ context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
			captured = filter
			return nil, nil
		}
		model := New(engine, Options{DataDir: t.TempDir(), Version: "test"})

		msg, ok := model.loadMessages()().(messagesLoadedMsg)
		require.True(t, ok)
		require.NoError(t, msg.err)
		assert.Equal(t, "email", captured.MessageType)
	})

	t.Run("fast search", func(t *testing.T) {
		var captured query.MessageFilter
		engine := newMockEngine(MockConfig{})
		engine.SearchFastWithStatsFunc = func(
			_ context.Context,
			_ *search.Query,
			_ string,
			filter query.MessageFilter,
			_ query.ViewType,
			_, _ int,
		) (*query.SearchFastResult, error) {
			captured = filter
			return &query.SearchFastResult{}, nil
		}
		model := New(engine, Options{DataDir: t.TempDir(), Version: "test"})

		msg, ok := model.loadSearch("shared term")().(searchResultsMsg)
		require.True(t, ok)
		require.NoError(t, msg.err)
		assert.Equal(t, "email", captured.MessageType)
	})

	t.Run("deep search", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		var captured *search.Query
		engine := newMockEngine(MockConfig{})
		engine.SearchFunc = func(_ context.Context, q *search.Query, _, _ int) ([]query.MessageSummary, error) {
			captured = q
			return nil, nil
		}
		model := New(engine, Options{DataDir: t.TempDir(), Version: "test"})
		model.searchMode = searchModeDeep

		msg, ok := model.loadSearch("shared term")().(searchResultsMsg)
		require.True(ok)
		require.NoError(msg.err)
		require.NotNil(captured)
		assert.Equal([]string{"email"}, captured.MessageTypes)
	})

	t.Run("thread", func(t *testing.T) {
		var captured query.MessageFilter
		engine := newMockEngine(MockConfig{})
		engine.ListMessagesFunc = func(_ context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
			captured = filter
			return nil, nil
		}
		model := New(engine, Options{DataDir: t.TempDir(), Version: "test"})

		msg, ok := model.loadThreadMessages(42)().(threadMessagesLoadedMsg)
		require.True(t, ok)
		require.NoError(t, msg.err)
		assert.Equal(t, "email", captured.MessageType)
	})
}
