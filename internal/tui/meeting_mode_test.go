package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
)

type meetingModeTextEngine struct{}

func (meetingModeTextEngine) ListConversations(
	context.Context, query.TextFilter,
) ([]query.ConversationRow, error) {
	return []query.ConversationRow{{ConversationID: 77}}, nil
}

func (meetingModeTextEngine) TextAggregate(
	context.Context, query.TextViewType, query.TextAggregateOptions,
) ([]query.AggregateRow, error) {
	return nil, nil
}

func (meetingModeTextEngine) ListConversationMessages(
	context.Context, int64, query.TextFilter,
) ([]query.MessageSummary, error) {
	return nil, nil
}

func (meetingModeTextEngine) TextSearch(
	context.Context, string, int, int,
) ([]query.MessageSummary, error) {
	return nil, nil
}

func (meetingModeTextEngine) GetTextStats(
	context.Context, query.TextStatsOptions,
) (*query.TotalStats, error) {
	return &query.TotalStats{MessageCount: 1}, nil
}

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
		query.AccountInfo{ID: 2, SourceType: meetingSourceGranola, Identifier: "work-notes"},
		query.AccountInfo{ID: 3, SourceType: meetingSourceCircleback, Identifier: "team-meetings"},
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
		query.AccountInfo{ID: 2, SourceType: meetingSourceGranola, Identifier: "work-notes"},
		query.AccountInfo{ID: 3, SourceType: meetingSourceCircleback, Identifier: "team-meetings"},
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
		query.AccountInfo{ID: 2, SourceType: meetingSourceGranola, Identifier: "work-notes"},
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
		{ID: oldSourceID, SourceType: meetingSourceGranola, Identifier: "old-source"},
		{ID: newSourceID, SourceType: meetingSourceCircleback, Identifier: "new-source"},
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

func TestMeetingEmptySearchAfterSourceChangeReloadsSelectedSourceList(t *testing.T) {
	for _, tc := range []struct {
		name          string
		completeRerun bool
	}{
		{name: "before rerun completes"},
		{name: "after rerun completes", completeRerun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			oldSourceID := int64(7)
			newSourceID := int64(8)
			var listedFilter query.MessageFilter
			engine := &querytest.MockEngine{}
			engine.SearchFunc = func(_ context.Context, _ *search.Query, _, _ int) ([]query.MessageSummary, error) {
				return []query.MessageSummary{{ID: 80, Subject: "New source result"}}, nil
			}
			engine.ListMessagesFunc = func(_ context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
				listedFilter = filter
				return []query.MessageSummary{{ID: 81, Subject: "New source list"}}, nil
			}
			model := New(engine, Options{})
			model.mode = modeMeetings
			model.accounts = []query.AccountInfo{
				{ID: oldSourceID, SourceType: meetingSourceGranola, Identifier: "old-source"},
				{ID: newSourceID, SourceType: meetingSourceCircleback, Identifier: "new-source"},
			}
			model.meetingState.sourceID = &oldSourceID
			model.meetingState.searchQuery = "roadmap"
			model.meetingState.preSearch = []query.MessageSummary{{ID: 7, Subject: "Old source list"}}
			model.meetingState.messages = []query.MessageSummary{{ID: 70, Subject: "Old source result"}}
			model.meetingState.requestID = 4
			model.meetingState.searchRequestID = 9
			model.modal = modalAccountSelector
			model.modalCursor = 2

			updated, rerunCmd := applyModalKey(t, model, keyEnter())
			if tc.completeRerun {
				for _, msg := range runBatchCommand(t, rerunCmd) {
					if loaded, ok := msg.(meetingSearchLoadedMsg); ok {
						updated = sendMsg(t, updated, loaded)
					}
				}
			}
			requestIDBeforeClear := updated.meetingState.searchRequestID

			updated, _ = sendKey(t, updated, key('/'))
			require.True(updated.meetingState.searchActive)
			updated.meetingState.searchInput.SetValue("")
			updated, cmd := sendKey(t, updated, keyEnter())

			assert.Empty(updated.meetingState.searchQuery)
			assert.Greater(updated.meetingState.searchRequestID, requestIDBeforeClear,
				"empty query must invalidate the source-scoped search")
			require.NotNil(cmd, "empty query must reload the selected source list")
			foundList := false
			for _, msg := range runBatchCommand(t, cmd) {
				if _, ok := msg.(meetingMessagesLoadedMsg); ok {
					foundList = true
				}
			}
			require.True(foundList)
			require.NotNil(listedFilter.SourceID)
			assert.Equal(newSourceID, *listedFilter.SourceID)
			assert.Nil(updated.meetingState.preSearch)
		})
	}
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

