package tui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
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
	assertions.NotNil(emptyFilter.SourceIDs)
	assertions.Empty(emptyFilter.SourceIDs)
}

func TestEmptyCollectionReadsMatchNothing(t *testing.T) {
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	tdb.SeedStandardDataSet()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Fail(t, "empty collection issued an HTTP request", "%s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	remote, err := daemonclient.NewEngine(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, remote.Close()) })

	for name, engine := range map[string]query.Engine{
		"sqlite": query.NewSQLiteEngine(tdb.DB),
		"daemon": remote,
	} {
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
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
			results, ok := model.loadSearch("needle")().(searchResultsMsg)
			requirements.True(ok)
			requirements.NoError(results.err)
			assertions.Empty(results.messages)
			thread, ok := model.loadThreadMessages(1)().(threadMessagesLoadedMsg)
			requirements.True(ok)
			requirements.NoError(thread.err)
			assertions.Empty(thread.messages)
		})
	}
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
	updated, _ := model.handleAggregateKeys(key('s'))
	model = asModel(t, updated)

	updated, _ = model.handleStatsLoaded(statsLoadedMsg{
		stats:                  &query.TotalStats{MessageCount: 3},
		requestID:              7,
		presentationGeneration: 20,
	})
	got := asModel(t, updated)
	require.NotNil(t, got.stats)
	assert.Equal(t, int64(3), got.stats.MessageCount)
}

func TestStatsSchedulersReplaceOldTotals(t *testing.T) {
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
			assertions := assert.New(t)
			requirements := require.New(t)
			model := tt.model(t)
			old, ok := model.loadStats()().(statsLoadedMsg)
			requirements.True(ok)
			old.stats = &query.TotalStats{MessageCount: 99}
			updated, cmd := tt.call(model, tt.key)
			got := asModel(t, updated)
			currentStats := got.stats
			got = sendMsg(t, got, old)
			assertions.Equal(currentStats, got.stats)
			var delivered bool
			for _, msg := range runBatchCommand(t, cmd) {
				if stats, ok := msg.(statsLoadedMsg); ok {
					requirements.NoError(stats.err)
					got = sendMsg(t, got, stats)
					assertions.Equal(stats.stats, got.stats)
					delivered = true
				}
			}
			assertions.True(delivered, "scheduled fresh totals")
		})
	}
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

	updated, _ = got.handleStatsLoaded(statsLoadedMsg{
		stats:                  &query.TotalStats{MessageCount: 99},
		requestID:              7,
		presentationGeneration: got.presentationGeneration,
	})
	got = asModel(t, updated)
	requirements.NotNil(got.stats)
	assertions.Equal(int64(1), got.stats.MessageCount)
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

func TestTextAccountSelectionPreservesEmailScope(t *testing.T) {
	accountID := int64(7)
	for name, scope := range map[string]sourceScope{
		"all":        allSourceScope(),
		"account":    accountSourceScope(&accountID),
		"collection": collectionSourceScope(query.CollectionScope{Name: "Work", SourceIDs: []int64{7, 8}}),
	} {
		t.Run(name, func(t *testing.T) {
			model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
			model.sourceScope = scope
			model.level = levelMessageList
			model.drillFilter = query.MessageFilter{Sender: "sender@example.com"}
			before := model.buildMessageFilter()
			model.mode = modeTexts
			model.textState.sourceID = &accountID
			model.openAccountSelector()
			model.modalCursor = 0

			model, _ = sendKey(t, model, keyEnter())
			assert.Nil(t, model.textState.sourceID)
			model.mode = modeEmail
			assert.Equal(t, before, model.buildMessageFilter())
			assert.Equal(t, levelMessageList, model.level)
		})
	}
}

