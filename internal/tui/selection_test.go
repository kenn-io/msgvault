package tui

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
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
	model, _ = sendKey(t, model, key('D'))

	assertModal(t, model, modalDeleteConfirm)
	assertPendingManifestGmailIDs(t, model, 2)
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
				view := model.View()
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

func TestMessageListDKeyAutoSelectsCurrentMessage(t *testing.T) {
	model := NewBuilder().
		WithMessages(
			query.MessageSummary{ID: 1, SourceMessageID: "msg1", Subject: "Hello"},
			query.MessageSummary{ID: 2, SourceMessageID: "msg2", Subject: "World"},
		).
		WithLevel(levelMessageList).
		WithAccounts(query.AccountInfo{ID: 1, Identifier: "test@gmail.com"}).
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
