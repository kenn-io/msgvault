package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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

	model = selectRow(t, model, 0)
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

	model = selectRow(t, model, 0)

	// Switch view with Shift+Tab
	model = applyAggregateKey(t, model, keyShiftTab())

	assertSelectionCount(t, model, 0)
}

func TestClearSelection(t *testing.T) {
	model := NewBuilder().
		WithRows(makeRow("alice@example.com", 10)).
		Build()

	model = selectRow(t, model, 0)
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

	model = selectRow(t, model, 0)
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
			model = selectRow(t, model, 0)

			newModel, _ := model.stageForDeletion()
			model = newModel.(Model)

			assertPendingManifest(t, model, tc.expectedAccount)
			assertModal(t, model, modalDeleteConfirm)

			if tc.checkViewWarning {
				view := model.View()
				if !strings.Contains(view, "Account not set") {
					t.Errorf("expected 'Account not set' warning in delete confirm modal, view:\n%s", view)
				}
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

	if model.selectionCount() != 5 {
		t.Errorf("expected SelectionCount() = 5, got %d", model.selectionCount())
	}
}

func TestHasSelection(t *testing.T) {
	model := Model{
		selection: selectionState{
			aggregateKeys: make(map[string]bool),
			messageIDs:    make(map[int64]bool),
		},
	}

	if model.hasSelection() {
		t.Error("expected HasSelection() = false for empty selection")
	}

	model.selection.aggregateKeys["test"] = true
	if !model.hasSelection() {
		t.Error("expected HasSelection() = true with aggregate selection")
	}

	model.selection.aggregateKeys = make(map[string]bool)
	model.selection.messageIDs[1] = true
	if !model.hasSelection() {
		t.Error("expected HasSelection() = true with message selection")
	}
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
	if !m.selection.aggregateKeys["alice@example.com"] {
		t.Error("expected alice to be selected")
	}

	m.toggleAggregateSelection()
	if m.selection.aggregateKeys["alice@example.com"] {
		t.Error("expected alice to be deselected")
	}
}

func TestSelectVisibleAggregates(t *testing.T) {
	rows := make([]query.AggregateRow, 0, 10)
	for i := 0; i < 10; i++ {
		rows = append(rows, query.AggregateRow{Key: fmt.Sprintf("user%d", i)})
	}
	m := NewBuilder().WithRows(rows...).Build()
	m.pageSize = 3
	m.scrollOffset = 2

	m.selectVisibleAggregates()

	for i := 2; i < 5; i++ {
		key := fmt.Sprintf("user%d", i)
		if !m.selection.aggregateKeys[key] {
			t.Errorf("expected %s to be selected", key)
		}
	}
	if m.selection.aggregateKeys["user0"] {
		t.Error("user0 should not be selected")
	}
}

func TestSelectVisibleAggregates_OffsetBeyondRows(t *testing.T) {
	m := NewBuilder().WithRows(makeRow("a", 0)).Build()
	m.scrollOffset = 100
	m.pageSize = 5

	m.selectVisibleAggregates()

	if len(m.selection.aggregateKeys) != 0 {
		t.Error("expected no selections when scrollOffset > len(rows)")
	}
}

func TestClearAllSelections(t *testing.T) {
	m := NewBuilder().WithRows(makeRow("a", 0)).Build()
	m.selection.aggregateKeys["a"] = true
	m.selection.messageIDs[1] = true

	m.clearAllSelections()

	if len(m.selection.aggregateKeys) != 0 || len(m.selection.messageIDs) != 0 {
		t.Error("expected all selections to be cleared")
	}
}
