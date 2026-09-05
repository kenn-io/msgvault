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

type reproductionCollectionScopeLister struct {
	scopes []query.CollectionScope
}

func (l reproductionCollectionScopeLister) ListCollectionScopes(_ context.Context) ([]query.CollectionScope, error) {
	return l.scopes, nil
}

func TestCollectionScopeSelectorReproduction(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var captured query.MessageFilter
	engine := newMockEngine(MockConfig{})
	engine.ListMessagesFunc = func(_ context.Context, filter query.MessageFilter) ([]query.MessageSummary, error) {
		captured = filter
		return nil, nil
	}

	model := New(engine, Options{
		DataDir: "/tmp/test",
		Version: "test",
		CollectionScopeLister: reproductionCollectionScopeLister{
			scopes: []query.CollectionScope{{Name: "Work", SourceIDs: []int64{1, 2}}},
		},
	})
	model.accounts = []query.AccountInfo{{ID: 1, Identifier: "alice@example.com"}, {ID: 2, Identifier: "bob@example.com"}}

	scopesMsg, ok := model.loadCollectionScopes()().(collectionScopesLoadedMsg)
	requirements.True(ok)
	requirements.NoError(scopesMsg.err)
	model = sendMsg(t, model, scopesMsg)
	model.openAccountSelector()

	modal := stripANSI(model.renderAccountSelectorModal())
	assertions.Contains(modal, "Work")

	model, _ = sendKey(t, model, keyDown())
	model, _ = sendKey(t, model, keyDown())
	model, _ = sendKey(t, model, keyDown())
	model, cmd := sendKey(t, model, keyEnter())
	assertions.NotNil(cmd)
	assertions.Equal("Collection: Work", model.scopeTitle())

	loaded, ok := model.loadMessages()().(messagesLoadedMsg)
	requirements.True(ok)
	requirements.NoError(loaded.err)
	assertions.Equal([]int64{1, 2}, captured.SourceIDs)
	assertions.Nil(captured.SourceID)
}

func TestCollectionScopeSourceFieldsPreserveKindsAndCopies(t *testing.T) {
	assertions := assert.New(t)
	accountID := int64(7)
	collectionIDs := []int64{7, 8}
	collection := query.CollectionScope{Name: "Work", SourceIDs: collectionIDs}

	accountFilter := query.MessageFilter{SourceID: &accountID, SourceIDs: []int64{99}}
	accountSourceScope(&accountID).apply(&accountFilter)
	assertions.Equal(&accountID, accountFilter.SourceID)
	assertions.Nil(accountFilter.SourceIDs)

	collectionFilter := query.MessageFilter{SourceID: &accountID}
	collectionSourceScope(collection).apply(&collectionFilter)
	assertions.Nil(collectionFilter.SourceID)
	assertions.Equal([]int64{7, 8}, collectionFilter.SourceIDs)
	collectionIDs[0] = 99
	assertions.Equal([]int64{7, 8}, collectionFilter.SourceIDs)

	empty := collectionSourceScope(query.CollectionScope{Name: "Empty", SourceIDs: []int64{}})
	emptyFilter := query.MessageFilter{SourceID: &accountID}
	empty.apply(&emptyFilter)
	assertions.True(empty.isEmpty())
	assertions.NotNil(emptyFilter.SourceIDs)
	assertions.Empty(emptyFilter.SourceIDs)
}