func TestMeetingListResponseOutsideMeetingModePreservesGlobalUIState(t *testing.T) {
	t.Run("successful response is cached", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		existingErr := errors.New("email request still loading")
		model := NewBuilder().Build()
		model.mode = modeEmail
		model.loading = true
		model.err = existingErr
		model.modal = modalHelp
		model.modalResult = "email help"
		model.meetingState.requestID = 4
		model.meetingState.listLoadingMore = true

		updatedModel, _ := model.Update(meetingMessagesLoadedMsg{
			messages:  []query.MessageSummary{{ID: 2, Subject: "Planning"}},
			requestID: 4,
		})
		updated, ok := updatedModel.(Model)
		require.True(ok)

		assert.True(updated.loading)
		require.ErrorIs(updated.err, existingErr)
		assert.Equal(modalHelp, updated.modal)
		assert.Equal("email help", updated.modalResult)
		assert.False(updated.meetingState.listLoadingMore)
		assert.True(updated.meetingState.initialized)
		require.Len(updated.meetingState.messages, 1)
		assert.Equal("Planning", updated.meetingState.messages[0].Subject)
	})

	t.Run("failed response stays scoped to meetings", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		existingErr := errors.New("email request still loading")
		model := NewBuilder().Build()
		model.mode = modeEmail
		model.loading = true
		model.err = existingErr
		model.modal = modalHelp
		model.modalResult = "email help"
		model.meetingState.requestID = 4
		model.meetingState.listLoadingMore = true

		updatedModel, _ := model.Update(meetingMessagesLoadedMsg{
			err:       errors.New("meeting load failed"),
			requestID: 4,
		})
		updated, ok := updatedModel.(Model)
		require.True(ok)

		assert.True(updated.loading)
		require.ErrorIs(updated.err, existingErr)
		assert.Equal(modalHelp, updated.modal)
		assert.Equal("email help", updated.modalResult)
		assert.False(updated.meetingState.listLoadingMore)
	})
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

func TestModeKeyClearsPreviousModePresentationState(t *testing.T) {
	assert := assert.New(t)
	model := NewBuilder().Build()
	model.textEngine = nil
	model.transitionBuffer = "frozen email view"
	model.inlineSearchLoading = true
	model.searchLoadingMore = true

	updated, _, handled := model.handleGlobalKeys(tea.KeyPressMsg{Code: 'm', Text: "m"})

	assert.True(handled)
	assert.Equal(modeMeetings, updated.mode)
	assert.Empty(updated.transitionBuffer)
	assert.False(updated.inlineSearchLoading)
	assert.False(updated.searchLoadingMore)
	assert.True(updated.loading, "Meetings load owns the shared loading indicator")
}

