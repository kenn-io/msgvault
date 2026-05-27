package tui

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
)

// ansiStart is the escape sequence prefix found in styled terminal output.
const ansiStart = "\x1b["

// colorProfileMu serializes tests that mutate the global lipgloss color profile.
var colorProfileMu sync.Mutex

// forceColorProfile sets lipgloss to ANSI color output for tests that assert
// on styled output. It acquires colorProfileMu to prevent data races with
// parallel tests and restores the original profile via t.Cleanup.
func forceColorProfile(t *testing.T) {
	t.Helper()
	colorProfileMu.Lock()
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(orig)
		colorProfileMu.Unlock()
	})
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func stripANSI(s string) string {
	return ansiPattern.ReplaceAllString(s, "")
}

// MockConfig holds configuration for creating a mock engine in tests.
// Using a struct instead of positional arguments makes tests more readable
// and easier to extend as the engine interface evolves.
type MockConfig struct {
	Rows     []query.AggregateRow
	Messages []query.MessageSummary
	Detail   *query.MessageDetail
	GmailIDs []string
}

// newMockEngine creates a querytest.MockEngine configured for TUI testing.
// The messages slice is returned from ListMessages, Search, SearchFast, and
// SearchFastCount, matching the legacy mockEngine behavior.
func newMockEngine(cfg MockConfig) *querytest.MockEngine {
	eng := &querytest.MockEngine{
		AggregateRows:     cfg.Rows,
		ListResults:       cfg.Messages,
		SearchResults:     cfg.Messages,
		SearchFastResults: cfg.Messages,
		GmailIDs:          cfg.GmailIDs,
	}
	eng.GetMessageFunc = func(_ context.Context, _ int64) (*query.MessageDetail, error) {
		return cfg.Detail, nil
	}
	eng.SearchFastCountFunc = func(_ context.Context, _ *search.Query, _ query.MessageFilter) (int64, error) {
		return int64(len(cfg.Messages)), nil
	}
	return eng
}

// =============================================================================
// Test Fixtures
// =============================================================================

// TestModelBuilder helps construct Model instances for testing.
type TestModelBuilder struct {
	rows               []query.AggregateRow
	messages           []query.MessageSummary
	messageDetail      *query.MessageDetail
	gmailIDs           []string
	accounts           []query.AccountInfo
	width              int
	height             int
	pageSize           int  // explicit override; 0 means auto-calculate from height
	rawPageSize        bool // when true, pageSize is set without clamping
	viewType           query.ViewType
	viewTypeSet        bool // tracks whether viewType was explicitly set (fixes ViewSenders iota 0 collision)
	level              viewLevel
	dataDir            string
	version            string
	loading            *bool // nil = auto (false if data provided), non-nil = explicit
	modal              *modalType
	accountFilter      *int64
	stats              *query.TotalStats
	contextStats       *query.TotalStats
	activeSearchQuery  string
	activeSearchMode   *searchModeKind
	selectedAggregates *selectedAggregates
}

func NewBuilder() *TestModelBuilder {
	return &TestModelBuilder{
		width:   100,
		height:  24,
		dataDir: "/tmp/test",
		version: "test123",
	}
}

func (b *TestModelBuilder) WithRows(rows ...query.AggregateRow) *TestModelBuilder {
	b.rows = rows
	return b
}

func (b *TestModelBuilder) WithMessages(msgs ...query.MessageSummary) *TestModelBuilder {
	b.messages = msgs
	return b
}

func (b *TestModelBuilder) WithDetail(detail *query.MessageDetail) *TestModelBuilder {
	b.messageDetail = detail
	return b
}

func (b *TestModelBuilder) WithGmailIDs(ids ...string) *TestModelBuilder {
	b.gmailIDs = ids
	return b
}

func (b *TestModelBuilder) WithAccounts(accounts ...query.AccountInfo) *TestModelBuilder {
	b.accounts = accounts
	return b
}

// WithStandardAccounts adds the standard testAccounts (user1@gmail.com, user2@gmail.com).
func (b *TestModelBuilder) WithStandardAccounts() *TestModelBuilder {
	b.accounts = append([]query.AccountInfo(nil), testAccounts...)
	return b
}

func (b *TestModelBuilder) WithSize(width, height int) *TestModelBuilder {
	b.width = width
	b.height = height
	return b
}

func (b *TestModelBuilder) WithLevel(level viewLevel) *TestModelBuilder {
	b.level = level
	return b
}