func TestEmptyCollectionReadsDoNotCallEngine(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	calls := 0
	engine := newMockEngine(MockConfig{})
	engine.ListMessagesFunc = func(_ context.Context, _ query.MessageFilter) ([]query.MessageSummary, error) {
		calls++
		return []query.MessageSummary{}, nil
	}
	engine.GetTotalStatsFunc = func(_ context.Context, _ query.StatsOptions) (*query.TotalStats, error) {
		calls++
		return &query.TotalStats{}, nil
	}
	engine.SearchFastWithStatsFunc = func(_ context.Context, _ *search.Query, _ string, _ query.MessageFilter, _ query.ViewType, _, _ int) (*query.SearchFastResult, error) {
		calls++
		return &query.SearchFastResult{}, nil
	}
	model := New(engine, Options{DataDir: t.TempDir(), Version: "test"})
	model.sourceScope = collectionSourceScope(query.CollectionScope{Name: "Empty", SourceIDs: []int64{}})

	data, ok := model.loadData()().(dataLoadedMsg)
	requirements.True(ok)
	requirements.NoError(data.err)
	assertions.Empty(data.rows)
	stats, ok := model.loadStats()().(statsLoadedMsg)
	requirements.True(ok)
	requirements.NoError(stats.err)
	assertions.Equal(&query.TotalStats{}, stats.stats)
	messages, ok := model.loadMessages()().(messagesLoadedMsg)
	requirements.True(ok)
	requirements.NoError(messages.err)
	assertions.Empty(messages.messages)
	searchResults, ok := model.loadSearch("needle")().(searchResultsMsg)
	requirements.True(ok)
	requirements.NoError(searchResults.err)
	assertions.Empty(searchResults.messages)
	assertions.Equal(0, calls)
}

func TestCollectionScopeStatsRejectStaleResponses(t *testing.T) {
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.stats = &query.TotalStats{MessageCount: 1}
	model.statsRequestID = 10
	model.presentationGeneration = 20
	model.invalidateSourceScope()

	stale := statsLoadedMsg{
		stats:                  &query.TotalStats{MessageCount: 99},
		requestID:              10,
		presentationGeneration: 20,
	}
	updated, _ := model.handleStatsLoaded(stale)
	got := asModel(t, updated)
	assert.Nil(t, got.stats)

	current := statsLoadedMsg{
		stats:                  &query.TotalStats{MessageCount: 2},
		requestID:              got.statsRequestID,
		presentationGeneration: got.presentationGeneration,
	}
	updated, _ = got.handleStatsLoaded(current)
	assert.Equal(t, int64(2), asModel(t, updated).stats.MessageCount)
}

func TestStatsResponseSurvivesAggregateOnlyRefresh(t *testing.T) {
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.statsRequestID = 7
	model.aggregateRequestID = 10
	model.presentationGeneration = 20
	model.aggregateRequestID++

	updated, _ := model.handleStatsLoaded(statsLoadedMsg{
		stats:                  &query.TotalStats{MessageCount: 3},
		requestID:              7,
		presentationGeneration: 20,
	})
	got := asModel(t, updated)
	require.NotNil(t, got.stats)
	assert.Equal(t, int64(3), got.stats.MessageCount)
}

func TestStatsRequestIDAdvancesAtEveryStatsScheduler(t *testing.T) {
	tests := []struct {
		name  string
		model func(t *testing.T) Model
		key   tea.KeyPressMsg
		call  func(Model, tea.KeyPressMsg) (tea.Model, tea.Cmd)
	}{
		{
			name: "mode switch",
			model: func(t *testing.T) Model {
				t.Helper()
				model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
				model.mode = modePeople
				return model
			},
			key: key('m'),
			call: func(model Model, key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
				updated, cmd, _ := model.handleGlobalKeys(key)
				return updated, cmd
			},
		},
		{
			name: "account selector",
			model: func(t *testing.T) Model {
				t.Helper()
				model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
				model.accounts = []query.AccountInfo{{ID: 1, Identifier: "alice@example.invalid"}}
				model.openAccountSelector()
				model.modalCursor = 1
				return model
			},
			key: keyEnter(),
			call: func(model Model, key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
				return model.handleAccountSelectorKeys(key)
			},
		},
		{
			name: "aggregate filter",
			model: func(t *testing.T) Model {
				t.Helper()
				model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
				model.modal = modalFilterToggle
				return model
			},
			key: keyEnter(),
			call: func(model Model, key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
				return model.handleFilterToggleKeys(key)
			},
		},
		{
			name: "message list filter",
			model: func(t *testing.T) Model {
				t.Helper()
				model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
				model.level = levelMessageList
				model.modal = modalFilterToggle
				return model
			},
			key: keyEnter(),
			call: func(model Model, key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
				return model.handleFilterToggleKeys(key)
			},
		},
		{
			name: "message list search filter",
			model: func(t *testing.T) Model {
				t.Helper()
				model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
				model.level = levelMessageList
				model.modal = modalFilterToggle
				model.searchQuery = "needle"
				return model
			},
			key: keyEnter(),
			call: func(model Model, key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
				return model.handleFilterToggleKeys(key)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := tt.model(t)
			before := model.statsRequestID
			updated, cmd := tt.call(model, tt.key)
			got := asModel(t, updated)
			assert.Greater(t, got.statsRequestID, before)
			assert.NotNil(t, cmd)
		})
	}
}

