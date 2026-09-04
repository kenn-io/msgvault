package tui

import (
	"context"
	"testing"

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
	require.True(t, ok)
	require.NoError(t, scopesMsg.err)
	model = sendMsg(t, model, scopesMsg)
	model.openAccountSelector()

	modal := stripANSI(model.renderAccountSelectorModal())
	assert.Contains(t, modal, "Work")

	model, _ = sendKey(t, model, keyDown())
	model, _ = sendKey(t, model, keyDown())
	model, _ = sendKey(t, model, keyDown())
	model, cmd := sendKey(t, model, keyEnter())
	assert.NotNil(t, cmd)
	assert.Equal(t, "Collection: Work", model.scopeTitle())

	loaded, ok := model.loadMessages()().(messagesLoadedMsg)
	require.True(t, ok)
	require.NoError(t, loaded.err)
	assert.Equal(t, []int64{1, 2}, captured.SourceIDs)
	assert.Nil(t, captured.SourceID)
}

func TestCollectionScopeSourceFieldsPreserveKindsAndCopies(t *testing.T) {
	accountID := int64(7)
	collectionIDs := []int64{7, 8}
	collection := query.CollectionScope{Name: "Work", SourceIDs: collectionIDs}

	accountFilter := query.MessageFilter{SourceID: &accountID, SourceIDs: []int64{99}}
	accountSourceScope(&accountID).apply(&accountFilter)
	assert.Equal(t, &accountID, accountFilter.SourceID)
	assert.Nil(t, accountFilter.SourceIDs)

	collectionFilter := query.MessageFilter{SourceID: &accountID}
	collectionSourceScope(collection).apply(&collectionFilter)
	assert.Nil(t, collectionFilter.SourceID)
	assert.Equal(t, []int64{7, 8}, collectionFilter.SourceIDs)
	collectionIDs[0] = 99
	assert.Equal(t, []int64{7, 8}, collectionFilter.SourceIDs)

	empty := collectionSourceScope(query.CollectionScope{Name: "Empty", SourceIDs: []int64{}})
	emptyFilter := query.MessageFilter{SourceID: &accountID}
	empty.apply(&emptyFilter)
	assert.True(t, empty.isEmpty())
	assert.NotNil(t, emptyFilter.SourceIDs)
	assert.Empty(t, emptyFilter.SourceIDs)
}

func TestEmptyCollectionReadsDoNotCallEngine(t *testing.T) {
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
	require.True(t, ok)
	require.NoError(t, data.err)
	assert.Empty(t, data.rows)
	stats, ok := model.loadStats()().(statsLoadedMsg)
	require.True(t, ok)
	require.NoError(t, stats.err)
	assert.Equal(t, &query.TotalStats{}, stats.stats)
	messages, ok := model.loadMessages()().(messagesLoadedMsg)
	require.True(t, ok)
	require.NoError(t, messages.err)
	assert.Empty(t, messages.messages)
	searchResults, ok := model.loadSearch("needle")().(searchResultsMsg)
	require.True(t, ok)
	require.NoError(t, searchResults.err)
	assert.Empty(t, searchResults.messages)
	assert.Equal(t, 0, calls)
}

func TestCollectionScopeStatsRejectStaleResponses(t *testing.T) {
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.stats = &query.TotalStats{MessageCount: 1}
	model.aggregateRequestID = 10
	model.presentationGeneration = 20
	model.invalidateSourceScope()

	stale := statsLoadedMsg{
		stats:                  &query.TotalStats{MessageCount: 99},
		requestID:              10,
		presentationGeneration: 20,
	}
	updated, _ := model.handleStatsLoaded(stale)
	got := updated.(Model)
	assert.Nil(t, got.stats)

	current := statsLoadedMsg{
		stats:                  &query.TotalStats{MessageCount: 2},
		requestID:              got.aggregateRequestID,
		presentationGeneration: got.presentationGeneration,
	}
	updated, _ = got.handleStatsLoaded(current)
	assert.Equal(t, int64(2), updated.(Model).stats.MessageCount)
}

func TestCollectionScopeInvalidationClearsNavigationAndReaders(t *testing.T) {
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.level = levelMessageDetail
	model.breadcrumbs = []navigationSnapshot{{state: viewState{level: levelAggregates}}}
	model.messageDetail = &query.MessageDetail{ID: 7}
	model.threadConversationID = 12
	model.threadMessages = []query.MessageSummary{{ID: 7}}
	model.parkedMessageReaders[modeEmail].messageDetail = &query.MessageDetail{ID: 8}

	model.invalidateSourceScope()

	assert.Equal(t, levelAggregates, model.level)
	assert.Empty(t, model.breadcrumbs)
	assert.Nil(t, model.messageDetail)
	assert.Empty(t, model.threadMessages)
	assert.Zero(t, model.threadConversationID)
	assert.Nil(t, model.parkedMessageReaders[modeEmail].messageDetail)
	_, cmd := model.goBack()
	assert.Nil(t, cmd)
}

func TestAllAccountsTextSelectionClearsInheritedEmailSource(t *testing.T) {
	accountID := int64(7)
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.mode = modeTexts
	model.accountFilter = &accountID
	model.modal = modalAccountSelector
	model.drillFilter = query.MessageFilter{Sender: "sender@example.test", SourceID: &accountID}

	model, _ = sendKey(t, model, keyEnter())
	assert.Nil(t, model.accountFilter)
	assert.True(t, model.sourceScopeExplicit)

	model.mode = modeEmail
	filter := model.buildMessageFilter()
	assert.Nil(t, filter.SourceID)
}

func TestCollectionRowsStayEmailOnly(t *testing.T) {
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
	assert.Len(t, model.selectorOptions(), 2)
	model.mode = modeMeetings
	assert.Len(t, model.selectorOptions(), 2)
	model.mode = modeEmail
	assert.Len(t, model.selectorOptions(), 3)
	model.sourceScope = collectionSourceScope(query.CollectionScope{Name: "Work", SourceIDs: []int64{1}})
	model.mode = modeTexts
	assert.Equal(t, "All Accounts", model.scopeTitle())
}

func TestCollectionScopeSelectionFencesPendingResponses(t *testing.T) {
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
	assert.NotNil(t, cmd)
	assert.Greater(t, updated.aggregateRequestID, oldAggregateRequestID)
	assert.Greater(t, updated.presentationGeneration, oldPresentationGeneration)

	stale := dataLoadedMsg{
		rows:                   []query.AggregateRow{{Key: "old", Count: 1}},
		requestID:              oldAggregateRequestID,
		presentationGeneration: oldPresentationGeneration,
	}
	updated = sendMsg(t, updated, stale)
	assert.Empty(t, updated.rows)
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
