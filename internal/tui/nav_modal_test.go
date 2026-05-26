package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
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

	assertpkg.False(t, model.quitting, "should not be quitting yet")
	assertpkg.Nil(t, cmd, "should not have quit command yet")

	// Press 'n' to cancel
	model, _ = sendKey(t, model, key('n'))

	assertModal(t, model, modalNone)
}

func TestQuitConfirmationConfirm(t *testing.T) {
	model := NewBuilder().WithModal(modalQuitConfirm).WithPageSize(10).WithSize(100, 20).Build()

	// Press 'y' to confirm quit
	m, cmd := applyModalKey(t, model, key('y'))

	assertpkg.True(t, m.quitting)
	assertpkg.NotNil(t, cmd, "expected quit command")
}

// =============================================================================
// Account Selector Modal Tests
// =============================================================================

func TestAccountSelectorModal(t *testing.T) {
	assert := assertpkg.New(t)
	model := NewBuilder().
		WithAccounts(
			query.AccountInfo{ID: 1, Identifier: "alice@example.com"},
			query.AccountInfo{ID: 2, Identifier: "bob@example.com"},
		).
		WithPageSize(10).WithSize(100, 20).
		Build()

	// Press 'A' to open account selector
	m := applyAggregateKey(t, model, key('A'))

	assert.Equal(modalAccountSelector, m.modal)
	assert.Equal(0, m.modalCursor, "expected All Accounts")

	// Navigate down
	m, _ = applyModalKey(t, m, key('j'))
	assert.Equal(1, m.modalCursor)

	// Select account
	var cmd tea.Cmd
	m, cmd = applyModalKey(t, m, keyEnter())

	assert.Equal(modalNone, m.modal, "after selection")
	requirepkg.NotNil(t, m.accountFilter)
	assert.Equal(int64(1), *m.accountFilter)
	assert.NotNil(cmd, "expected command to reload data")
}

func TestOpenAccountSelector(t *testing.T) {
	t.Run("no accounts", func(t *testing.T) {
		m := NewBuilder().Build()
		m.openAccountSelector()
		assertModal(t, m, modalAccountSelector)
		assertpkg.Equal(t, 0, m.modalCursor)
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
		assertpkg.Equal(t, 2, m.modalCursor, "index 1 + 1 for All Accounts")
	})
}

// =============================================================================
// Filter Toggle Modal Tests
// =============================================================================

func TestFilterToggleModal(t *testing.T) {
	assert := assertpkg.New(t)
	model := NewBuilder().WithPageSize(10).WithSize(100, 20).Build()

	// Press 'f' to open filter modal
	m := applyAggregateKey(t, model, key('f'))

	assert.Equal(modalFilterToggle, m.modal)
	assert.Equal(0, m.modalCursor)

	// Space toggles checkbox at cursor 0 (attachments only) — modal stays open
	m, _ = applyModalKey(t, m, key(' '))

	assert.Equal(modalFilterToggle, m.modal, "expected modal to stay open after Space")
	assert.True(m.filters.attachmentsOnly, "after toggling")

	// Toggle again with 'x' to uncheck
	m, _ = applyModalKey(t, m, key('x'))
	assert.False(m.filters.attachmentsOnly, "after second toggle")

	// Navigate down to "Hide deleted from source"
	m, _ = applyModalKey(t, m, key('j'))
	assert.Equal(1, m.modalCursor)

	// Space toggles
	m, _ = applyModalKey(t, m, key(' '))
	assert.True(m.filters.hideDeletedFromSource, "after Space toggle")

	// Enter applies and closes modal
	var cmd tea.Cmd
	m, cmd = applyModalKey(t, m, keyEnter())

	assert.Equal(modalNone, m.modal, "after Enter")
	assert.NotNil(cmd, "expected command to reload data after Enter")

	// Esc also applies and closes
	m2 := applyAggregateKey(t, model, key('f'))
	m2, cmd = applyModalKey(t, m2, keyEsc())
	assert.Equal(modalNone, m2.modal, "after Esc")
	assert.NotNil(cmd, "expected command to reload data after Esc")
}

func TestFilterToggleInMessageList(t *testing.T) {
	assert := assertpkg.New(t)
	model := NewBuilder().WithLevel(levelMessageList).WithPageSize(10).WithSize(100, 20).Build()

	// Press 'f' to open filter modal in message list
	m := applyMessageListKey(t, model, key('f'))

	assert.Equal(modalFilterToggle, m.modal)

	// Space toggles "Only with attachments"
	m, _ = applyModalKey(t, m, key(' '))
	assert.True(m.filters.attachmentsOnly)

	// Enter applies and closes
	var cmd tea.Cmd
	m, cmd = applyModalKey(t, m, keyEnter())
	assert.Equal(modalNone, m.modal)
	assert.NotNil(cmd, "expected command to reload messages")
}

func TestOpenFilterModal(t *testing.T) {
	m := NewBuilder().Build()

	m.openFilterModal()
	assertModal(t, m, modalFilterToggle)
	assertpkg.Equal(t, 0, m.modalCursor)
}

func TestFilterToggleInDrillDown(t *testing.T) {
	assert := assertpkg.New(t)
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
	requirepkg.Equal(t, modalFilterToggle, m.modal)

	// Toggle both filters off using space/x
	m, _ = applyModalKey(t, m, key(' ')) // cursor 0: toggle attachmentsOnly off
	m, _ = applyModalKey(t, m, key('j')) // move to cursor 1
	m, _ = applyModalKey(t, m, key('x')) // toggle hideDeletedFromSource off

	assert.False(m.filters.attachmentsOnly, "after toggle")
	assert.False(m.filters.hideDeletedFromSource, "after toggle")

	// Close modal — drillFilter must be resynced
	m, cmd := applyModalKey(t, m, keyEsc())

	assert.Equal(modalNone, m.modal)
	assert.NotNil(cmd, "expected command to reload data after Esc")

	// Verify drillFilter was resynced with the updated global toggles
	assert.False(m.drillFilter.WithAttachmentsOnly, "after resync")
	assert.False(m.drillFilter.HideDeletedFromSource, "after resync")

	// Non-toggle fields should be preserved
	assert.Equal("alice@example.com", m.drillFilter.Sender)
}
