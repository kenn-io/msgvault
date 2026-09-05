package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestSelectionToggle(t *testing.T) {
	model := NewBuilder().WithRows(
		makeRow("alice@example.com", 10),
		makeRow("bob@example.com", 5),
		makeRow("carol@example.com", 3),
	).Build()

	// Toggle selection with space
	model.cursor = 0
	model, _ = sendKey(t, model, key(' '))

	assertSelected(t, model, "alice@example.com")

	// Toggle off
	model, _ = sendKey(t, model, key(' '))

	assertNotSelected(t, model, "alice@example.com")
}

func TestSelectAllVisible(t *testing.T) {
	model := NewBuilder().
		WithRows(
			makeRow("row1", 10), makeRow("row2", 9), makeRow("row3", 8),
			makeRow("row4", 7), makeRow("row5", 6), makeRow("row6", 5),
		).
		WithPageSize(3).
		Build()

	model = applyAggregateKey(t, model, key('S'))

	assertSelectionCount(t, model, 3)
	assertSelected(t, model, "row1")
	assertSelected(t, model, "row2")
	assertSelected(t, model, "row3")
	assertNotSelected(t, model, "row4")
	assertNotSelected(t, model, "row5")
	assertNotSelected(t, model, "row6")
}

func TestSelectAllVisibleWithScroll(t *testing.T) {
	model := NewBuilder().
		WithRows(
			makeRow("row1", 10), makeRow("row2", 9), makeRow("row3", 8),
			makeRow("row4", 7), makeRow("row5", 6), makeRow("row6", 5),
		).
		WithPageSize(3).
		Build()
	model.scrollOffset = 2 // Scrolled down, showing row3-row5

	model = applyAggregateKey(t, model, key('S'))

	assertSelectionCount(t, model, 3)
	assertNotSelected(t, model, "row1")
	assertNotSelected(t, model, "row2")
	assertSelected(t, model, "row3")
	assertSelected(t, model, "row4")
	assertSelected(t, model, "row5")
	assertNotSelected(t, model, "row6")
}

func TestSelectionClearedOnViewSwitch(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 10)).
		Build()

	model = selectRow(t, model)
	assertSelectionCount(t, model, 1)

	// Switch view with Tab
	model = applyAggregateKey(t, model, keyTab())

	assertSelectionCount(t, model, 0)
	assertSelectionViewTypeMatches(t, model)
}

func TestSelectionClearedOnShiftTab(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 10)).
		Build()

	model = selectRow(t, model)

	// Switch view with Shift+Tab
	model = applyAggregateKey(t, model, keyShiftTab())

	assertSelectionCount(t, model, 0)
}

func TestClearSelection(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 10)).
		Build()

	model = selectRow(t, model)
	assertSelectionCount(t, model, 1)

	// Clear with 'x'
	model = applyAggregateKey(t, model, key('x'))

	assertSelectionCount(t, model, 0)
}

func TestStageForDeletionWithAggregateSelection(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 2)).
		WithGmailIDs("msg1", "msg2").
		Build()

	model = selectRow(t, model)
	var cmd tea.Cmd
	model, cmd = applyAggregateKeyWithCmd(t, model, key('D'))
	model = finishDeletionResolution(t, model, cmd)

	assertModal(t, model, modalDeleteConfirm)
	assertPendingManifestGmailIDs(t, model, 2)
}

func deletionScopeMessages(count int) []query.MessageSummary {
	messages := make([]query.MessageSummary, count)
	for i := range count {
		messages[i] = query.MessageSummary{
			ID: int64(i + 1), SourceID: 1, SourceMessageID: fmt.Sprintf("message-%03d", i+1),
		}
	}
	return messages
}

func deletionScopeTargets(messages []query.MessageSummary) []query.DeletionTarget {
	targets := make([]query.DeletionTarget, len(messages))
	for i, msg := range messages {
		targets[i] = query.DeletionTarget{
			MessageID: msg.ID, SourceID: 1, SourceType: "gmail",
			SourceIdentifier: "user@example.com", SourceMessageID: msg.SourceMessageID,
		}
	}
	return targets
}