func (b *TestModelBuilder) WithViewType(vt query.ViewType) *TestModelBuilder {
	b.viewType = vt
	b.viewTypeSet = true
	return b
}

func (b *TestModelBuilder) WithPageSize(size int) *TestModelBuilder {
	b.pageSize = size
	b.rawPageSize = false
	return b
}

// WithPageSizeRaw sets pageSize without clamping, allowing zero/negative values for edge-case testing.
func (b *TestModelBuilder) WithPageSizeRaw(size int) *TestModelBuilder {
	b.pageSize = size
	b.rawPageSize = true
	return b
}

func (b *TestModelBuilder) WithLoading(loading bool) *TestModelBuilder {
	b.loading = &loading
	return b
}

func (b *TestModelBuilder) WithModal(mt modalType) *TestModelBuilder {
	b.modal = &mt
	return b
}

func (b *TestModelBuilder) WithAccountFilter(id *int64) *TestModelBuilder {
	b.accountFilter = id
	return b
}

// selectedAggregates holds the aggregate selection state for the builder.
type selectedAggregates struct {
	keys        []string
	viewType    query.ViewType
	viewTypeSet bool // tracks whether viewType was explicitly set
}

// WithSelectedAggregates pre-populates aggregate selection with the given keys.
// The viewType is inferred from the builder's viewType setting.
func (b *TestModelBuilder) WithSelectedAggregates(keys ...string) *TestModelBuilder {
	if b.selectedAggregates == nil {
		b.selectedAggregates = &selectedAggregates{}
	}
	b.selectedAggregates.keys = keys
	return b
}

// WithSelectedAggregatesViewType sets the viewType for aggregate selection.
// Use this when the selection viewType differs from the model's viewType.
func (b *TestModelBuilder) WithSelectedAggregatesViewType(vt query.ViewType) *TestModelBuilder {
	if b.selectedAggregates == nil {
		b.selectedAggregates = &selectedAggregates{}
	}
	b.selectedAggregates.viewType = vt
	b.selectedAggregates.viewTypeSet = true
	return b
}

func (b *TestModelBuilder) WithStats(stats *query.TotalStats) *TestModelBuilder {
	b.stats = stats
	return b
}

func (b *TestModelBuilder) WithContextStats(stats *query.TotalStats) *TestModelBuilder {
	b.contextStats = stats
	return b
}

func (b *TestModelBuilder) Build() Model {
	engine := newMockEngine(MockConfig{
		Rows:     b.rows,
		Messages: b.messages,
		Detail:   b.messageDetail,
		GmailIDs: b.gmailIDs,
	})

	model := New(engine, Options{DataDir: b.dataDir, Version: b.version})

	b.configureDimensions(&model)
	b.configureData(&model)
	b.configureState(&model)

	return model
}

// calculatePageSize determines the page size based on builder configuration.
func (b *TestModelBuilder) calculatePageSize() int {
	if b.rawPageSize {
		return b.pageSize
	}
	if b.pageSize > 0 {
		return b.pageSize
	}
	size := b.height - 5
	if size < 1 {
		return 1
	}
	return size
}

// configureDimensions sets width, height, and pageSize on the model.
func (b *TestModelBuilder) configureDimensions(m *Model) {
	m.width = b.width
	m.height = b.height
	m.pageSize = b.calculatePageSize()
}

// configureData pre-populates the model with test data (rows, messages, detail).
func (b *TestModelBuilder) configureData(m *Model) {
	if len(b.rows) > 0 {
		m.rows = b.rows
	}
	if len(b.messages) > 0 {
		m.messages = b.messages
	}
	if b.messageDetail != nil {
		m.messageDetail = b.messageDetail
	}
}

// configureState applies loading, level, viewType, accounts, modal, filters, and selection.
func (b *TestModelBuilder) configureState(m *Model) {
	// Loading: explicit if set, otherwise false only when data is provided
	if b.loading != nil {
		m.loading = *b.loading
	} else if len(b.rows) > 0 || len(b.messages) > 0 || b.messageDetail != nil {
		m.loading = false
	}

	if b.level != levelAggregates {
		m.level = b.level
	}

	if b.viewTypeSet {
		m.viewType = b.viewType
	}

	if len(b.accounts) > 0 {
		m.accounts = b.accounts
	}

	if b.modal != nil {
		m.modal = *b.modal
	}

	if b.accountFilter != nil {
		m.accountFilter = b.accountFilter
	}

	if b.stats != nil {
		m.stats = b.stats
	}

	if b.contextStats != nil {
		m.contextStats = b.contextStats
	}

	if b.activeSearchMode != nil {
		m.inlineSearchActive = true
		m.searchMode = *b.activeSearchMode
		m.searchInput.SetValue(b.activeSearchQuery)
	}

	if b.selectedAggregates != nil {
		for _, k := range b.selectedAggregates.keys {
			m.selection.aggregateKeys[k] = true
		}
		if b.selectedAggregates.viewTypeSet {
			m.selection.aggregateViewType = b.selectedAggregates.viewType
		} else {
			m.selection.aggregateViewType = m.viewType
		}
	}
}