func TestModeSwitchScopesPreviousModeCompletions(t *testing.T) {
	previousErr := errors.New("previous mode failed")

	for _, origin := range []struct {
		name    string
		mode    tuiMode
		message func(Model) tea.Msg
	}{
		{
			name: "email",
			mode: modeEmail,
			message: func(model Model) tea.Msg {
				return dataLoadedMsg{
					err:       previousErr,
					requestID: model.aggregateRequestID,
				}
			},
		},
		{
			name: "texts",
			mode: modeTexts,
			message: func(Model) tea.Msg {
				return textConversationsLoadedMsg{err: previousErr}
			},
		},
	} {
		for _, previousCompletesFirst := range []bool{true, false} {
			orderName := "meetings completes first"
			if previousCompletesFirst {
				orderName = "previous mode completes first"
			}
			t.Run(origin.name+"/"+orderName, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				model := NewBuilder().Build()
				model.mode = origin.mode
				if origin.mode == modeEmail {
					model.textEngine = nil
				}
				model.loading = true

				switched, _, handled := model.handleGlobalKeys(tea.KeyPressMsg{Code: 'm', Text: "m"})
				require.True(handled)
				require.Equal(modeMeetings, switched.mode)

				completePrevious := func(current Model) Model {
					updatedModel, _ := current.Update(origin.message(current))
					updated, ok := updatedModel.(Model)
					require.True(ok)
					return updated
				}

				completeMeeting := func(current Model) Model {
					updatedModel, _ := current.Update(meetingMessagesLoadedMsg{
						messages:               []query.MessageSummary{{ID: 42, Subject: "Planning"}},
						requestID:              current.meetingState.requestID,
						presentationGeneration: current.presentationGeneration,
					})
					updated, ok := updatedModel.(Model)
					require.True(ok)
					return updated
				}

				if previousCompletesFirst {
					switched = completePrevious(switched)
					assert.True(switched.loading, "previous mode must not stop Meetings loading")
					require.NoError(switched.err)
					assert.Equal(modalNone, switched.modal)
					switched = completeMeeting(switched)
				} else {
					switched = completeMeeting(switched)
					require.False(switched.loading)
					switched = completePrevious(switched)
				}

				assert.False(switched.loading)
				require.NoError(switched.err)
				assert.Equal(modalNone, switched.modal)
				require.Len(switched.meetingState.messages, 1)
				assert.Equal(int64(42), switched.meetingState.messages[0].ID)
			})
		}
	}
}

func TestModeCycleRejectsStalePresentationCompletions(t *testing.T) {
	detail := &query.MessageDetail{ID: 42, Subject: "Planning"}
	meeting := query.MessageSummary{ID: 84, Subject: "Weekly sync"}
	staleErr := errors.New("stale activation failed")

	for _, scenario := range []struct {
		name         string
		mode         tuiMode
		capture      func(Model) tea.Cmd
		assertCached func(*assert.Assertions, *require.Assertions, Model)
	}{
		{
			name: "email detail",
			mode: modeEmail,
			capture: func(model Model) tea.Cmd {
				return model.loadMessageDetail(detail.ID)
			},
			assertCached: func(assert *assert.Assertions, require *require.Assertions, model Model) {
				require.NotNil(model.messageDetail)
				assert.Equal(detail.ID, model.messageDetail.ID)
			},
		},
		{
			name: "text conversations",
			mode: modeTexts,
			capture: func(model Model) tea.Cmd {
				return model.loadTextConversations()
			},
			assertCached: func(assert *assert.Assertions, require *require.Assertions, model Model) {
				require.Len(model.textState.conversations, 1)
				assert.Equal(int64(77), model.textState.conversations[0].ConversationID)
			},
		},
		{
			name: "meeting search",
			mode: modeMeetings,
			capture: func(model Model) tea.Cmd {
				return model.loadMeetingSearch("weekly", 0, false)
			},
			assertCached: func(assert *assert.Assertions, require *require.Assertions, model Model) {
				require.Len(model.meetingState.messages, 1)
				assert.Equal(meeting.ID, model.meetingState.messages[0].ID)
			},
		},
	} {
		for _, staleFailure := range []bool{false, true} {
			completion := "success"
			if staleFailure {
				completion = "error"
			}
			t.Run(scenario.name+"/"+completion, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				model := NewBuilder().WithDetail(detail).WithMessages(meeting).Build()
				model.messageDetail = nil
				model.textEngine = meetingModeTextEngine{}
				model.mode = scenario.mode

				staleMsg := scenario.capture(model)()
				if staleFailure {
					staleMsg = modeCompletionWithError(t, staleMsg, staleErr)
				}

				for range 3 {
					var handled bool
					model, _, handled = model.handleGlobalKeys(tea.KeyPressMsg{Code: 'm', Text: "m"})
					require.True(handled)
				}
				require.Equal(scenario.mode, model.mode)
				require.True(model.loading)

				currentMsg := scenario.capture(model)()
				model.transitionBuffer = "current activation"
				model = sendMsg(t, model, staleMsg)

				assert.True(model.loading, "stale completion must not stop the current load")
				assert.Equal("current activation", model.transitionBuffer)
				require.NoError(model.err)
				assert.Equal(modalNone, model.modal)
				if !staleFailure {
					scenario.assertCached(assert, require, model)
				}

				model = sendMsg(t, model, currentMsg)
				assert.False(model.loading)
				assert.Empty(model.transitionBuffer)
				require.NoError(model.err)
				assert.Equal(modalNone, model.modal)
			})
		}
	}
}

