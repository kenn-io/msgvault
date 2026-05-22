package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"go.kenn.io/msgvault/internal/query"
)

// =============================================================================
// Quit Confirmation Modal Tests
// =============================================================================

func TestQuitConfirmationModal(t *testing.T) {
	model := NewBuilder().Build()

	// Press 'q' should open quit confirmation, not quit immediately
	var cmd tea.Cmd
	model, cmd = sendKey(t, model, key('q'))

	assertModal(t, model, modalQuitConfirm)

	if model.quitting {
		t.Error("should not be quitting yet")
	}
	if cmd != nil {
		t.Error("should not have quit command yet")
	}

	// Press 'n' to cancel
	model, _ = sendKey(t, model, key('n'))

	assertModal(t, model, modalNone)
}

func TestQuitConfirmationConfirm(t *testing.T) {
	model := NewBuilder().WithModal(modalQuitConfirm).WithPageSize(10).WithSize(100, 20).Build()

	// Press 'y' to confirm quit
	m, cmd := applyModalKey(t, model, key('y'))

	if !m.quitting {
		t.Error("expected quitting = true")
	}
	if cmd == nil {
		t.Error("expected quit command")
	}
}

// =============================================================================
// Account Selector Modal Tests
// =============================================================================

func TestAccountSelectorModal(t *testing.T) {
	model := NewBuilder().
		WithAccounts(
			query.AccountInfo{ID: 1, Identifier: "alice@example.com"},
			query.AccountInfo{ID: 2, Identifier: "bob@example.com"},
		).
		WithPageSize(10).WithSize(100, 20).
		Build()

	// Press 'A' to open account selector
	m := applyAggregateKey(t, model, key('A'))

	if m.modal != modalAccountSelector {
		t.Errorf("expected modalAccountSelector, got %v", m.modal)
	}
	if m.modalCursor != 0 {
		t.Errorf("expected modalCursor = 0 (All Accounts), got %d", m.modalCursor)
	}

	// Navigate down
	m, _ = applyModalKey(t, m, key('j'))
	if m.modalCursor != 1 {
		t.Errorf("expected modalCursor = 1, got %d", m.modalCursor)
	}

	// Select account
	var cmd tea.Cmd
	m, cmd = applyModalKey(t, m, keyEnter())

	if m.modal != modalNone {
		t.Errorf("expected modalNone after selection, got %v", m.modal)
	}
	if m.accountFilter == nil || *m.accountFilter != 1 {
		t.Errorf("expected accountFilter = 1, got %v", m.accountFilter)
	}
	if cmd == nil {
		t.Error("expected command to reload data")
	}
}

func TestOpenAccountSelector(t *testing.T) {
	t.Run("no accounts", func(t *testing.T) {
		m := NewBuilder().Build()
		m.openAccountSelector()
		assertModal(t, m, modalAccountSelector)
		if m.modalCursor != 0 {
			t.Errorf("expected modalCursor 0, got %d", m.modalCursor)
		}
	})

	t.Run("with matching filter", func(t *testing.T) {
		acctID := int64(42)
		m := NewBuilder().WithAccounts(
			query.AccountInfo{ID: 10, Identifier: "a@example.com"},
			query.AccountInfo{ID: 42, Identifier: "b@example.com"},
		).Build()
		m.accountFilter = &acctID
		m.openAccountSelector()
		assertModal(t, m, modalAccountSelector)
		if m.modalCursor != 2 { // index 1 + 1 for "All Accounts"
			t.Errorf("expected modalCursor 2, got %d", m.modalCursor)
		}
	})
}

// =============================================================================
// Filter Toggle Modal Tests
// =============================================================================