func TestAggregateRefreshDoesNotAdvanceStatsRequestID(t *testing.T) {
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.statsRequestID = 7
	model.aggregateRequestID = 10

	updated, cmd := model.handleAggregateKeys(key('s'))
	got := asModel(t, updated)
	assert.Equal(t, uint64(7), got.statsRequestID)
	assert.Greater(t, got.aggregateRequestID, uint64(10))
	assert.NotNil(t, cmd)
}

func TestFilterToggleSupersedesInFlightStatsResponse(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.level = levelMessageList
	model.modal = modalFilterToggle
	model.statsRequestID = 7
	model.presentationGeneration = 20
	model.stats = &query.TotalStats{MessageCount: 1}

	updated, cmd := model.handleFilterToggleKeys(keyEnter())
	got := asModel(t, updated)
	assertions.NotNil(cmd)
	assertions.Equal(uint64(8), got.statsRequestID)

	updated, _ = got.handleStatsLoaded(statsLoadedMsg{
		stats:                  &query.TotalStats{MessageCount: 99},
		requestID:              7,
		presentationGeneration: got.presentationGeneration,
	})
	got = asModel(t, updated)
	requirements.NotNil(got.stats)
	assertions.Equal(int64(1), got.stats.MessageCount)
}

func TestLoadDataCarriesAggregateRequestID(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.statsRequestID = 7
	model.aggregateRequestID = 11

	loaded, ok := model.loadData()().(dataLoadedMsg)
	requirements.True(ok)
	requirements.NoError(loaded.err)
	assertions.Equal(uint64(11), loaded.requestID)
}

func TestCollectionScopeInvalidationClearsNavigationAndReaders(t *testing.T) {
	assertions := assert.New(t)
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.level = levelMessageDetail
	model.breadcrumbs = []navigationSnapshot{{state: viewState{level: levelAggregates}}}
	model.messageDetail = &query.MessageDetail{ID: 7}
	model.threadConversationID = 12
	model.threadMessages = []query.MessageSummary{{ID: 7}}
	model.parkedMessageReaders[modeEmail].messageDetail = &query.MessageDetail{ID: 8}

	model.invalidateSourceScope()

	assertions.Equal(levelAggregates, model.level)
	assertions.Empty(model.breadcrumbs)
	assertions.Nil(model.messageDetail)
	assertions.Empty(model.threadMessages)
	assertions.Zero(model.threadConversationID)
	assertions.Nil(model.parkedMessageReaders[modeEmail].messageDetail)
	_, cmd := model.goBack()
	assertions.Nil(cmd)
}