// sendKey sends a key message to the model and returns the updated concrete Model.
func sendKey(t *testing.T, m Model, k tea.KeyMsg) (Model, tea.Cmd) {
	t.Helper()
	newM, cmd := m.Update(k)
	return asModel(t, newM), cmd
}

// sendMsg sends any tea.Msg through Update and returns the concrete Model.
func sendMsg(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	newM, _ := m.Update(msg)
	return asModel(t, newM)
}

// asModel asserts that a tea.Model is the concrete Model type, failing the
// test otherwise. Centralizes the checked type assertion used across the TUI
// tests where Update/handle* return the tea.Model interface.
func asModel(t *testing.T, m tea.Model) Model {
	t.Helper()
	model, ok := m.(Model)
	require.True(t, ok, "expected Model, got %T", m)
	return model
}

// assertModal checks that the model is in the expected modal state.
func assertModal(t *testing.T, m Model, expected modalType) {
	t.Helper()
	assert.Equal(t, expected, m.modal)
}

// assertModalCleared checks that the modal is dismissed and modalResult is empty.
func assertModalCleared(t *testing.T, m Model) {
	t.Helper()
	assert.Equal(t, modalNone, m.modal)
	assert.Empty(t, m.modalResult)
}

// assertPendingManifestCleared checks that pendingManifest is nil.
func assertPendingManifestCleared(t *testing.T, m Model) {
	t.Helper()
	assert.Nil(t, m.pendingManifest)
}

// assertPendingManifestGmailIDs checks that pendingManifest has the expected number of Gmail IDs.
func assertPendingManifestGmailIDs(t *testing.T, m Model, expectedCount int) {
	t.Helper()
	require.NotNil(t, m.pendingManifest, "expected pendingManifest to be set")
	assert.Len(t, m.pendingManifest.GmailIDs, expectedCount)
}

// assertSelectionViewTypeMatches checks that aggregateViewType matches the model's viewType.
func assertSelectionViewTypeMatches(t *testing.T, m Model) {
	t.Helper()
	assert.Equal(t, m.viewType, m.selection.aggregateViewType, "aggregateViewType should match viewType")
}

// assertHasSelection checks that the model has at least one selection.
func assertHasSelection(t *testing.T, m Model, expected bool) {
	t.Helper()
	assert.Equal(t, expected, m.hasSelection(), "hasSelection()")
}

// assertMessageSelected checks that a specific message ID is selected.
func assertMessageSelected(t *testing.T, m Model, id int64) {
	t.Helper()
	assert.True(t, m.selection.messageIDs[id], "expected message ID %d to be selected", id)
}

// assertFilterKey checks the model's filterKey field.
func assertFilterKey(t *testing.T, m Model, expected string) {
	t.Helper()
	assert.Equal(t, expected, m.filterKey, "filterKey")
}

// assertBreadcrumbCount checks the number of breadcrumbs.
func assertBreadcrumbCount(t *testing.T, m Model, expected int) {
	t.Helper()
	assert.Len(t, m.breadcrumbs, expected)
}

// assertLevel checks that the model is at the expected view level.
func assertLevel(t *testing.T, m Model, expected viewLevel) {
	t.Helper()
	assert.Equal(t, expected, m.level, "level")
}

// Common test data.
var (
	// testAggregateRows provides a standard set of aggregate rows for testing.
	testAggregateRows = []query.AggregateRow{
		{Key: "alice@example.com", Count: 100, TotalSize: 1000, AttachmentCount: 5},
		{Key: "bob@example.com", Count: 50, TotalSize: 500, AttachmentCount: 2},
		{Key: "charlie@example.com", Count: 25, TotalSize: 250, AttachmentCount: 1},
	}

	// testAccounts provides a standard pair of test accounts for deletion tests.
	testAccounts = []query.AccountInfo{
		{ID: 1, Identifier: "user1@gmail.com"},
		{ID: 2, Identifier: "user2@gmail.com"},
	}

	// standardRows provides the common alice/bob pair used in view render tests.
	standardRows = []query.AggregateRow{
		{Key: "alice@example.com", Count: 100, TotalSize: 500000},
		{Key: "bob@example.com", Count: 50, TotalSize: 250000},
	}
)