type engineWithoutDeletionSearchResolver struct {
	query.Engine
}

func deletionScopeModel(t *testing.T, engine *querytest.MockEngine, messages []query.MessageSummary) Model {
	t.Helper()
	model := NewBuilder().
		WithLevel(levelMessageList).
		WithMessages(messages...).
		WithAccounts(query.AccountInfo{ID: 1, SourceType: "gmail", Identifier: "user@example.com"}).
		Build()
	model.engine = engine
	model.actions = NewActionController(engine, t.TempDir(), nil)
	return model
}

func finishDeletionResolution(t *testing.T, model Model, cmd tea.Cmd) Model {
	t.Helper()
	require.NotNil(t, cmd)
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, batchedCmd := range batch {
			if batchedMsg := batchedCmd(); batchedMsg != nil {
				if _, ok := batchedMsg.(deletionPreparedMsg); ok {
					msg = batchedMsg
					break
				}
			}
		}
	}
	_, ok := msg.(deletionPreparedMsg)
	require.True(t, ok, "expected deletion resolution message")
	updated, _ := model.Update(msg)
	return asModel(t, updated)
}

func TestDeletionKeyScope(t *testing.T) {
	allTargets := []query.DeletionTarget{
		{MessageID: 1, SourceID: 1, SourceType: "gmail", SourceIdentifier: "user@example.com", SourceMessageID: "message-001"},
		{MessageID: 2, SourceID: 1, SourceType: "gmail", SourceIdentifier: "user@example.com", SourceMessageID: "message-002"},
	}
	engine := &querytest.MockEngine{
		ListResults:     deletionScopeMessages(2),
		DeletionTargets: allTargets,
		GetDeletionTargetsByFilterFunc: func(_ context.Context, filter query.MessageFilter) ([]query.DeletionTarget, error) {
			assert.Equal(t, emailMessageType, filter.MessageType)
			return allTargets, nil
		},
	}

	t.Run("lowercase stages current message", func(t *testing.T) {
		model := deletionScopeModel(t, engine, deletionScopeMessages(1))
		model = applyMessageListKey(t, model, key('d'))
		assertPendingManifestGmailIDs(t, model, 1)
	})

	t.Run("uppercase stages every filter match", func(t *testing.T) {
		model := deletionScopeModel(t, engine, deletionScopeMessages(1))
		model, cmd := applyMessageListKeyWithCmd(t, model, key('D'))
		assert.True(t, model.deletionLoading)
		assertModal(t, model, modalNone)
		assert.Contains(t, model.View().Content, "Resolving all deletion matches")
		model = finishDeletionResolution(t, model, cmd)
		assert.False(t, model.deletionLoading)
		assertPendingManifestGmailIDs(t, model, 2)
	})
}

func TestUppercaseDResolvesEveryFilteredMessageInOneBackendCall(t *testing.T) {
	allMessages := deletionScopeMessages(searchPageSize + 1)
	var calls int
	engine := &querytest.MockEngine{
		GetDeletionTargetsByFilterFunc: func(_ context.Context, filter query.MessageFilter) ([]query.DeletionTarget, error) {
			calls++
			assert.Equal(t, query.Pagination{}, filter.Pagination)
			assert.True(t, filter.WithAttachmentsOnly)
			assert.True(t, filter.MatchesEmpty(query.ViewSenders))
			return deletionScopeTargets(allMessages), nil
		},
	}
	model := deletionScopeModel(t, engine, allMessages[:1])
	model.filters.attachmentsOnly = true
	model.drillFilter.SetEmptyTarget(query.ViewSenders)

	model, cmd := applyMessageListKeyWithCmd(t, model, key('D'))
	model = finishDeletionResolution(t, model, cmd)

	assertPendingManifestGmailIDs(t, model, len(allMessages))
	assert.Equal(t, 1, calls)
}