func TestTextAccountSelectionKeepsEmailScopeInSync(t *testing.T) {
	accountID := int64(7)
	t.Run("clears inherited email source", func(t *testing.T) {
		assertions := assert.New(t)
		model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
		model.mode = modeTexts
		model.accountFilter = &accountID
		model.modal = modalAccountSelector
		model.drillFilter = query.MessageFilter{Sender: "sender@example.test", SourceID: &accountID}

		model, _ = sendKey(t, model, keyEnter())
		assertions.Nil(model.accountFilter)
		assertions.Equal(sourceScopeAll, model.sourceScope.kind)
		assertions.True(model.sourceScopeExplicit)

		model.mode = modeEmail
		filter := model.buildMessageFilter()
		assertions.Nil(filter.SourceID)
	})

	t.Run("clears explicit email account", func(t *testing.T) {
		model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
		model.mode = modeTexts
		model.accountFilter = &accountID
		model.sourceScope = accountSourceScope(&accountID)
		model.sourceScopeExplicit = true
		model.modal = modalAccountSelector
		model.drillFilter = query.MessageFilter{Sender: "sender@example.test", SourceID: &accountID}

		model, _ = sendKey(t, model, keyEnter())
		assert.Nil(t, model.accountFilter)
		assert.Equal(t, sourceScopeAll, model.sourceScope.kind)

		model.mode = modeEmail
		filter := model.buildMessageFilter()
		assert.Nil(t, filter.SourceID)
	})
}

func TestTextDetailAccountSelectionLeavesNoIndefiniteLoading(t *testing.T) {
	firstID := int64(1)
	secondID := int64(2)
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.accounts = []query.AccountInfo{
		{ID: firstID, Identifier: "first@example.invalid"},
		{ID: secondID, Identifier: "second@example.invalid"},
	}
	model.mode = modeTexts
	model.textState.level = textLevelDetail
	model.textState.selectedMessageID = 0
	model.messageDetail = nil
	model.accountFilter = &firstID
	model.sourceScope = accountSourceScope(&firstID)
	model.sourceScopeExplicit = true
	model.modal = modalAccountSelector
	model.modalCursor = 2

	got, cmd := sendKey(t, model, keyEnter())

	assert.Nil(t, cmd)
	assert.False(t, got.loading)
}

func TestStageForDeletionUsesEmailScopeAuthority(t *testing.T) {
	accountID := int64(7)
	var captured query.MessageFilter
	engine := &querytest.MockEngine{
		GetDeletionTargetsByFilterFunc: func(_ context.Context, filter query.MessageFilter) ([]query.DeletionTarget, error) {
			captured = filter
			return []query.DeletionTarget{{
				MessageID: 1, SourceID: accountID, SourceType: "gmail",
				SourceIdentifier: "account@example.invalid", SourceMessageID: "gm-1",
			}}, nil
		},
	}
	model := New(engine, Options{DataDir: t.TempDir(), Version: "test"})
	model.accounts = []query.AccountInfo{{ID: accountID, SourceType: "gmail", Identifier: "account@example.invalid"}}
	model.sourceScope = accountSourceScope(&accountID)
	model.accountFilter = nil
	model.selection.aggregateKeys["sender@example.invalid"] = true
	model.selection.aggregateViewType = query.ViewSenders

	updated, _ := model.stageForDeletion()
	got := asModel(t, updated)

	assert.Equal(t, &accountID, captured.SourceID)
	assert.Nil(t, captured.SourceIDs)
	assert.Equal(t, modalDeleteConfirm, got.modal)
}

func TestTextAccountSelectionInvalidatesParkedEmailReader(t *testing.T) {
	firstID := int64(1)
	secondID := int64(2)
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.accounts = []query.AccountInfo{
		{ID: firstID, Identifier: "first@example.invalid"},
		{ID: secondID, Identifier: "second@example.invalid"},
	}
	model.mode = modeEmail
	model.messageDetail = &query.MessageDetail{ID: 1}
	model.switchMessageReaderState(modeTexts)
	model.mode = modeTexts
	model.messageDetail = &query.MessageDetail{ID: 2}
	model.accountFilter = &firstID
	model.sourceScope = accountSourceScope(&firstID)
	model.sourceScopeExplicit = true
	model.modal = modalAccountSelector
	model.modalCursor = 2

	model, _ = sendKey(t, model, keyEnter())

	assert.Nil(t, model.parkedMessageReaders[modeEmail].messageDetail)
	assert.Equal(t, int64(2), model.messageDetail.ID)
	assert.Equal(t, secondID, *model.currentSourceScope().accountID)
}

