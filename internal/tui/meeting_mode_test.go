package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
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
	assert := assert.New(t)
	sourceID := int64(42)
	model := NewBuilder().Build()
	model.meetingState.sourceID = &sourceID

	filter := model.meetingMessageFilter()

	assert.Equal("meeting_transcript", filter.MessageType)
	require.NotNil(t, filter.SourceID)
	assert.Equal(sourceID, *filter.SourceID)
	assert.Equal(query.MessageSortByDate, filter.Sorting.Field)
	assert.Equal(query.SortDesc, filter.Sorting.Direction)
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
	assert := assert.New(t)
	selectedID := int64(3)
	model := NewBuilder().WithAccounts(
		query.AccountInfo{ID: 1, SourceType: "gmail", Identifier: "user@example.com"},
		query.AccountInfo{ID: 2, SourceType: "granola", Identifier: "work-notes"},
		query.AccountInfo{ID: 3, SourceType: "circleback", Identifier: "team-meetings"},
	).Build()
	model.mode = modeMeetings
	model.meetingState.sourceID = &selectedID

	model.openAccountSelector()

	assert.Equal(2, model.modalCursor, "selected meeting source follows All Sources")
	view := stripANSI(model.renderAccountSelectorModal())
	assert.Contains(view, "Select Source")
	assert.Contains(view, "All Sources")
	assert.Contains(view, "work-notes")
	assert.Contains(view, "team-meetings")
	assert.NotContains(view, "user@example.com")
}

func TestMeetingAccountSelectionDoesNotReplaceEmailFilter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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

	require.NotNil(updatedModel.meetingState.sourceID)
	assert.Equal(int64(2), *updatedModel.meetingState.sourceID)
	require.NotNil(updatedModel.accountFilter)
	assert.Equal(emailID, *updatedModel.accountFilter)
}

func TestMeetingAccountSelectionRerunsActiveSearchForNewSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	oldSourceID := int64(7)
	newSourceID := int64(8)
	var captured *search.Query
	engine := &querytest.MockEngine{}
	engine.SearchFunc = func(_ context.Context, q *search.Query, _, _ int) ([]query.MessageSummary, error) {
		captured = q
		return []query.MessageSummary{{ID: 80, Subject: "New source result"}}, nil
	}
	model := New(engine, Options{})
	model.mode = modeMeetings
	model.accounts = []query.AccountInfo{
		{ID: oldSourceID, SourceType: "granola", Identifier: "old-source"},
		{ID: newSourceID, SourceType: "circleback", Identifier: "new-source"},
	}
	model.meetingState.sourceID = &oldSourceID
	model.meetingState.searchQuery = "roadmap"
	model.meetingState.preSearch = []query.MessageSummary{{ID: 7, Subject: "Old source list"}}
	model.meetingState.messages = []query.MessageSummary{{ID: 70, Subject: "Old source result"}}
	model.meetingState.requestID = 4
	model.meetingState.searchRequestID = 9
	model.meetingState.searchOffset = 25
	model.meetingState.searchComplete = true
	model.meetingState.listLoadingMore = true
	model.modal = modalAccountSelector
	model.modalCursor = 2

	updated, cmd := applyModalKey(t, model, keyEnter())

	require.NotNil(updated.meetingState.sourceID)
	assert.Equal(newSourceID, *updated.meetingState.sourceID)
	assert.Equal(uint64(5), updated.meetingState.requestID, "invalidate in-flight list")
	assert.Equal(uint64(10), updated.meetingState.searchRequestID, "invalidate in-flight search")
	assert.Equal("roadmap", updated.meetingState.searchQuery)
	assert.Nil(updated.meetingState.preSearch, "old source list cannot be restored")
	assert.Zero(updated.meetingState.searchOffset)
	assert.False(updated.meetingState.searchComplete)
	assert.False(updated.meetingState.listLoadingMore)

	msgs := runBatchCommand(t, cmd)
	var loaded meetingSearchLoadedMsg
	foundSearch := false
	for _, msg := range msgs {
		if candidate, ok := msg.(meetingSearchLoadedMsg); ok {
			loaded = candidate
			foundSearch = true
		}
	}
	require.True(foundSearch, "source change should rerun the active meeting search")
	assert.Equal(uint64(10), loaded.requestID)
	assert.Zero(loaded.offset)
	require.NotNil(captured)
	assert.Equal([]int64{newSourceID}, captured.AccountIDs)
	assert.Equal([]string{meetingMessageType}, captured.MessageTypes)

	updated = sendMsg(t, updated, meetingMessagesLoadedMsg{
		messages:  []query.MessageSummary{{ID: 71, Subject: "Stale list"}},
		requestID: 4,
	})
	updated = sendMsg(t, updated, meetingSearchLoadedMsg{
		messages:  []query.MessageSummary{{ID: 72, Subject: "Stale search"}},
		requestID: 9,
	})
	require.Len(updated.meetingState.messages, 1)
	assert.Equal(int64(70), updated.meetingState.messages[0].ID, "stale responses must be ignored")

	updated = sendMsg(t, updated, loaded)
	require.Len(updated.meetingState.messages, 1)
	assert.Equal(int64(80), updated.meetingState.messages[0].ID)
}