func TestUppercaseDSearchStagesOnlySourceDeletableMessages(t *testing.T) {
	messages := []query.MessageSummary{
		{ID: 1, SourceID: 1, SourceMessageID: "gmail-message"},
		{ID: 2, SourceID: 2, SourceMessageID: "mbox-message"},
	}
	engine := &querytest.MockEngine{
		GetDeletionTargetsBySearchFunc: func(_ context.Context, parsed *search.Query, filter query.MessageFilter, mode query.DeletionSearchMode) ([]query.DeletionTarget, error) {
			assert.Equal(t, []string{"invoice"}, parsed.TextTerms)
			assert.Equal(t, emailMessageType, filter.MessageType)
			assert.Equal(t, query.DeletionSearchFast, mode)
			return []query.DeletionTarget{{
				MessageID: 1, SourceID: 1, SourceType: "gmail",
				SourceIdentifier: "user@example.com", SourceMessageID: "gmail-message",
			}}, nil
		},
	}
	model := deletionScopeModel(t, engine, messages)
	model.accounts = []query.AccountInfo{
		{ID: 1, SourceType: "gmail", Identifier: "user@example.com"},
		{ID: 2, SourceType: "mbox", Identifier: "archive.mbox"},
	}
	model.searchQuery = "invoice"
	model.searchMode = searchModeFast

	model, cmd := applyMessageListKeyWithCmd(t, model, key('D'))
	model = finishDeletionResolution(t, model, cmd)

	assertPendingManifestGmailIDs(t, model, 1)
	assert.Equal(t, []string{"gmail-message"}, model.pendingManifest.GmailIDs)
}

func TestUppercaseDStagesEveryFastSearchMatch(t *testing.T) {
	allMessages := deletionScopeMessages(searchPageSize + 1)
	var calls int
	engine := &querytest.MockEngine{
		GetDeletionTargetsBySearchFunc: func(_ context.Context, parsed *search.Query, filter query.MessageFilter, mode query.DeletionSearchMode) ([]query.DeletionTarget, error) {
			calls++
			assert.Equal(t, []string{"invoice"}, parsed.TextTerms)
			assert.Equal(t, emailMessageType, filter.MessageType)
			assert.Equal(t, query.Pagination{}, filter.Pagination)
			assert.Equal(t, query.DeletionSearchFast, mode)
			return deletionScopeTargets(allMessages), nil
		},
	}
	model := deletionScopeModel(t, engine, allMessages[:1])
	model.searchQuery = "invoice"
	model.searchMode = searchModeFast

	model, cmd := applyMessageListKeyWithCmd(t, model, key('D'))
	assert.Equal(t, 0, calls, "search must not run on the update loop")
	model = finishDeletionResolution(t, model, cmd)

	assertPendingManifestGmailIDs(t, model, len(allMessages))
	assert.Equal(t, 1, calls)
}

func TestUppercaseDStagesEveryDeepSearchMatchWithinFilter(t *testing.T) {
	allMessages := deletionScopeMessages(searchPageSize + 1)
	var calls int
	engine := &querytest.MockEngine{
		GetDeletionTargetsBySearchFunc: func(_ context.Context, parsed *search.Query, filter query.MessageFilter, mode query.DeletionSearchMode) ([]query.DeletionTarget, error) {
			calls++
			assert.Equal(t, []string{"invoice"}, parsed.TextTerms)
			assert.Equal(t, "alice@example.com", filter.Sender)
			assert.True(t, filter.MatchesEmpty(query.ViewSenderNames))
			assert.Equal(t, query.DeletionSearchDeep, mode)
			return deletionScopeTargets(allMessages), nil
		},
	}
	model := deletionScopeModel(t, engine, allMessages[:1])
	model.searchQuery = "invoice"
	model.searchMode = searchModeDeep
	model.drillFilter.Sender = "alice@example.com"
	model.drillFilter.SetEmptyTarget(query.ViewSenderNames)

	model, cmd := applyMessageListKeyWithCmd(t, model, key('D'))
	assert.Equal(t, 0, calls, "search must not run on the update loop")
	model = finishDeletionResolution(t, model, cmd)

	assertPendingManifestGmailIDs(t, model, len(allMessages))
	assert.Equal(t, 1, calls)
}