func modeCompletionWithError(t *testing.T, msg tea.Msg, err error) tea.Msg {
	t.Helper()
	switch msg := msg.(type) {
	case messageDetailLoadedMsg:
		msg.detail = nil
		msg.err = err
		return msg
	case textConversationsLoadedMsg:
		msg.conversations = nil
		msg.stats = nil
		msg.err = err
		return msg
	case meetingSearchLoadedMsg:
		msg.messages = nil
		msg.err = err
		return msg
	default:
		require.FailNow(t, "unsupported completion type", "%T", msg)
		return nil
	}
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
		query.AccountInfo{ID: 2, SourceType: meetingSourceGranola, Identifier: "work-notes"},
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

func TestMeetingDetailScrollingClampsAtRenderedMaximum(t *testing.T) {
	newModel := func() Model {
		model := NewBuilder().WithSize(40, 12).Build()
		model.mode = modeMeetings
		model.meetingState.level = meetingLevelDetail
		model.meetingState.detail = &query.MessageDetail{
			Subject:  "Planning",
			BodyText: strings.Repeat("Transcript line with enough text to wrap.\n", 20),
		}
		return model
	}

	for _, test := range []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{name: "down", key: keyDown()},
		{name: "page down", key: tea.KeyPressMsg{Code: tea.KeyPgDown}},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := newModel()
			maxScroll := max(len(model.meetingDetailLines())-model.detailPageSize(), 0)
			require.Positive(t, maxScroll)

			for range maxScroll + 20 {
				model, _ = sendKey(t, model, test.key)
			}

			assert.Equal(t, maxScroll, model.meetingState.detailScroll)
		})
	}
}

func TestMeetingDetailResizeClampsScrollAndRecomputesFindMatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	model := NewBuilder().WithSize(80, 12).Build()
	model.mode = modeMeetings
	model.meetingState.level = meetingLevelDetail
	model.meetingState.detail = &query.MessageDetail{
		Subject: "Planning",
		BodyText: strings.Repeat("context before the search result ", 8) +
			"needle appears after text that wraps differently",
	}
	model.meetingState.detailSearchQuery = "needle"
	model.findMeetingDetailMatches()
	require.NotEmpty(model.meetingState.detailSearchMatches)
	wideMatch := model.meetingState.detailSearchMatches[0]
	model.meetingState.detailScroll = 1000

	resized := resizeModel(t, model, 28, 12)
	lines := plainMarkdownLines(resized.meetingDetailLines())
	expectedMatches := make([]int, 0, 1)
	for index, line := range lines {
		if strings.Contains(strings.ToLower(line), "needle") {
			expectedMatches = append(expectedMatches, index)
		}
	}
	require.NotEmpty(expectedMatches)
	assert.NotEqual(wideMatch, expectedMatches[0], "test fixture must rewrap the match")
	assert.Equal(expectedMatches, resized.meetingState.detailSearchMatches)
	maxScroll := max(len(lines)-resized.detailPageSize(), 0)
	assert.LessOrEqual(resized.meetingState.detailScroll, maxScroll)
}

func TestMeetingDetailFindIgnoresMarkdownANSISequences(t *testing.T) {
	forceColorProfile(t)
	model := NewBuilder().WithSize(80, 12).Build()
	model.mode = modeMeetings
	model.meetingState.level = meetingLevelDetail
	model.meetingState.detail = &query.MessageDetail{
		Subject:  "Planning",
		BodyText: "alpha\n\nbeta",
	}
	model.markdownCache = newMarkdownCache(true, false)
	model.meetingState.detailSearchQuery = "38"

	model.findMeetingDetailMatches()

	assert.Empty(t, model.meetingState.detailSearchMatches)
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