// standardStats returns a fresh stats object (1000 msgs, ~5MB, 50 attachments)
// for each call, preventing cross-test state leakage.
func standardStats() *query.TotalStats {
	return &query.TotalStats{MessageCount: 1000, TotalSize: 5000000, AttachmentCount: 50}
}

// newTestModelWithRows creates a test model pre-populated with aggregate rows.
// The model is returned with loading=false since data is present.
// Use NewBuilder().WithRows(...).WithLoading(true) if you need loading=true.
func newTestModelWithRows(rows []query.AggregateRow) Model {
	return NewBuilder().
		WithRows(rows...).
		WithPageSize(10).
		Build()
}

// newTestModelAtLevel creates a test model at the specified navigation level.
// This helper uses the TestModelBuilder internally for consistency.
func newTestModelAtLevel(level viewLevel) Model {
	return NewBuilder().
		WithLevel(level).
		WithPageSize(10).
		Build()
}

// withSearchQuery sets a search query on the model.
func (m Model) withSearchQuery(q string) Model {
	m.searchQuery = q
	return m
}

// withRequestID sets the aggregate request ID for testing stale response handling.
func (m Model) withAggregateRequestID() Model {
	m.aggregateRequestID = 1
	return m
}

// withContextStats sets context stats on the model.
func (m Model) withContextStats(stats *query.TotalStats) Model {
	m.contextStats = stats
	return m
}

// withGlobalStats sets global stats on the model.
func (m Model) withGlobalStats(stats *query.TotalStats) Model {
	m.stats = stats
	return m
}

// sumAggregateStats calculates expected totals from aggregate rows.
func sumAggregateStats(rows []query.AggregateRow) (count, size, attachments int64) {
	for _, row := range rows {
		count += row.Count
		size += row.TotalSize
		attachments += row.AttachmentCount
	}
	return
}

// -----------------------------------------------------------------------------
// Key Event Helpers - reduce verbosity of tea.KeyMsg construction
// -----------------------------------------------------------------------------

// key returns a KeyMsg for a single rune (e.g., key('x'), key(' ')).
func key(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// keyEnter returns a KeyMsg for the Enter key.
func keyEnter() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyEnter}
}

// keyEsc returns a KeyMsg for the Escape key.
func keyEsc() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyEscape}
}

// keyTab returns a KeyMsg for the Tab key.
func keyTab() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyTab}
}

// keyDown returns a KeyMsg for the Down arrow key.
func keyDown() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyDown}
}

// keyShiftTab returns a KeyMsg for Shift+Tab.
func keyShiftTab() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyShiftTab}
}

// keyLeft returns a KeyMsg for the Left arrow key.
func keyLeft() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyLeft}
}

// keyRight returns a KeyMsg for the Right arrow key.
func keyRight() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRight}
}

// keyHome returns a KeyMsg for the Home key.
func keyHome() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyHome}
}

// -----------------------------------------------------------------------------
// Data Factories - create test data with minimal boilerplate
// -----------------------------------------------------------------------------

// makeRow creates an AggregateRow with the given key and count.
func makeRow(key string, count int) query.AggregateRow {
	return query.AggregateRow{Key: key, Count: int64(count)}
}

// makeRows creates 10 AggregateRows with sequential keys ("row-0", "row-1", ...).
func makeRows() []query.AggregateRow {
	const n = 10
	rows := make([]query.AggregateRow, n)
	for i := range rows {
		rows[i] = query.AggregateRow{
			Key:   fmt.Sprintf("row-%d", i),
			Count: int64((i + 1) * 10),
		}
	}
	return rows
}

// makeMessages creates n MessageSummary values with sequential IDs and subjects.
func makeMessages(n int) []query.MessageSummary {
	msgs := make([]query.MessageSummary, n)
	for i := range msgs {
		msgs[i] = query.MessageSummary{
			ID:      int64(i + 1),
			Subject: fmt.Sprintf("Subject %d", i+1),
		}
	}
	return msgs
}