func TestUppercaseDSearchRequiresStableResolver(t *testing.T) {
	messages := deletionScopeMessages(1)
	baseEngine := &querytest.MockEngine{}
	engine := engineWithoutDeletionSearchResolver{Engine: baseEngine}
	model := deletionScopeModel(t, baseEngine, messages)
	model.engine = engine
	model.actions = NewActionController(engine, t.TempDir(), nil)
	model.searchQuery = "invoice"
	model.searchMode = searchModeFast

	model, cmd := applyMessageListKeyWithCmd(t, model, key('D'))
	model = finishDeletionResolution(t, model, cmd)

	assertModal(t, model, modalDeleteResult)
	assert.Contains(t, model.modalResult, "cannot resolve every search match safely")
}

func TestUppercaseDRejectsSemanticSearch(t *testing.T) {
	engine := &querytest.MockEngine{}
	model := deletionScopeModel(t, engine, deletionScopeMessages(1))
	model.searchQuery = "find the invoice"
	model.searchMode = searchModeSemantic

	model, cmd := applyMessageListKeyWithCmd(t, model, key('D'))
	model = finishDeletionResolution(t, model, cmd)

	assertModal(t, model, modalDeleteResult)
	assert.Contains(t, model.modalResult, "semantic")
}

func TestUppercaseDRejectsMalformedSearchQuery(t *testing.T) {
	engine := &querytest.MockEngine{
		SearchFastFunc: func(_ context.Context, _ *search.Query, _ query.MessageFilter, _, _ int) ([]query.MessageSummary, error) {
			require.FailNow(t, "malformed bulk-deletion query reached the search engine")
			return nil, nil
		},
	}
	model := deletionScopeModel(t, engine, deletionScopeMessages(1))
	model.searchQuery = "before:not-a-date"
	model.searchMode = searchModeFast

	model, cmd := applyMessageListKeyWithCmd(t, model, key('D'))
	model = finishDeletionResolution(t, model, cmd)

	assertModal(t, model, modalDeleteResult)
	assert.Contains(t, model.modalResult, "invalid value")
}

func TestUppercaseDReportsNoMatchingDeletableMessages(t *testing.T) {
	model := deletionScopeModel(t, &querytest.MockEngine{
		GetDeletionTargetsByFilterFunc: func(context.Context, query.MessageFilter) ([]query.DeletionTarget, error) {
			return nil, nil
		},
	}, deletionScopeMessages(1))

	model, cmd := applyMessageListKeyWithCmd(t, model, key('D'))
	model = finishDeletionResolution(t, model, cmd)

	assertModal(t, model, modalDeleteResult)
	assert.Contains(t, model.modalResult, "no deletable messages match the current filter or search")
}

func TestAllMatchesManifestRecordsMatchScopeInsteadOfStaleSelection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	controller := NewActionController(&querytest.MockEngine{
		GetDeletionTargetsByFilterFunc: func(_ context.Context, filter query.MessageFilter) ([]query.DeletionTarget, error) {
			assertions.Equal("current@example.com", filter.Sender)
			return deletionScopeTargets(deletionScopeMessages(2)), nil
		},
	}, t.TempDir(), nil)

	manifest, err := controller.StageForDeletion(DeletionContext{
		AllMatches:         true,
		AggregateSelection: map[string]bool{"stale@example.com": true},
		MessageSelection:   map[int64]bool{99: true},
		AggregateViewType:  query.ViewSenders,
		Accounts:           []query.AccountInfo{{ID: 1, SourceType: "gmail", Identifier: "user@example.com"}},
		MatchFilter:        query.MessageFilter{Sender: "current@example.com"},
	})
	requirements.NoError(err)
	assertions.Equal("all-matches", manifest.Description)
	assertions.Equal([]string{"current@example.com"}, manifest.Filters.Senders)
	assertions.NotContains(manifest.Filters.Senders, "stale@example.com")

	var provenance struct {
		Scope       string `json:"scope"`
		MatchFilter struct {
			Sender string `json:"sender"`
		} `json:"match_filter"`
	}
	requirements.NoError(json.Unmarshal(manifest.RawFilter, &provenance))
	assertions.Equal("all_matches", provenance.Scope)
	assertions.Equal("current@example.com", provenance.MatchFilter.Sender)
}