func TestMeetingEscapeReloadsListWhenSearchSourceChanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	sourceID := int64(8)
	var captured query.MessageFilter
	engine := &querytest.MockEngine{}
	engine.ListMessagesFunc = func(_ context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
		captured = filter
		return []query.MessageSummary{{ID: 81, Subject: "New source list"}}, nil
	}
	model := New(engine, Options{})
	model.mode = modeMeetings
	model.meetingState.sourceID = &sourceID
	model.meetingState.searchQuery = "roadmap"
	model.meetingState.searchInput.SetValue("roadmap")
	model.meetingState.messages = []query.MessageSummary{{ID: 80, Subject: "Search result"}}
	model.meetingState.preSearch = nil

	updated, cmd := sendKey(t, model, keyEsc())

	assert.Empty(updated.meetingState.searchQuery)
	require.NotNil(cmd, "clearing search should load the selected source's normal list")
	msgs := runBatchCommand(t, cmd)
	foundList := false
	for _, msg := range msgs {
		if _, ok := msg.(meetingMessagesLoadedMsg); ok {
			foundList = true
		}
	}
	require.True(foundList)
	require.NotNil(captured.SourceID)
	assert.Equal(sourceID, *captured.SourceID)
}

func runBatchCommand(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	require.NotNil(t, cmd)
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	msgs := make([]tea.Msg, 0, len(batch))
	for _, child := range batch {
		msgs = append(msgs, child())
	}
	return msgs
}

func TestLoadMeetingMessagesUsesMeetingScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require.True(ok)
	require.NoError(loaded.err)
	require.Len(loaded.messages, 1)
	assert.Equal(meetingMessageType, captured.MessageType)
	require.NotNil(captured.SourceID)
	assert.Equal(sourceID, *captured.SourceID)
}

func TestLoadMeetingMessagesUsesRequestedPage(t *testing.T) {
	assert := assert.New(t)
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
	assert.True(loaded.append)
	assert.Equal(messageListPageSize, captured.Pagination.Offset)
	assert.Equal(messageListPageSize, captured.Pagination.Limit)
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
	assert := assert.New(t)
	model := NewBuilder().Build()
	model.mode = modeMeetings

	sorted, sortCmd := sendKey(t, model, key('s'))

	assert.Equal(query.MessageSortBySubject, sorted.meetingState.sortField)
	assert.NotNil(sortCmd)

	reversed, reverseCmd := sendKey(t, sorted, key('r'))

	assert.Equal(query.SortAsc, reversed.meetingState.sortDirection)
	assert.NotNil(reverseCmd)
}