func TestEmailScopeSelectionPreservesTextAccount(t *testing.T) {
	textID := int64(7)
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.accounts = []query.AccountInfo{{ID: 8, Identifier: "email@example.com"}}
	model.collectionScopes = []query.CollectionScope{{Name: "Work", SourceIDs: []int64{8, 9}}}
	model.textState.sourceID = &textID
	for _, choice := range []int{1, 2, 0} {
		model.openAccountSelector()
		model.modalCursor = choice
		model, _ = sendKey(t, model, keyEnter())
		assert.Equal(t, &textID, model.textState.sourceID)
	}
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
	model.textState.sourceID = &firstID
	model.sourceScope = accountSourceScope(&firstID)
	model.modal = modalAccountSelector
	model.modalCursor = 2

	got, cmd := sendKey(t, model, keyEnter())

	assert.Nil(t, cmd)
	assert.False(t, got.loading)
}

func TestTextDetailAccountSelectionReloadsMissingDetail(t *testing.T) {
	assertions := assert.New(t)
	firstID := int64(1)
	secondID := int64(2)
	engine := newMockEngine(MockConfig{})
	engine.GetMessageFunc = func(_ context.Context, id int64) (*query.MessageDetail, error) {
		return &query.MessageDetail{ID: id, ConversationID: 7}, nil
	}
	model := New(engine, Options{DataDir: t.TempDir(), Version: "test"})
	model.accounts = []query.AccountInfo{
		{ID: firstID, Identifier: "first@example.invalid"},
		{ID: secondID, Identifier: "second@example.invalid"},
	}
	model.mode = modeTexts
	model.textState.level = textLevelDetail
	model.textState.selectedConvID = 7
	model.textState.selectedMessageID = 42
	model.messageDetail = nil
	model.textState.sourceID = &firstID
	model.sourceScope = accountSourceScope(&firstID)
	model.modal = modalAccountSelector
	model.modalCursor = 2

	got, cmd := sendKey(t, model, keyEnter())

	assertions.NotNil(cmd)
	assertions.True(got.loading)
	got = deliverTextCommand(t, got, cmd)
	assertions.Equal(int64(42), got.messageDetail.ID)
	assertions.False(got.loading)
}

func TestTextDetailAccountSelectionPreservesLoadedDetail(t *testing.T) {
	assertions := assert.New(t)
	firstID := int64(1)
	secondID := int64(2)
	detail := &query.MessageDetail{ID: 42, ConversationID: 7}
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.accounts = []query.AccountInfo{
		{ID: firstID, Identifier: "first@example.invalid"},
		{ID: secondID, Identifier: "second@example.invalid"},
	}
	model.mode = modeTexts
	model.textState.level = textLevelDetail
	model.textState.selectedConvID = 7
	model.textState.selectedMessageID = detail.ID
	model.messageDetail = detail
	model.textRequestID = 10
	model.textState.sourceID = &firstID
	model.sourceScope = accountSourceScope(&firstID)
	model.modal = modalAccountSelector
	model.modalCursor = 2

	got, cmd := sendKey(t, model, keyEnter())

	assertions.Nil(cmd)
	assertions.False(got.loading)
	assertions.Same(detail, got.messageDetail)
}

func TestStaleTextDetailCompletionRejectedAfterAccountChange(t *testing.T) {
	assertions := assert.New(t)
	firstID := int64(1)
	secondID := int64(2)
	oldDetail := &query.MessageDetail{ID: 42, ConversationID: 7}
	oldErr := errors.New("old detail error")
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.accounts = []query.AccountInfo{
		{ID: firstID, Identifier: "first@example.invalid"},
		{ID: secondID, Identifier: "second@example.invalid"},
	}
	model.mode = modeTexts
	model.textState.level = textLevelDetail
	model.textState.selectedConvID = 7
	model.textState.selectedMessageID = oldDetail.ID
	model.messageDetail = oldDetail
	model.err = oldErr
	model.textRequestID = 10
	model.presentationGeneration = 20
	model.textState.sourceID = &firstID
	model.sourceScope = accountSourceScope(&firstID)
	model.modal = modalAccountSelector
	model.modalCursor = 2

	got, _ := sendKey(t, model, keyEnter())
	updated, _ := got.handleTextMessageLoaded(textMessageLoadedMsg{
		detail:                 &query.MessageDetail{ID: 42, ConversationID: 7},
		requestID:              10,
		conversationID:         7,
		messageID:              42,
		presentationGeneration: 20,
	})
	got = asModel(t, updated)

	assertions.Same(oldDetail, got.messageDetail)
	require.ErrorIs(t, got.err, oldErr)
}