func TestAllMatchesCanonicalEmptySearchUsesFilterScope(t *testing.T) {
	tests := []struct {
		name        string
		searchQuery string
	}{
		{name: "empty subject", searchQuery: "subject:"},
		{name: "whitespace", searchQuery: "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var filterCalls, searchCalls int
			engine := &querytest.MockEngine{
				GetDeletionTargetsByFilterFunc: func(context.Context, query.MessageFilter) ([]query.DeletionTarget, error) {
					filterCalls++
					return deletionScopeTargets(deletionScopeMessages(1)), nil
				},
				GetDeletionTargetsBySearchFunc: func(context.Context, *search.Query, query.MessageFilter, query.DeletionSearchMode) ([]query.DeletionTarget, error) {
					searchCalls++
					return deletionScopeTargets(deletionScopeMessages(1)), nil
				},
			}
			controller := NewActionController(engine, t.TempDir(), nil)

			manifest, err := controller.StageForDeletion(DeletionContext{
				AllMatches: true, SearchQuery: tt.searchQuery, SearchMode: searchModeFast,
			})

			requirements.NoError(err)
			assertions.Equal(1, filterCalls)
			assertions.Zero(searchCalls)
			assertions.Equal("all-matches", manifest.Description)
			var provenance allMatchesManifestProvenance
			requirements.NoError(json.Unmarshal(manifest.RawFilter, &provenance))
			assertions.Empty(provenance.SearchQuery)
			assertions.Empty(provenance.SearchMode)
		})
	}
}

func TestBulkDeletionResolutionBlocksMessageListActions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	model := deletionScopeModel(t, &querytest.MockEngine{}, deletionScopeMessages(1))
	model, resolveCmd := sendKey(t, model, key('D'))
	require.NotNil(resolveCmd)
	require.True(model.deletionLoading)

	for _, blockedKey := range []tea.KeyPressMsg{key('d'), key('f'), key('?')} {
		updated, cmd := sendKey(t, model, blockedKey)
		assert.True(updated.deletionLoading)
		assertModal(t, updated, modalNone)
		assert.Nil(updated.pendingManifest)
		assert.Nil(cmd)
	}

	canceled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	model.deletionCancel = func() {
		cancel()
		close(canceled)
	}
	updated, cmd := sendKey(t, model, keyEsc())
	assert.False(updated.deletionLoading)
	assert.NotNil(cmd)
	select {
	case <-canceled:
	default:
		require.FailNow("Esc did not cancel deletion resolution")
	}
	require.ErrorIs(ctx.Err(), context.Canceled)

	model.deletionLoading = true
	model.deletionCancel = cancel
	updated, cmd = sendKey(t, model, key('q'))
	assertModal(t, updated, modalQuitConfirm)
	assert.False(updated.deletionLoading)
	assert.Nil(cmd)
}

func TestBulkDeletionResolutionBlocksAndCancelsAggregateActions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	model := deletionScopeModel(t, &querytest.MockEngine{}, deletionScopeMessages(1))
	model.level = levelAggregates
	model.viewType = query.ViewSenders
	model.rows = []query.AggregateRow{
		makeRow("alice@example.com", 1),
		makeRow("bob@example.com", 1),
	}

	model, resolveCmd := sendKey(t, model, key('D'))
	require.NotNil(resolveCmd)
	require.True(model.deletionLoading)

	updated, cmd := sendKey(t, model, key('j'))
	assert.Zero(updated.cursor)
	assert.True(updated.deletionLoading)
	assert.Nil(cmd)

	canceled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	model.deletionCancel = func() {
		cancel()
		close(canceled)
	}
	updated, cmd = sendKey(t, model, keyEsc())
	assert.False(updated.deletionLoading)
	assert.NotNil(cmd)
	select {
	case <-canceled:
	default:
		require.FailNow("Esc did not cancel aggregate deletion resolution")
	}
	require.ErrorIs(ctx.Err(), context.Canceled)
}

