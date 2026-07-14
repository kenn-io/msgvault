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

func TestLoadMeetingMessagesUsesRequestedPage(t *testing.T) {
	var captured query.MessageFilter
	engine := &querytest.MockEngine{}
	engine.ListMessagesFunc = func(_ context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
		captured = filter
		return []query.MessageSummary{{ID: 11}}, nil
	}
	model := New(engine, Options{})

	msg := model.loadMeetingMessagesWithOffset(messageListPageSize, true)()

	loaded, ok := msg.(meetingMessagesLoadedMsg)
	require.True(t, ok)
	assert.True(t, loaded.append)
	assert.Equal(t, messageListPageSize, captured.Pagination.Offset)
	assert.Equal(t, messageListPageSize, captured.Pagination.Limit)
}

func TestMeetingMessagePagesAppend(t *testing.T) {
	model := NewBuilder().Build()
	model.mode = modeMeetings
	model.meetingState.requestID = 3
	model.meetingState.messages = []query.MessageSummary{{ID: 1}}

	updatedModel, _ := model.Update(meetingMessagesLoadedMsg{
		messages:  []query.MessageSummary{{ID: 2}},
		requestID: 3,
		append:    true,
	})
	updated, ok := updatedModel.(Model)
	require.True(t, ok)

	assert.Equal(t, []int64{1, 2}, []int64{
		updated.meetingState.messages[0].ID,
		updated.meetingState.messages[1].ID,
	})
}

func TestMeetingNavigationLoadsNextPage(t *testing.T) {
	t.Run("meeting list", func(t *testing.T) {
		model := NewBuilder().Build()
		model.mode = modeMeetings
		model.meetingState.messages = []query.MessageSummary{{ID: 1}}
		model.meetingState.cursor = 0
		model.meetingState.listComplete = false

		updated, cmd := sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})

		assert.True(t, updated.meetingState.listLoadingMore)
		assert.NotNil(t, cmd)
	})

	t.Run("search results", func(t *testing.T) {
		model := NewBuilder().Build()
		model.mode = modeMeetings
		model.meetingState.messages = []query.MessageSummary{{ID: 1}}
		model.meetingState.cursor = 0
		model.meetingState.searchQuery = "roadmap"
		model.meetingState.searchComplete = false

		updated, cmd := sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyPgDown})

		assert.True(t, updated.meetingState.listLoadingMore)
		assert.NotNil(t, cmd)
	})
}