// assertSelected checks that the given key is selected.
func assertSelected(t *testing.T, m Model, key string) {
	t.Helper()
	assert.True(t, m.selection.aggregateKeys[key], "expected %q to be selected", key)
}

// assertNotSelected checks that the given key is not selected.
func assertNotSelected(t *testing.T, m Model, key string) {
	t.Helper()
	assert.False(t, m.selection.aggregateKeys[key], "expected %q to not be selected", key)
}

// assertSelectionCount checks the number of selected items.
func assertSelectionCount(t *testing.T, m Model, expected int) {
	t.Helper()
	assert.Len(t, m.selection.aggregateKeys, expected)
}

// -----------------------------------------------------------------------------
// Key Application Helpers - remove handleXKeys casting boilerplate
// -----------------------------------------------------------------------------

// selectRow moves the cursor to the first row and toggles selection with space.
func selectRow(t *testing.T, m Model) Model {
	t.Helper()
	m.cursor = 0
	return applyAggregateKey(t, m, key(' '))
}

// assertPendingManifest asserts that pendingManifest is non-nil and its Account
// filter matches wantAccount.
func assertPendingManifest(t *testing.T, m Model, wantAccount string) {
	t.Helper()
	require.NotNil(t, m.pendingManifest, "expected pendingManifest to be set")
	assert.Equal(t, wantAccount, m.pendingManifest.Filters.Account, "manifest account")
}

// applyAggregateKey sends a key through handleAggregateKeys and returns the concrete Model.
func applyAggregateKey(t *testing.T, m Model, k tea.KeyMsg) Model {
	t.Helper()
	newModel, _ := m.handleAggregateKeys(k)
	return asModel(t, newModel)
}

// applyAggregateKeyWithCmd sends a key through handleAggregateKeys and returns Model and Cmd.
func applyAggregateKeyWithCmd(t *testing.T, m Model, k tea.KeyMsg) (Model, tea.Cmd) {
	t.Helper()
	newModel, cmd := m.handleAggregateKeys(k)
	return asModel(t, newModel), cmd
}

// applyMessageListKey sends a key through handleMessageListKeys and returns the concrete Model.
func applyMessageListKey(t *testing.T, m Model, k tea.KeyMsg) Model {
	t.Helper()
	newModel, _ := m.handleMessageListKeys(k)
	return asModel(t, newModel)
}

// applyMessageListKeyWithCmd sends a key through handleMessageListKeys and returns Model and Cmd.
func applyMessageListKeyWithCmd(t *testing.T, m Model, k tea.KeyMsg) (Model, tea.Cmd) {
	t.Helper()
	newModel, cmd := m.handleMessageListKeys(k)
	return asModel(t, newModel), cmd
}

// applyModalKey sends a key through handleModalKeys and returns the concrete Model and Cmd.
func applyModalKey(t *testing.T, m Model, k tea.KeyMsg) (Model, tea.Cmd) {
	t.Helper()
	newModel, cmd := m.handleModalKeys(k)
	return asModel(t, newModel), cmd
}

// applyDetailKey sends a key through handleMessageDetailKeys and returns the concrete Model.
func applyDetailKey(t *testing.T, m Model, k tea.KeyMsg) Model {
	t.Helper()
	newModel, _ := m.handleMessageDetailKeys(k)
	return asModel(t, newModel)
}

// -----------------------------------------------------------------------------
// View Fit Helpers - centralize line counting and resize logic
// -----------------------------------------------------------------------------

// countViewLines returns the number of non-trailing-empty lines in a view string.
func countViewLines(view string) int {
	lines := strings.Split(view, "\n")
	actual := len(lines)
	if actual > 0 && lines[actual-1] == "" {
		actual--
	}
	return actual
}

// assertViewFitsHeight checks that the rendered view fits within the given height.
func assertViewFitsHeight(t *testing.T, view string, height int) {
	t.Helper()
	actual := countViewLines(view)
	assert.LessOrEqual(t, actual, height, "View has %d lines but terminal height is %d", actual, height)
}

// resizeModel sends a WindowSizeMsg and returns the updated model.
func resizeModel(t *testing.T, m Model, w, h int) Model {
	t.Helper()
	newModel, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return asModel(t, newModel)
}

// assertState checks level, viewType, and cursor in one call.
func assertState(t *testing.T, m Model, level viewLevel, view query.ViewType, cursor int) {
	t.Helper()
	assert.Equal(t, level, m.level, "level")
	assert.Equal(t, view, m.viewType, "viewType")
	assert.Equal(t, cursor, m.cursor, "cursor")
}