func TestStageForDeletion(t *testing.T) {
	accountID1 := int64(1)
	nonExistentID := int64(999)

	tests := []struct {
		name             string
		accountFilter    *int64
		accounts         []query.AccountInfo
		expectedAccount  string
		checkViewWarning bool // whether to check for "Account not set" warning
	}{
		{
			name:            "with account filter",
			accountFilter:   &accountID1,
			accounts:        testAccounts,
			expectedAccount: "user1@gmail.com",
		},
		{
			name:            "single account auto-selects",
			accounts:        []query.AccountInfo{{ID: 1, Identifier: "only@gmail.com"}},
			expectedAccount: "only@gmail.com",
		},
		{
			name:            "multiple accounts no filter",
			accounts:        testAccounts,
			expectedAccount: "",
		},
		{
			name:             "account filter not found",
			accountFilter:    &nonExistentID,
			accounts:         testAccounts,
			expectedAccount:  "",
			checkViewWarning: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBuilder().
				WithRows(makeRow("alice@example.com", 2)).
				WithGmailIDs("msg1", "msg2")

			if len(tc.accounts) > 0 {
				b = b.WithAccounts(tc.accounts...)
			}
			if tc.accountFilter != nil {
				b = b.WithAccountFilter(tc.accountFilter)
			}

			model := b.Build()
			model = selectRow(t, model)

			newModel, _ := model.stageForDeletion()
			model = asModel(t, newModel)

			assertPendingManifest(t, model, tc.expectedAccount)
			assertModal(t, model, modalDeleteConfirm)

			if tc.checkViewWarning {
				view := model.View().Content
				assert.Contains(t, view, "Account not set", "expected warning in delete confirm modal")
			}
		})
	}
}

func TestAKeyShowsAllMessages(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 2)).
		Build()

	var cmd tea.Cmd
	model, cmd = sendKey(t, model, key('a'))

	assertLevel(t, model, levelMessageList)
	assertFilterKey(t, model, "")
	assertCmd(t, cmd, true)
	assertBreadcrumbCount(t, model, 1)
}

func TestStageForDeletionPreservesConversationOnlyDrillScope(t *testing.T) {
	var captured query.MessageFilter
	engine := newMockEngine(MockConfig{})
	engine.GetDeletionTargetsByFilterFunc = func(_ context.Context, filter query.MessageFilter) ([]query.DeletionTarget, error) {
		captured = filter
		return []query.DeletionTarget{{
			MessageID: 1, SourceID: 1, SourceType: "gmail",
			SourceIdentifier: "test@example.com", SourceMessageID: "message-1",
		}}, nil
	}
	conversationID := int64(42)
	model := New(engine, Options{DataDir: t.TempDir(), Version: "test"})
	model.drillFilter = query.MessageFilter{ConversationID: &conversationID}
	model.selection.aggregateKeys["2026"] = true
	model.selection.aggregateViewType = query.ViewTime
	model.timeGranularity = query.TimeYear

	newModel, _ := model.stageForDeletion()
	model = asModel(t, newModel)

	require.NotNil(t, captured.ConversationID)
	assert.Equal(t, conversationID, *captured.ConversationID)
	assertModal(t, model, modalDeleteConfirm)
}

func TestModalDismiss(t *testing.T) {
	model := NewBuilder().
		WithModal(modalDeleteResult).
		Build()
	model.modalResult = "Test result"

	model, _ = applyModalKey(t, model, key('x'))

	assertModalCleared(t, model)
}