func TestMeetingSortKeysReloadList(t *testing.T) {
	model := NewBuilder().Build()
	model.mode = modeMeetings

	sorted, sortCmd := sendKey(t, model, key('s'))

	assert.Equal(t, query.MessageSortBySubject, sorted.meetingState.sortField)
	assert.NotNil(t, sortCmd)

	reversed, reverseCmd := sendKey(t, sorted, key('r'))

	assert.Equal(t, query.SortAsc, reversed.meetingState.sortDirection)
	assert.NotNil(t, reverseCmd)
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

func TestModeKeyRestoresInitializedMeetingState(t *testing.T) {
	model := NewBuilder().Build()
	model.textEngine = nil
	model.mode = modeEmail
	model.meetingState.initialized = true
	model.meetingState.searchQuery = "roadmap"
	model.meetingState.messages = []query.MessageSummary{{ID: 2, Subject: "Planning"}}

	updated, cmd, handled := model.handleGlobalKeys(tea.KeyPressMsg{Code: 'm', Text: "m"})

	assert.True(t, handled)
	assert.Equal(t, modeMeetings, updated.mode)
	assert.Nil(t, cmd, "restoring Meetings should not overwrite its independent state")
	assert.Equal(t, "roadmap", updated.meetingState.searchQuery)
	require.Len(t, updated.meetingState.messages, 1)
}

func TestMeetingKeysUseIndependentCursor(t *testing.T) {
	model := NewBuilder().Build()
	model.mode = modeMeetings
	model.meetingState.messages = []query.MessageSummary{{ID: 1}, {ID: 2}}

	updated, _ := sendKey(t, model, key('j'))

	assert.Equal(t, 1, updated.meetingState.cursor)
	assert.Zero(t, updated.cursor, "email cursor remains unchanged")
}

func TestMeetingModeIgnoresMutationKeys(t *testing.T) {
	model := NewBuilder().Build()
	model.mode = modeMeetings
	model.meetingState.messages = []query.MessageSummary{{ID: 1}}

	for _, keyMsg := range []tea.KeyPressMsg{
		{Code: ' '},
		{Code: 'S', Text: "S"},
		{Code: 'd', Text: "d"},
		{Code: 'D', Text: "D"},
		{Code: 'x', Text: "x"},
	} {
		updated, cmd := sendKey(t, model, keyMsg)
		assert.Nil(t, cmd, "key %q must not start a mutation", keyMsg.String())
		assert.Equal(t, modalNone, updated.modal, "key %q must not open a mutation modal", keyMsg.String())
		assert.Empty(t, updated.selection.messageIDs, "key %q must not select messages", keyMsg.String())
	}
}

func TestMeetingModeOpensSourceSelector(t *testing.T) {
	model := NewBuilder().WithAccounts(
		query.AccountInfo{ID: 1, SourceType: "gmail", Identifier: "user@example.com"},
		query.AccountInfo{ID: 2, SourceType: "granola", Identifier: "work-notes"},
	).Build()
	model.mode = modeMeetings

	updated, cmd := sendKey(t, model, key('A'))

	assert.Nil(t, cmd)
	assert.Equal(t, modalAccountSelector, updated.modal)
	assert.Len(t, updated.selectableAccounts(), 1)
}

func TestMeetingDetailFlowRestoresListPosition(t *testing.T) {
	model := NewBuilder().Build()
	model.mode = modeMeetings
	model.meetingState.messages = []query.MessageSummary{{ID: 1}, {ID: 2, Subject: "Planning"}}
	model.meetingState.cursor = 1

	openedModel, cmd := sendKey(t, model, keyEnter())

	assert.Equal(t, meetingLevelDetail, openedModel.meetingState.level)
	assert.NotNil(t, cmd)
	requestID := openedModel.meetingState.detailRequestID

	loadedModel, _ := openedModel.Update(meetingDetailLoadedMsg{
		detail:    &query.MessageDetail{ID: 2, Subject: "Planning", BodyText: "Transcript"},
		requestID: requestID,
	})
	loaded, ok := loadedModel.(Model)
	require.True(t, ok)
	require.NotNil(t, loaded.meetingState.detail)

	returned, _ := sendKey(t, loaded, keyEsc())

	assert.Equal(t, meetingLevelList, returned.meetingState.level)
	assert.Equal(t, 1, returned.meetingState.cursor)
}

func TestMeetingDetailFindJumpsToTranscriptMatch(t *testing.T) {
	model := NewBuilder().WithSize(80, 12).Build()
	model.mode = modeMeetings
	model.meetingState.level = meetingLevelDetail
	model.meetingState.detail = &query.MessageDetail{
		Subject:  "Planning",
		BodyText: "first line\nsecond line\nneedle in transcript\nlast line",
	}

	searching, focusCmd := sendKey(t, model, key('/'))
	assert.True(t, searching.meetingState.detailSearchActive)
	assert.NotNil(t, focusCmd)
	searching.meetingState.detailSearchInput.SetValue("needle")

	matched, _ := sendKey(t, searching, keyEnter())

	assert.Equal(t, "needle", matched.meetingState.detailSearchQuery)
	assert.NotEmpty(t, matched.meetingState.detailSearchMatches)
	assert.Positive(t, matched.meetingState.detailScroll)
}

func TestMeetingDetailNavigationRecomputesFindMatches(t *testing.T) {
	model := NewBuilder().Build()
	model.mode = modeMeetings
	model.meetingState.level = meetingLevelDetail
	model.meetingState.detailRequestID = 5
	model.meetingState.detailSearchQuery = "needle"

	updatedModel, _ := model.Update(meetingDetailLoadedMsg{
		detail:    &query.MessageDetail{Subject: "Next", BodyText: "new needle occurrence"},
		requestID: 5,
	})
	updated, ok := updatedModel.(Model)
	require.True(t, ok)

	assert.NotEmpty(t, updated.meetingState.detailSearchMatches)
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