func TestTextAccountSelectionSurvivesBackNavigation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	_, err := tdb.DB.Exec(`
		INSERT INTO sources (id, source_type, identifier) VALUES
			(7, 'imessage', 'first@example.com'), (8, 'imessage', 'second@example.com');
		INSERT INTO participants (id, phone_number, display_name) VALUES
			(11, '+15550000011', 'Alice'), (12, '+15550000012', 'Bob');
		INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type, title) VALUES
			(701, 7, 'chat-701', 'direct_chat', 'First'),
			(702, 8, 'chat-702', 'direct_chat', 'Second');
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id) VALUES
			(801, 701, 7, 'message-801', 'imessage', '2026-01-01 10:00:00', 'first', 11),
			(802, 702, 8, 'message-802', 'imessage', '2026-01-01 11:00:00', 'second', 12);
		INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (701, 11), (702, 12);
	`)
	requirements.NoError(err)
	model := New(query.NewSQLiteEngine(tdb.DB), Options{DataDir: t.TempDir(), Version: "test"})
	model.accounts = []query.AccountInfo{
		{ID: 7, Identifier: "first@example.com"}, {ID: 8, Identifier: "second@example.com"},
	}
	model.mode = modeTexts
	model.textState.sourceID = new(int64(7))
	model = sendMsg(t, model, model.loadTextConversations()())
	requirements.Len(model.textState.conversations, 1)
	assertions.Equal(int64(701), model.textState.conversations[0].ConversationID)

	// Enter a conversation, change account, then return through its breadcrumb.
	model, _ = sendKey(t, model, keyEnter())
	model.openAccountSelector()
	model.modalCursor = 2
	model, _ = sendKey(t, model, keyEnter())
	updated, cmd := model.textGoBack()
	model = asModel(t, updated)
	requirements.NotNil(cmd)
	model = sendMsg(t, model, cmd())

	requirements.NoError(model.err)
	requirements.Len(model.textState.conversations, 1)
	assertions.Equal(int64(702), model.textState.conversations[0].ConversationID)
	requirements.NotNil(model.textState.stats)
	assertions.Equal(int64(1), model.textState.stats.MessageCount)
	assertions.Equal("second@example.com", model.scopeTitle())
}

func TestTextGlobalSearchTimelinePreservedOnAccountSelection(t *testing.T) {
	assertions := assert.New(t)
	firstID := int64(1)
	secondID := int64(2)
	messages := []query.MessageSummary{{ID: 42}}
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.accounts = []query.AccountInfo{
		{ID: firstID, Identifier: "first@example.invalid"},
		{ID: secondID, Identifier: "second@example.invalid"},
	}
	model.mode = modeTexts
	model.textState.level = textLevelTimeline
	model.textState.globalSearchTimeline = true
	model.textState.messages = messages
	model.textState.sourceID = &firstID
	model.sourceScope = accountSourceScope(&firstID)
	model.modal = modalAccountSelector
	model.modalCursor = 2

	got, cmd := sendKey(t, model, keyEnter())

	assertions.Nil(cmd)
	assertions.False(got.loading)
	assertions.True(got.textState.globalSearchTimeline)
	assertions.Equal(messages, got.textState.messages)
}