func TestConfirmModalCancel(t *testing.T) {
	model := NewBuilder().
		WithModal(modalDeleteConfirm).
		Build()
	model.pendingManifest = &deletion.Manifest{}

	model, _ = applyModalKey(t, model, key('n'))

	assertModal(t, model, modalNone)
	assertPendingManifestCleared(t, model)
}

func TestSelectionCount(t *testing.T) {
	model := Model{
		selection: selectionState{
			aggregateKeys: map[string]bool{"a": true, "b": true},
			messageIDs:    map[int64]bool{1: true, 2: true, 3: true},
		},
	}

	assert.Equal(t, 5, model.selectionCount())
}

func TestHasSelection(t *testing.T) {
	model := Model{
		selection: selectionState{
			aggregateKeys: make(map[string]bool),
			messageIDs:    make(map[int64]bool),
		},
	}

	assert.False(t, model.hasSelection(), "expected false for empty selection")

	model.selection.aggregateKeys["test"] = true
	assert.True(t, model.hasSelection(), "with aggregate selection")

	model.selection.aggregateKeys = make(map[string]bool)
	model.selection.messageIDs[1] = true
	assert.True(t, model.hasSelection(), "with message selection")
}

func TestDKeyAutoSelectsCurrentRow(t *testing.T) {
	model := NewBuilder().
		WithRows(
			makeRow("alice@example.com", 10),
			makeRow("bob@example.com", 5),
		).
		WithGmailIDs("msg1", "msg2").
		WithViewType(query.ViewSenders).
		WithAccounts(query.AccountInfo{ID: 1, Identifier: "test@gmail.com"}).
		Build()
	model.cursor = 1

	assertHasSelection(t, model, false)

	m := applyAggregateKey(t, model, key('d'))

	assertSelected(t, m, "bob@example.com")
	assertModal(t, m, modalDeleteConfirm)
}

func TestDKeyWithExistingSelection(t *testing.T) {
	model := NewBuilder().
		WithRows(
			makeRow("alice@example.com", 10),
			makeRow("bob@example.com", 5),
		).
		WithGmailIDs("msg1", "msg2").
		WithViewType(query.ViewSenders).
		WithAccounts(query.AccountInfo{ID: 1, Identifier: "test@gmail.com"}).
		WithSelectedAggregates("alice@example.com").
		Build()
	model.cursor = 1

	m := applyAggregateKey(t, model, key('d'))

	assertSelected(t, m, "alice@example.com")
	assertNotSelected(t, m, "bob@example.com")
	assertModal(t, m, modalDeleteConfirm)
}

func TestUppercaseDStagesCurrentAggregateRowDespiteExistingSelection(t *testing.T) {
	var gotSender string
	engine := &querytest.MockEngine{
		GetDeletionTargetsByFilterFunc: func(_ context.Context, filter query.MessageFilter) ([]query.DeletionTarget, error) {
			gotSender = filter.Sender
			return deletionScopeTargets(deletionScopeMessages(1)), nil
		},
	}
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 10), makeRow("bob@example.com", 5)).
		WithViewType(query.ViewSenders).
		WithAccounts(query.AccountInfo{ID: 1, SourceType: "gmail", Identifier: "test@gmail.com"}).
		WithSelectedAggregates("alice@example.com").
		Build()
	model.engine = engine
	model.actions = NewActionController(engine, t.TempDir(), nil)
	model.cursor = 1

	updated, cmd := applyAggregateKeyWithCmd(t, model, key('D'))
	updated = finishDeletionResolution(t, updated, cmd)

	assert.Equal(t, "bob@example.com", gotSender)
	assert.Equal(t, "Senders-bob@example.com", updated.pendingManifest.Description)
	assert.Equal(t, []string{"bob@example.com"}, updated.pendingManifest.Filters.Senders)
}