func TestFilterToggleModal(t *testing.T) {
	model := NewBuilder().WithPageSize(10).WithSize(100, 20).Build()

	// Press 'f' to open filter modal
	m := applyAggregateKey(t, model, key('f'))

	if m.modal != modalFilterToggle {
		t.Errorf("expected modalFilterToggle, got %v", m.modal)
	}
	if m.modalCursor != 0 {
		t.Errorf("expected modalCursor = 0, got %d", m.modalCursor)
	}

	// Space toggles checkbox at cursor 0 (attachments only) — modal stays open
	m, _ = applyModalKey(t, m, key(' '))

	if m.modal != modalFilterToggle {
		t.Errorf("expected modal to stay open after Space, got %v", m.modal)
	}
	if !m.filters.attachmentsOnly {
		t.Error("expected attachmentsOnly = true after toggling")
	}

	// Toggle again with 'x' to uncheck
	m, _ = applyModalKey(t, m, key('x'))
	if m.filters.attachmentsOnly {
		t.Error("expected attachmentsOnly = false after second toggle")
	}

	// Navigate down to "Hide deleted from source"
	m, _ = applyModalKey(t, m, key('j'))
	if m.modalCursor != 1 {
		t.Errorf("expected modalCursor = 1, got %d", m.modalCursor)
	}

	// Space toggles
	m, _ = applyModalKey(t, m, key(' '))
	if !m.filters.hideDeletedFromSource {
		t.Error("expected hideDeletedFromSource = true after Space toggle")
	}

	// Enter applies and closes modal
	var cmd tea.Cmd
	m, cmd = applyModalKey(t, m, keyEnter())

	if m.modal != modalNone {
		t.Errorf("expected modalNone after Enter, got %v", m.modal)
	}
	if cmd == nil {
		t.Error("expected command to reload data after Enter")
	}

	// Esc also applies and closes
	m2 := applyAggregateKey(t, model, key('f'))
	m2, cmd = applyModalKey(t, m2, keyEsc())
	if m2.modal != modalNone {
		t.Errorf("expected modalNone after Esc, got %v", m2.modal)
	}
	if cmd == nil {
		t.Error("expected command to reload data after Esc")
	}
}

func TestFilterToggleInMessageList(t *testing.T) {
	model := NewBuilder().WithLevel(levelMessageList).WithPageSize(10).WithSize(100, 20).Build()

	// Press 'f' to open filter modal in message list
	m := applyMessageListKey(t, model, key('f'))

	if m.modal != modalFilterToggle {
		t.Errorf("expected modalFilterToggle, got %v", m.modal)
	}

	// Space toggles "Only with attachments"
	m, _ = applyModalKey(t, m, key(' '))
	if !m.filters.attachmentsOnly {
		t.Error("expected attachmentsOnly = true")
	}

	// Enter applies and closes
	var cmd tea.Cmd
	m, cmd = applyModalKey(t, m, keyEnter())
	if m.modal != modalNone {
		t.Errorf("expected modalNone, got %v", m.modal)
	}
	if cmd == nil {
		t.Error("expected command to reload messages")
	}
}

func TestOpenFilterModal(t *testing.T) {
	m := NewBuilder().Build()

	m.openFilterModal()
	assertModal(t, m, modalFilterToggle)
	if m.modalCursor != 0 {
		t.Errorf("expected modalCursor 0, got %d", m.modalCursor)
	}
}

func TestFilterToggleInDrillDown(t *testing.T) {
	// Simulate being in a sub-aggregate drill-down with filters initially on.
	model := NewBuilder().WithLevel(levelDrillDown).WithPageSize(10).WithSize(100, 20).Build()
	model.filters.attachmentsOnly = true
	model.filters.hideDeletedFromSource = true
	model.drillFilter = query.MessageFilter{
		Sender:                "alice@example.com",
		WithAttachmentsOnly:   true,
		HideDeletedFromSource: true,
	}

	// Open filter modal
	m := applyAggregateKey(t, model, key('f'))
	if m.modal != modalFilterToggle {
		t.Fatalf("expected modalFilterToggle, got %v", m.modal)
	}

	// Toggle both filters off using space/x
	m, _ = applyModalKey(t, m, key(' ')) // cursor 0: toggle attachmentsOnly off
	m, _ = applyModalKey(t, m, key('j')) // move to cursor 1
	m, _ = applyModalKey(t, m, key('x')) // toggle hideDeletedFromSource off

	if m.filters.attachmentsOnly {
		t.Error("expected attachmentsOnly = false after toggle")
	}
	if m.filters.hideDeletedFromSource {
		t.Error("expected hideDeletedFromSource = false after toggle")
	}

	// Close modal — drillFilter must be resynced
	m, cmd := applyModalKey(t, m, keyEsc())

	if m.modal != modalNone {
		t.Errorf("expected modalNone, got %v", m.modal)
	}
	if cmd == nil {
		t.Error("expected command to reload data after Esc")
	}

	// Verify drillFilter was resynced with the updated global toggles
	if m.drillFilter.WithAttachmentsOnly {
		t.Error("expected drillFilter.WithAttachmentsOnly = false after resync")
	}
	if m.drillFilter.HideDeletedFromSource {
		t.Error("expected drillFilter.HideDeletedFromSource = false after resync")
	}

	// Non-toggle fields should be preserved
	if m.drillFilter.Sender != "alice@example.com" {
		t.Errorf("expected drillFilter.Sender preserved, got %q", m.drillFilter.Sender)
	}
}