func TestCollectionRowsStayEmailOnly(t *testing.T) {
	assertions := assert.New(t)
	model := New(newMockEngine(MockConfig{}), Options{
		DataDir: "/tmp/test",
		Version: "test",
		CollectionScopeLister: reproductionCollectionScopeLister{
			scopes: []query.CollectionScope{{Name: "Work", SourceIDs: []int64{1}}},
		},
	})
	model.accounts = []query.AccountInfo{{ID: 1, SourceType: meetingSourceImported, Identifier: "alice@example.com"}}
	model.collectionScopes = []query.CollectionScope{{Name: "Work", SourceIDs: []int64{1}}}

	model.mode = modeTexts
	assertions.Len(model.selectorOptions(), 2)
	model.mode = modeMeetings
	assertions.Len(model.selectorOptions(), 2)
	model.mode = modeEmail
	assertions.Len(model.selectorOptions(), 3)
	model.sourceScope = collectionSourceScope(query.CollectionScope{Name: "Work", SourceIDs: []int64{1}})
	model.mode = modeTexts
	assertions.Equal("All Accounts", model.scopeTitle())
}

func TestCollectionScopeSelectionFencesPendingResponses(t *testing.T) {
	assertions := assert.New(t)
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.accounts = []query.AccountInfo{{ID: 1, Identifier: "alice@example.com"}}
	model.collectionScopes = []query.CollectionScope{{Name: "Work", SourceIDs: []int64{1}}}
	model.openAccountSelector()
	model.aggregateRequestID = 10
	model.loadRequestID = 20
	model.detailRequestID = 30
	model.searchRequestID = 40
	model.presentationGeneration = 50
	oldAggregateRequestID := model.aggregateRequestID
	oldPresentationGeneration := model.presentationGeneration

	model.modalCursor = 2
	updated, cmd := sendKey(t, model, keyEnter())
	assertions.NotNil(cmd)
	assertions.Greater(updated.aggregateRequestID, oldAggregateRequestID)
	assertions.Greater(updated.presentationGeneration, oldPresentationGeneration)

	stale := dataLoadedMsg{
		rows:                   []query.AggregateRow{{Key: "old", Count: 1}},
		requestID:              oldAggregateRequestID,
		presentationGeneration: oldPresentationGeneration,
	}
	updated = sendMsg(t, updated, stale)
	assertions.Empty(updated.rows)
}

func TestCollectionDeletionRetainsExactSourcesAndRejectsMultipleSources(t *testing.T) {
	var captured query.MessageFilter
	engine := &querytest.MockEngine{
		GetDeletionTargetsByFilterFunc: func(_ context.Context, filter query.MessageFilter) ([]query.DeletionTarget, error) {
			captured = filter
			return []query.DeletionTarget{
				{MessageID: 1, SourceID: 1, SourceType: "gmail", SourceIdentifier: "one@example.invalid", SourceMessageID: "gm-1"},
				{MessageID: 2, SourceID: 2, SourceType: "gmail", SourceIdentifier: "two@example.invalid", SourceMessageID: "gm-2"},
			}, nil
		},
	}
	controller := NewActionController(engine, t.TempDir(), nil)
	_, err := controller.StageForDeletion(DeletionContext{
		AggregateSelection: map[string]bool{"example.invalid": true},
		AggregateViewType:  query.ViewDomains,
		SourceIDs:          []int64{1, 2},
		Accounts: []query.AccountInfo{
			{ID: 1, Identifier: "one@example.invalid"},
			{ID: 2, Identifier: "two@example.invalid"},
		},
	})
	require.ErrorContains(t, err, "press 'a' to filter by account")
	assert.Equal(t, []int64{1, 2}, captured.SourceIDs)
	assert.Nil(t, captured.SourceID)
}