func TestUppercaseDWaitsForAggregateReloadAfterViewChange(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 1)).
		WithViewType(query.ViewSenders).
		Build()

	model, loadCmd := applyAggregateKeyWithCmd(t, model, key('g'))
	requirements.NotNil(loadCmd)
	requirements.True(model.loading)

	model, deletionCmd := applyAggregateKeyWithCmd(t, model, key('D'))
	assertions.Nil(deletionCmd)
	assertions.False(model.deletionLoading)
	assertions.Nil(model.pendingManifest)
	assertModal(t, model, modalNone)
}

func TestUppercaseDAggregateStagesOnlyDisplayedSearchAndAttachmentMatches(t *testing.T) {
	tdb := dbtest.NewTestDB(t, "../store/schema.sql")
	tdb.SeedStandardDataSet()
	engine := query.NewSQLiteEngine(tdb.DB)
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 1)).
		WithViewType(query.ViewSenders).
		WithAccounts(query.AccountInfo{ID: 1, SourceType: "gmail", Identifier: "test@gmail.com"}).
		Build()
	model.engine = engine
	model.actions = NewActionController(engine, t.TempDir(), nil)
	model.searchQuery = "Hello"
	model.searchMode = searchModeFast
	model.filters.attachmentsOnly = true

	model, cmd := applyAggregateKeyWithCmd(t, model, key('D'))
	if cmd != nil {
		model = finishDeletionResolution(t, model, cmd)
	}

	require.NotNil(t, model.pendingManifest)
	assert.Equal(t, []string{"msg2"}, model.pendingManifest.GmailIDs)
}

func TestMessageListDKeyAutoSelectsCurrentMessage(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, SourceID: 1, SourceMessageID: "msg1", Subject: "Hello"},
			query.MessageSummary{ID: 2, SourceID: 1, SourceMessageID: "msg2", Subject: "World"},
		).
		WithLevel(levelMessageList).
		WithAccounts(query.AccountInfo{ID: 1, SourceType: "gmail", Identifier: "test@gmail.com"}).
		Build()

	assertHasSelection(t, model, false)

	m := applyMessageListKey(t, model, key('d'))

	assertMessageSelected(t, m, 1)
	assertModal(t, m, modalDeleteConfirm)
}

func TestToggleAggregateSelection(t *testing.T) {
	m := NewBuilder().WithRows(
		makeRow("alice@example.com", 0),
		makeRow("bob@example.com", 0),
	).Build()
	m.cursor = 0

	m.toggleAggregateSelection()
	assert.True(t, m.selection.aggregateKeys["alice@example.com"], "expected alice to be selected")

	m.toggleAggregateSelection()
	assert.False(t, m.selection.aggregateKeys["alice@example.com"], "expected alice to be deselected")
}

func TestSelectVisibleAggregates(t *testing.T) {
	rows := make([]query.AggregateRow, 0, 10)
	for i := range 10 {
		rows = append(rows, query.AggregateRow{Key: fmt.Sprintf("user%d", i)})
	}
	m := NewBuilder().WithRows(rows...).Build()
	m.pageSize = 3
	m.scrollOffset = 2

	m.selectVisibleAggregates()

	for i := 2; i < 5; i++ {
		k := fmt.Sprintf("user%d", i)
		assert.True(t, m.selection.aggregateKeys[k], "expected %s to be selected", k)
	}
	assert.False(t, m.selection.aggregateKeys["user0"], "user0 should not be selected")
}

func TestSelectVisibleAggregates_OffsetBeyondRows(t *testing.T) {
	m := NewBuilder().WithRows(makeRow("a", 0)).Build()
	m.scrollOffset = 100
	m.pageSize = 5

	m.selectVisibleAggregates()

	assert.Empty(t, m.selection.aggregateKeys, "expected no selections when scrollOffset > len(rows)")
}

func TestClearAllSelections(t *testing.T) {
	m := NewBuilder().WithRows(makeRow("a", 0)).Build()
	m.selection.aggregateKeys["a"] = true
	m.selection.messageIDs[1] = true

	m.clearAllSelections()

	assert.Empty(t, m.selection.aggregateKeys, "aggregateKeys should be cleared")
	assert.Empty(t, m.selection.messageIDs, "messageIDs should be cleared")
}