// drillDown presses Enter on the current cursor item and returns the updated Model.
func drillDown(t *testing.T, m Model) Model {
	t.Helper()
	m, _ = sendKey(t, m, keyEnter())
	return m
}

// -----------------------------------------------------------------------------
// Search Helpers - reduce search test boilerplate
// -----------------------------------------------------------------------------

// WithActiveSearch configures the builder's model for inline search state.
func (b *TestModelBuilder) WithActiveSearch(q string, mode searchModeKind) *TestModelBuilder {
	b.activeSearchQuery = q
	b.activeSearchMode = &mode
	return b
}

// applySearchResults simulates the arrival of search results via Update.
func applySearchResults(t *testing.T, m Model, reqID uint64, msgs []query.MessageSummary, total int64) Model {
	t.Helper()
	msg := searchResultsMsg{
		messages:   msgs,
		requestID:  reqID,
		totalCount: total,
	}
	newModel, _ := m.Update(msg)
	return asModel(t, newModel)
}

// applySearchResultsWithStats simulates search results that include aggregate stats.
func applySearchResultsWithStats(t *testing.T, m Model, reqID uint64, msgs []query.MessageSummary, total int64, stats *query.TotalStats) Model {
	t.Helper()
	msg := searchResultsMsg{
		messages:   msgs,
		requestID:  reqID,
		totalCount: total,
		stats:      stats,
	}
	newModel, _ := m.Update(msg)
	return asModel(t, newModel)
}

// applySearchResultsAppend simulates appended (paginated) search results.
func applySearchResultsAppend(t *testing.T, m Model, reqID uint64, msgs []query.MessageSummary, total int64) Model {
	t.Helper()
	msg := searchResultsMsg{
		messages:   msgs,
		requestID:  reqID,
		totalCount: total,
		append:     true,
	}
	newModel, _ := m.Update(msg)
	return asModel(t, newModel)
}

// assertContextStats checks contextStats fields. Use -1 for size or attachments to skip that check.
func assertContextStats(t *testing.T, m Model, wantCount int, wantSize int64, wantAttach int) {
	t.Helper()
	require.NotNil(t, m.contextStats, "expected contextStats to be set")
	assert.Equal(t, int64(wantCount), m.contextStats.MessageCount, "MessageCount")
	if wantSize != -1 {
		assert.Equal(t, wantSize, m.contextStats.TotalSize, "TotalSize")
	}
	if wantAttach != -1 {
		assert.Equal(t, int64(wantAttach), m.contextStats.AttachmentCount, "AttachmentCount")
	}
}

// assertSearchMode checks the model's searchMode field.
func assertSearchMode(t *testing.T, m Model, expected searchModeKind) {
	t.Helper()
	assert.Equal(t, expected, m.searchMode, "searchMode")
}

// assertLoading checks the model's loading state fields.
func assertLoading(t *testing.T, m Model, loading, inlineSearchLoading bool) {
	t.Helper()
	assert.Equal(t, loading, m.loading, "loading")
	assert.Equal(t, inlineSearchLoading, m.inlineSearchLoading, "inlineSearchLoading")
}

// assertCmd checks whether a command is nil or non-nil as expected.
func assertCmd(t *testing.T, cmd tea.Cmd, wantCmd bool) {
	t.Helper()
	if wantCmd {
		assert.NotNil(t, cmd, "expected command to be returned")
	} else {
		assert.Nil(t, cmd, "expected no command")
	}
}

// assertSearchQuery checks the model's searchQuery field.
func assertSearchQuery(t *testing.T, m Model, expected string) {
	t.Helper()
	assert.Equal(t, expected, m.searchQuery, "searchQuery")
}

// assertInlineSearchActive checks the model's inlineSearchActive field.
func assertInlineSearchActive(t *testing.T, m Model, expected bool) {
	t.Helper()
	assert.Equal(t, expected, m.inlineSearchActive, "inlineSearchActive")
}

// applyInlineSearchKey sends a key through handleInlineSearchKeys and returns Model and Cmd.
func applyInlineSearchKey(t *testing.T, m Model, k tea.KeyMsg) (Model, tea.Cmd) {
	t.Helper()
	newModel, cmd := m.handleInlineSearchKeys(k)
	return asModel(t, newModel), cmd
}

// =============================================================================
// Tests
// =============================================================================