func TestTextModeSwitchActivationUnchangedByExtraction(t *testing.T) {
	tests := []struct {
		name           string
		level          textViewLevel
		selectedID     int64
		detail         *query.MessageDetail
		globalTimeline bool
		wantCmd        bool
		wantLoading    bool
	}{
		{name: "loaded detail", level: textLevelDetail, selectedID: 42, detail: &query.MessageDetail{ID: 42}, wantCmd: false, wantLoading: false},
		{name: "missing detail", level: textLevelDetail, selectedID: 42, wantCmd: true, wantLoading: true},
		{name: "global timeline", level: textLevelTimeline, globalTimeline: true, wantCmd: false, wantLoading: false},
		{name: "aggregate", level: textLevelAggregate, wantCmd: true, wantLoading: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			engine := newMockEngine(MockConfig{})
			engine.GetMessageFunc = func(_ context.Context, id int64) (*query.MessageDetail, error) {
				return &query.MessageDetail{ID: id}, nil
			}
			model := New(engine, Options{DataDir: t.TempDir(), Version: "test", TextEngine: meetingModeTextEngine{}})
			model.mode = modeEmail
			model.textState.level = tt.level
			model.textState.selectedConvID = 7
			model.textState.selectedMessageID = tt.selectedID
			model.textState.globalSearchTimeline = tt.globalTimeline
			model.parkedMessageReaders[modeTexts].messageDetail = tt.detail

			got, cmd, handled := model.handleGlobalKeys(key('m'))

			assertions.True(handled)
			assertions.Equal(modeTexts, got.mode)
			assertions.Equal(tt.wantCmd, cmd != nil)
			assertions.Equal(tt.wantLoading, got.loading)
		})
	}
}

func TestTextDetailReloadErrorSettlesLoading(t *testing.T) {
	firstID := int64(1)
	secondID := int64(2)
	engine := newMockEngine(MockConfig{})
	engine.GetMessageFunc = func(_ context.Context, _ int64) (*query.MessageDetail, error) {
		return nil, errors.New("detail unavailable")
	}
	model := New(engine, Options{DataDir: t.TempDir(), Version: "test"})
	model.accounts = []query.AccountInfo{
		{ID: firstID, Identifier: "first@example.invalid"},
		{ID: secondID, Identifier: "second@example.invalid"},
	}
	model.mode = modeTexts
	model.textState.level = textLevelDetail
	model.textState.selectedConvID = 7
	model.textState.selectedMessageID = 42
	model.textState.sourceID = &firstID
	model.sourceScope = accountSourceScope(&firstID)
	model.modal = modalAccountSelector
	model.modalCursor = 2

	got, cmd := sendKey(t, model, keyEnter())
	got = deliverTextCommand(t, got, cmd)

	assert.False(t, got.loading)
	assert.Equal(t, modalError, got.modal)
	assert.NotEmpty(t, got.modalResult)
}

func TestTextActivationSpinnerIsIdempotent(t *testing.T) {
	model := New(newMockEngine(MockConfig{}), Options{DataDir: t.TempDir(), Version: "test"})
	model.mode = modeTexts
	model.textState.level = textLevelConversations
	model.spinnerActive = true

	cmd := model.activateTextPresentation()

	assert.NotNil(t, cmd)
	assert.True(t, model.spinnerActive)
}

func deliverTextCommand(t *testing.T, model Model, cmd tea.Cmd) Model {
	t.Helper()
	require.NotNil(t, cmd)
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			childMsg := child()
			if _, ok := childMsg.(textMessageLoadedMsg); ok {
				model = sendMsg(t, model, childMsg)
			}
		}
		return model
	}
	return sendMsg(t, model, msg)
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
	model.selection.aggregateKeys["sender@example.invalid"] = true
	model.selection.aggregateViewType = query.ViewSenders

	updated, _ := model.stageForDeletion()
	got := asModel(t, updated)

	assert.Equal(t, &accountID, captured.SourceID)
	assert.Nil(t, captured.SourceIDs)
	assert.Equal(t, modalDeleteConfirm, got.modal)
}

func TestTextAccountSelectionPreservesParkedEmailReader(t *testing.T) {
	assertions := assert.New(t)
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
	model.textState.sourceID = &firstID
	model.sourceScope = accountSourceScope(&firstID)
	model.modal = modalAccountSelector
	model.modalCursor = 2

	model, _ = sendKey(t, model, keyEnter())

	require.NotNil(t, model.parkedMessageReaders[modeEmail].messageDetail)
	assertions.Equal(int64(1), model.parkedMessageReaders[modeEmail].messageDetail.ID)
	assertions.Equal(int64(2), model.messageDetail.ID)
	assertions.Equal(firstID, *model.sourceScope.accountID)
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