func TestMeetingLoadDoesNotReplaceEmailState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	model := NewBuilder().WithMessages(query.MessageSummary{ID: 1, Subject: "Email"}).Build()
	model.mode = modeMeetings
	model.meetingState.requestID = 4

	updatedModel, _ := model.Update(meetingMessagesLoadedMsg{
		messages:  []query.MessageSummary{{ID: 2, Subject: "Planning"}},
		requestID: 4,
	})
	updated, ok := updatedModel.(Model)
	require.True(ok)

	require.Len(updated.meetingState.messages, 1)
	assert.Equal("Planning", updated.meetingState.messages[0].Subject)
	require.Len(updated.messages, 1)
	assert.Equal("Email", updated.messages[0].Subject)
}

func TestModeKeyStartsMeetingLoad(t *testing.T) {
	assert := assert.New(t)
	model := NewBuilder().Build()
	model.textEngine = nil

	updated, cmd, handled := model.handleGlobalKeys(tea.KeyPressMsg{Code: 'm', Text: "m"})

	assert.True(handled)
	assert.Equal(modeMeetings, updated.mode)
	assert.True(updated.loading)
	assert.NotNil(cmd)
}

func TestModeKeyRestoresInitializedMeetingState(t *testing.T) {
	assert := assert.New(t)
	model := NewBuilder().Build()
	model.textEngine = nil
	model.mode = modeEmail
	model.meetingState.initialized = true
	model.meetingState.searchQuery = "roadmap"
	model.meetingState.messages = []query.MessageSummary{{ID: 2, Subject: "Planning"}}

	updated, cmd, handled := model.handleGlobalKeys(tea.KeyPressMsg{Code: 'm', Text: "m"})

	assert.True(handled)
	assert.Equal(modeMeetings, updated.mode)
	assert.Nil(cmd, "restoring Meetings should not overwrite its independent state")
	assert.Equal("roadmap", updated.meetingState.searchQuery)
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
	require := require.New(t)
	assert := assert.New(t)
	model := NewBuilder().Build()
	model.mode = modeMeetings
	model.meetingState.messages = []query.MessageSummary{{ID: 1}, {ID: 2, Subject: "Planning"}}
	model.meetingState.cursor = 1

	openedModel, cmd := sendKey(t, model, keyEnter())

	assert.Equal(meetingLevelDetail, openedModel.meetingState.level)
	assert.NotNil(cmd)
	requestID := openedModel.meetingState.detailRequestID

	loadedModel, _ := openedModel.Update(meetingDetailLoadedMsg{
		detail:    &query.MessageDetail{ID: 2, Subject: "Planning", BodyText: "Transcript"},
		requestID: requestID,
	})
	loaded, ok := loadedModel.(Model)
	require.True(ok)
	require.NotNil(loaded.meetingState.detail)

	returned, _ := sendKey(t, loaded, keyEsc())

	assert.Equal(meetingLevelList, returned.meetingState.level)
	assert.Equal(1, returned.meetingState.cursor)
}

func TestMeetingDetailFindJumpsToTranscriptMatch(t *testing.T) {
	assert := assert.New(t)
	model := NewBuilder().WithSize(80, 12).Build()
	model.mode = modeMeetings
	model.meetingState.level = meetingLevelDetail
	model.meetingState.detail = &query.MessageDetail{
		Subject:  "Planning",
		BodyText: "first line\nsecond line\nneedle in transcript\nlast line",
	}

	searching, focusCmd := sendKey(t, model, key('/'))
	assert.True(searching.meetingState.detailSearchActive)
	assert.NotNil(focusCmd)
	searching.meetingState.detailSearchInput.SetValue("needle")

	matched, _ := sendKey(t, searching, keyEnter())

	assert.Equal("needle", matched.meetingState.detailSearchQuery)
	assert.NotEmpty(matched.meetingState.detailSearchMatches)
	assert.Positive(matched.meetingState.detailScroll)
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
