package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func TestNextModeCyclesThroughMeetings(t *testing.T) {
	t.Run("with texts available", func(t *testing.T) {
		assert.Equal(t, modeTexts, nextMode(modeEmail, true))
		assert.Equal(t, modeMeetings, nextMode(modeTexts, true))
		assert.Equal(t, modeEmail, nextMode(modeMeetings, true))
	})

	t.Run("without texts available", func(t *testing.T) {
		assert.Equal(t, modeMeetings, nextMode(modeEmail, false))
		assert.Equal(t, modeEmail, nextMode(modeMeetings, false))
	})
}

func TestMeetingMessageFilter(t *testing.T) {
	sourceID := int64(42)
	model := NewBuilder().Build()
	model.meetingState.sourceID = &sourceID

	filter := model.meetingMessageFilter()

	assert.Equal(t, "meeting_transcript", filter.MessageType)
	require.NotNil(t, filter.SourceID)
	assert.Equal(t, sourceID, *filter.SourceID)
	assert.Equal(t, query.MessageSortByDate, filter.Sorting.Field)
	assert.Equal(t, query.SortDesc, filter.Sorting.Direction)
}

func TestMeetingAccountsExcludeUnrelatedSources(t *testing.T) {
	model := NewBuilder().WithAccounts(
		query.AccountInfo{ID: 1, SourceType: "gmail", Identifier: "user@example.com"},
		query.AccountInfo{ID: 2, SourceType: "granola", Identifier: "work-notes"},
		query.AccountInfo{ID: 3, SourceType: "circleback", Identifier: "team-meetings"},
		query.AccountInfo{ID: 4, SourceType: "teams", Identifier: "team-chat"},
	).Build()

	accounts := model.meetingAccounts()

	require.Len(t, accounts, 2)
	assert.Equal(t, []string{"work-notes", "team-meetings"}, []string{
		accounts[0].Identifier,
		accounts[1].Identifier,
	})
}

func TestMeetingAccountSelectorUsesMeetingSources(t *testing.T) {
	selectedID := int64(3)
	model := NewBuilder().WithAccounts(
		query.AccountInfo{ID: 1, SourceType: "gmail", Identifier: "user@example.com"},
		query.AccountInfo{ID: 2, SourceType: "granola", Identifier: "work-notes"},
		query.AccountInfo{ID: 3, SourceType: "circleback", Identifier: "team-meetings"},
	).Build()
	model.mode = modeMeetings
	model.meetingState.sourceID = &selectedID

	model.openAccountSelector()

	assert.Equal(t, 2, model.modalCursor, "selected meeting source follows All Sources")
	view := stripANSI(model.renderAccountSelectorModal())
	assert.Contains(t, view, "Select Source")
	assert.Contains(t, view, "All Sources")
	assert.Contains(t, view, "work-notes")
	assert.Contains(t, view, "team-meetings")
	assert.NotContains(t, view, "user@example.com")
}

func TestMeetingAccountSelectionDoesNotReplaceEmailFilter(t *testing.T) {
	emailID := int64(1)
	model := NewBuilder().WithAccounts(
		query.AccountInfo{ID: emailID, SourceType: "gmail", Identifier: "user@example.com"},
		query.AccountInfo{ID: 2, SourceType: "granola", Identifier: "work-notes"},
	).Build()
	model.mode = modeMeetings
	model.accountFilter = &emailID
	model.modal = modalAccountSelector
	model.modalCursor = 1

	updatedModel, _ := applyModalKey(t, model, keyEnter())

	require.NotNil(t, updatedModel.meetingState.sourceID)
	assert.Equal(t, int64(2), *updatedModel.meetingState.sourceID)
	require.NotNil(t, updatedModel.accountFilter)
	assert.Equal(t, emailID, *updatedModel.accountFilter)
}

func TestLoadMeetingMessagesUsesMeetingScope(t *testing.T) {
	sourceID := int64(7)
	var captured query.MessageFilter
	engine := &querytest.MockEngine{}
	engine.ListMessagesFunc = func(_ context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
		captured = filter
		return []query.MessageSummary{{ID: 10, Subject: "Planning"}}, nil
	}
	model := New(engine, Options{})
	model.meetingState.sourceID = &sourceID

	msg := model.loadMeetingMessages()()

	loaded, ok := msg.(meetingMessagesLoadedMsg)
	require.True(t, ok)
	require.NoError(t, loaded.err)
	require.Len(t, loaded.messages, 1)
	assert.Equal(t, meetingMessageType, captured.MessageType)
	require.NotNil(t, captured.SourceID)
	assert.Equal(t, sourceID, *captured.SourceID)
}

func TestMeetingLoadDoesNotReplaceEmailState(t *testing.T) {
	model := NewBuilder().WithMessages(query.MessageSummary{ID: 1, Subject: "Email"}).Build()
	model.mode = modeMeetings
	model.meetingState.requestID = 4

	updatedModel, _ := model.Update(meetingMessagesLoadedMsg{
		messages:  []query.MessageSummary{{ID: 2, Subject: "Planning"}},
		requestID: 4,
	})
	updated, ok := updatedModel.(Model)
	require.True(t, ok)

	require.Len(t, updated.meetingState.messages, 1)
	assert.Equal(t, "Planning", updated.meetingState.messages[0].Subject)
	require.Len(t, updated.messages, 1)
	assert.Equal(t, "Email", updated.messages[0].Subject)
}

func TestModeKeyStartsMeetingLoad(t *testing.T) {
	model := NewBuilder().Build()
	model.textEngine = nil

	updated, cmd, handled := model.handleGlobalKeys(tea.KeyPressMsg{Code: 'm', Text: "m"})

	assert.True(t, handled)
	assert.Equal(t, modeMeetings, updated.mode)
	assert.True(t, updated.loading)
	assert.NotNil(t, cmd)
}

func TestModeKeyReachesMeetings(t *testing.T) {
	t.Run("skips unavailable texts", func(t *testing.T) {
		model := NewBuilder().Build()
		model.textEngine = nil

		updated, _, handled := model.handleGlobalKeys(tea.KeyPressMsg{Code: 'm', Text: "m"})

		assert.True(t, handled)
		assert.Equal(t, modeMeetings, updated.mode)
	})

	t.Run("cycles after texts", func(t *testing.T) {
		model := NewBuilder().Build()
		model.mode = modeTexts

		updated, _, handled := model.handleGlobalKeys(tea.KeyPressMsg{Code: 'm', Text: "m"})

		assert.True(t, handled)
		assert.Equal(t, modeMeetings, updated.mode)
	})
}
