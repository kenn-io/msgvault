// Package tui provides a terminal user interface for msgvault.
package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/update"
)

// defaultAggregateLimit is the maximum number of aggregate rows to load for display.
// The true total count is obtained via TotalUnique (COUNT(*) OVER()), so the
// footer can show "N of M" even when M exceeds this limit. This limit only
// affects how many rows are available for scrolling in the UI.
const defaultAggregateLimit = 50000

// safeCmdWithPanic wraps an async operation with panic recovery.
// The errMsg function converts a panic value into the appropriate message type.
// This eliminates boilerplate panic recovery code in all async data loading commands.
func safeCmdWithPanic(fn func() tea.Msg, errMsg func(any) tea.Msg) tea.Cmd {
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				msg = errMsg(r)
			}
		}()
		return fn()
	}
}

// defaultThreadMessageLimit is the maximum number of messages to load in a thread view.
const defaultThreadMessageLimit = 1000

// Options configuration for TUI.
type Options struct {
	DataDir   string
	ExportDir string
	Version   string

	// AggregateLimit overrides the maximum number of aggregate rows to load.
	// Zero uses the default (50,000).
	AggregateLimit int

	// ThreadMessageLimit overrides the maximum number of messages in a thread view.
	// Zero uses the default (1,000).
	ThreadMessageLimit int

	// TextEngine provides text message query operations.
	// When non-nil, Texts participates in the top-level mode cycle.
	TextEngine query.TextEngine

	// PeopleBackend provides the People directory and contact workspace.
	// When nil, embedders retain the three existing modes.
	PeopleBackend peoplebrowser.Backend

	// ManifestSaver saves staged deletion manifests. When nil, manifests are
	// saved under DataDir for tests or direct embedding.
	ManifestSaver DeletionManifestSaver

	// AttachmentReader opens attachment content streams. When nil, exports read
	// from DataDir/attachments for tests or direct embedding.
	AttachmentReader AttachmentReader

	// SemanticSearch enables the third TUI search mode. Callers should only set
	// it after the daemon reports that vector search is configured.
	SemanticSearch query.SemanticMessageSearcher

	// AnalyticsNotice, when non-empty, is shown on the info line while
	// aggregate views load. It explains slow first queries (for example,
	// the daemon falling back to live SQL because no analytics cache is
	// built for the archive).
	AnalyticsNotice string

	// SettingsBackend reads and writes the same daemon-owned settings catalog
	// used by the Web UI. When nil, the Settings surface reports unavailable.
	SettingsBackend SettingsBackend

	// CollectionScopeLister optionally supplies named Email source scopes. The
	// command root gates this capability on the daemon API schema version.
	CollectionScopeLister query.CollectionScopeLister
}

type sourceScopeKind uint8

const (
	sourceScopeAll sourceScopeKind = iota
	sourceScopeAccount
	sourceScopeCollection
)

type sourceScope struct {
	kind           sourceScopeKind
	accountID      *int64
	collectionName string
	sourceIDs      []int64
}

type scopeOptionKind uint8

const (
	scopeOptionAll scopeOptionKind = iota
	scopeOptionAccount
	scopeOptionCollection
)

type scopeOption struct {
	kind       scopeOptionKind
	label      string
	accountID  *int64
	collection query.CollectionScope
}

func allSourceScope() sourceScope { return sourceScope{} }

func accountSourceScope(id *int64) sourceScope {
	if id == nil {
		return allSourceScope()
	}
	idCopy := *id
	return sourceScope{kind: sourceScopeAccount, accountID: &idCopy}
}

func collectionSourceScope(collection query.CollectionScope) sourceScope {
	return sourceScope{
		kind:           sourceScopeCollection,
		collectionName: collection.Name,
		sourceIDs:      copySourceIDs(collection.SourceIDs),
	}
}

func (s sourceScope) isEmpty() bool {
	return s.kind == sourceScopeCollection && s.sourceIDs != nil && len(s.sourceIDs) == 0
}

func (s sourceScope) title(accounts []query.AccountInfo) string {
	if s.kind == sourceScopeCollection {
		return "Collection: " + s.collectionName
	}
	if s.accountID == nil {
		return "All Accounts"
	}
	for _, account := range accounts {
		if account.ID == *s.accountID {
			return account.Identifier
		}
	}
	return "All Accounts"
}

func (s sourceScope) apply(filter *query.MessageFilter) {
	filter.SourceID = nil
	filter.SourceIDs = nil
	switch s.kind {
	case sourceScopeAll:
		return
	case sourceScopeAccount:
		if s.accountID != nil {
			id := *s.accountID
			filter.SourceID = &id
		}
	case sourceScopeCollection:
		filter.SourceIDs = copySourceIDs(s.sourceIDs)
	}
}

func copySourceIDs(ids []int64) []int64 {
	if ids == nil {
		return nil
	}
	return append(make([]int64, 0, len(ids)), ids...)
}

func (m Model) currentSourceScope() sourceScope {
	if m.sourceScope.kind == sourceScopeAll && m.accountFilter != nil {
		return accountSourceScope(m.accountFilter)
	}
	return m.sourceScope
}

func (m Model) applyEmailSourceScope(filter *query.MessageFilter) {
	if m.mode != modeEmail {
		filter.SourceID = m.accountFilter
		filter.SourceIDs = nil
		return
	}
	scope := m.currentSourceScope()
	if !m.sourceScopeExplicit && scope.kind == sourceScopeAll && m.accountFilter == nil &&
		(filter.SourceID != nil || filter.SourceIDs != nil) {
		return
	}
	scope.apply(filter)
}

func (m Model) scopeTitle() string {
	if m.mode != modeEmail {
		return accountSourceScope(m.accountFilter).title(m.accounts)
	}
	return m.currentSourceScope().title(m.accounts)
}

func (m Model) scopeOptions() []scopeOption {
	options := []scopeOption{{kind: scopeOptionAll, label: "All Accounts"}}
	for _, account := range m.accounts {
		id := account.ID
		options = append(options, scopeOption{kind: scopeOptionAccount, label: account.Identifier, accountID: &id})
	}
	for _, collection := range m.collectionScopes {
		if collection.Name == "All" {
			continue
		}
		options = append(options, scopeOption{kind: scopeOptionCollection, label: collection.Name,
			collection: query.CollectionScope{Name: collection.Name, SourceIDs: copySourceIDs(collection.SourceIDs)}})
	}
	return options
}

func (m Model) selectorOptions() []scopeOption {
	if m.mode == modeEmail {
		return m.scopeOptions()
	}
	options := []scopeOption{{kind: scopeOptionAll}}
	for _, account := range m.selectableAccounts() {
		id := account.ID
		options = append(options, scopeOption{kind: scopeOptionAccount, label: account.Identifier, accountID: &id})
	}
	return options
}

func (s sourceScope) matches(option scopeOption) bool {
	switch option.kind {
	case scopeOptionAll:
		return s.kind == sourceScopeAll
	case scopeOptionAccount:
		return s.kind == sourceScopeAccount && s.accountID != nil && option.accountID != nil && *s.accountID == *option.accountID
	case scopeOptionCollection:
		return s.kind == sourceScopeCollection && s.collectionName == option.collection.Name
	default:
		return false
	}
}

// modalType represents the type of modal dialog.
type modalType int

const (
	modalNone modalType = iota
	modalDeleteConfirm
	modalDeleteResult
	modalAccountSelector
	modalFilterToggle
	modalExportAttachments
	modalExportResult
	modalQuitConfirm
	modalHelp
	modalError
)

// searchModeKind represents the active message search strategy.
type searchModeKind int

const (
	searchModeFast     searchModeKind = iota // Parquet metadata only (subject, sender, recipient)
	searchModeDeep                           // SQLite FTS5 (includes body text)
	searchModeSemantic                       // Hybrid lexical/vector ranking through the daemon
)

func (m Model) hasActiveSemanticSearch() bool {
	return m.searchMode == searchModeSemantic && m.searchQuery != ""
}

// selectionState tracks selected items for batch operations.
type selectionState struct {
	// Selected aggregate keys (sender emails, domains, labels, etc.)
	// Keys are scoped to the current ViewType to prevent collisions.
	aggregateKeys map[string]bool

	// ViewType that the aggregateKeys belong to
	aggregateViewType query.ViewType

	// Selected message IDs
	messageIDs map[int64]bool
}

// Model is the main TUI model following the Elm architecture.
//
// The receiver mix below is intentional: tea.Model (Init/Update/View) uses
// value receivers so each update returns a modified copy, while the in-place
// mutation helpers (toggleAggregateSelection, pushBreadcrumb, etc.) use pointer
// receivers to mutate that copy. Converting either set would break the TUI.
//
//nolint:recvcheck // intentional value/pointer mix required by tea.Model semantics
type Model struct {
	viewState // Embedded state

	// Top-level mode: Email or Texts
	mode tuiMode

	// Query engine for data access
	engine query.Engine

	// Text message query engine (nil if no text data available)
	textEngine query.TextEngine

	// Texts mode state (separate from email viewState)
	textState textState

	// Shared message-detail rendering has independent state per top-level mode.
	// messageReaderState in viewState is the active mode's instance.
	parkedMessageReaders [modeCount]messageReaderState

	// Meetings mode state (separate from email and text navigation state)
	meetingState meetingState

	// People mode dependencies and state remain separate from every other mode.
	peopleBackend peoplebrowser.Backend
	peopleState   peopleState

	// Settings is a global shell surface, deliberately separate from the
	// content-mode cycle so opening it cannot disturb content navigation.
	settingsBackend   SettingsBackend
	settings          settingsState
	settingsRequestID uint64

	// Version info for title bar
	version string

	// Notice shown on the info line while aggregate data loads (e.g. the
	// daemon is running live SQL because no analytics cache is built).
	analyticsNotice string

	// Terminal-dependent styles
	styles        tuiStyles
	markdownCache *markdownCache

	// Update notification
	updateAvailable  string // Latest version if update available
	updateIsDevBuild bool   // True if running a dev build

	// Configurable limits
	aggregateLimit     int
	threadMessageLimit int

	// Navigation
	breadcrumbs []navigationSnapshot

	// Global Stats (not view specific)
	stats                 *query.TotalStats // Global stats
	accounts              []query.AccountInfo
	collectionScopeLister query.CollectionScopeLister
	collectionScopes      []query.CollectionScope
	sourceScope           sourceScope
	sourceScopeExplicit   bool

	// Account filter (nil = all accounts)
	accountFilter *int64

	// Content filters
	filters struct {
		attachmentsOnly       bool // show only messages with attachments
		hideDeletedFromSource bool // exclude messages deleted from source
	}

	// Pagination config
	pageSize int // Rows visible per page

	// Selection state
	selection selectionState

	// Modal state
	modal            modalType
	modalCursor      int                // Cursor position within modal (for selector modals)
	modalResultTitle string             // Optional title for action-result modals
	modalResult      string             // Result message to display
	helpScroll       int                // Scroll offset for help modal
	pendingManifest  *deletion.Manifest // Manifest being confirmed

	// Action controller (deletion, export)
	actions *ActionController

	// Terminal dimensions
	width  int
	height int

	// Loading state
	loading         bool
	deletionLoading bool // True while resolving every message matching the current filter
	deletionCancel  context.CancelFunc
	err             error
	spinnerFrame    int  // Current frame index into spinnerFrames
	spinnerActive   bool // True when spinner tick is running

	// Request tracking to ignore stale async results
	aggregateRequestID     uint64 // Current request ID for aggregate data
	loadRequestID          uint64 // Current request ID for message list
	detailRequestID        uint64 // Current request ID for message detail
	searchRequestID        uint64 // Current request ID for search results
	deletionRequestID      uint64 // Current request ID for bulk deletion target resolution
	presentationGeneration uint64 // Mode activation owning shared presentation
	textRequestID          uint64 // Latest Text navigation/data request

	// Search state
	searchMode        searchModeKind // Fast metadata, deep FTS, or semantic hybrid
	semanticSearch    query.SemanticMessageSearcher
	searchInput       textinput.Model // Text input for search query
	searchTotalCount  int64           // Total matching messages (for pagination display)
	searchOffset      int             // Current offset for pagination
	searchLoadingMore bool            // True when loading additional results

	// Navigation restoration state
	restorePosition bool // When true, don't reset cursor/scroll on data load (used by goBack)

	// Inline search state (vim-like search bar on info line)
	inlineSearchActive   bool   // True when inline search bar is active
	inlineSearchDebounce uint64 // Increment to cancel pending debounce timers
	inlineSearchLoading  bool   // True when a debounced search query is in-flight
	inlineSearchError    string // Non-modal validation feedback for incomplete/invalid input
	searchHistory        []string
	searchHistoryIndex   int
	searchHistoryDraft   string

	// Pre-search snapshot: cached message list state before search began,
	// so Esc can restore instantly without re-querying.
	preSearchMessages     []query.MessageSummary
	preSearchCursor       int
	preSearchScrollOffset int
	preSearchContextStats *query.TotalStats
	// preSearchSnapshotInvalid marks that the saved list belongs to a
	// previous filter scope. Clearing search must reload the current scope.
	preSearchSnapshotInvalid bool

	// transitionBuffer holds the last rendered view during level transitions.
	// When non-empty, View() returns this string instead of rendering fresh content.
	// This prevents visual flashing during async data loads on screen transitions.
	transitionBuffer string

	// Flash message (temporary notification)
	flashMessage   string    // Message to display
	flashExpiresAt time.Time // When the flash message expires

	// Export attachments state
	exportSelection map[int]bool // Selected attachment indices for export
	exportCursor    int          // Cursor position in export modal

	// Quit flag
	quitting bool
}

// New creates a new TUI model with the given options.
func New(engine query.Engine, opts Options) Model {
	ti := textinput.New()
	ti.Placeholder = "search (Tab: deep)"
	ti.CharLimit = 200
	ti.SetWidth(50)
	meetingInput := textinput.New()
	meetingInput.Placeholder = "search meetings and transcripts"
	meetingInput.CharLimit = 200
	meetingInput.SetWidth(50)
	meetingDetailInput := textinput.New()
	meetingDetailInput.Placeholder = "find in transcript"
	meetingDetailInput.CharLimit = 200
	meetingDetailInput.SetWidth(40)
	peopleMeetingDetailInput := textinput.New()
	peopleMeetingDetailInput.Placeholder = "find in transcript"
	peopleMeetingDetailInput.CharLimit = 200
	peopleMeetingDetailInput.SetWidth(40)
	peopleInput := textinput.New()
	peopleInput.Placeholder = "search names and identifiers"
	peopleInput.CharLimit = 200
	peopleInput.SetWidth(50)

	aggLimit := opts.AggregateLimit
	if aggLimit == 0 {
		aggLimit = defaultAggregateLimit
	}
	threadLimit := opts.ThreadMessageLimit
	if threadLimit == 0 {
		threadLimit = defaultThreadMessageLimit
	}

	textEngine := opts.TextEngine
	if textEngine == nil {
		// Try type assertion as fallback
		if te, ok := engine.(query.TextEngine); ok {
			textEngine = te
		}
	}

	return Model{
		engine:                engine,
		textEngine:            textEngine,
		collectionScopeLister: opts.CollectionScopeLister,
		sourceScope:           allSourceScope(),
		peopleBackend:         opts.PeopleBackend,
		settingsBackend:       opts.SettingsBackend,
		settings:              newSettingsState(),
		actions: NewActionControllerWithOptions(engine, ActionControllerOptions{
			DataDir:             opts.DataDir,
			AttachmentOutputDir: opts.ExportDir,
			ManifestSaver:       opts.ManifestSaver,
			AttachmentReader:    opts.AttachmentReader,
		}),
		semanticSearch:     opts.SemanticSearch,
		version:            opts.Version,
		analyticsNotice:    opts.AnalyticsNotice,
		aggregateLimit:     aggLimit,
		threadMessageLimit: threadLimit,
		level:              levelAggregates,
		viewType:           query.ViewSenders,
		timeGranularity:    query.TimeMonth,
		sortField:          query.SortByCount,
		sortDirection:      query.SortDesc,
		msgSortField:       query.MessageSortByDate,
		msgSortDirection:   query.SortDesc,
		meetingState: meetingState{
			searchInput:       meetingInput,
			detailSearchInput: meetingDetailInput,
		},
		peopleState: peopleState{
			searchInput: peopleInput, location: time.Local,
			parkedMeetingState: meetingState{detailSearchInput: peopleMeetingDetailInput},
		},
		pageSize:      20,
		loading:       true,
		spinnerActive: true,
		styles:        newStyles(false),
		markdownCache: newMarkdownCache(false, noColorRequested()),
		selection: selectionState{
			aggregateKeys:     make(map[string]bool),
			aggregateViewType: query.ViewSenders, // Match initial viewType
			messageIDs:        make(map[int64]bool),
		},
		searchInput: ti,
		searchMode:  searchModeFast,
	}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		tea.RequestBackgroundColor,
		m.loadData(),
		m.loadStats(),
		m.loadAccounts(),
		m.loadCollectionScopes(),
		m.checkForUpdate(),
		spinnerTick(), // Start spinner for initial load
	)
}

// checkForUpdate runs a background update check using the cached version info.
func (m Model) checkForUpdate() tea.Cmd {
	version := m.version
	return func() tea.Msg {
		info, err := update.CheckForUpdate(version, false)
		if err != nil || info == nil {
			return updateCheckMsg{}
		}
		return updateCheckMsg{version: info.LatestVersion, isDevBuild: info.IsDevBuild}
	}
}

// dataLoadedMsg is sent when aggregate data is loaded.
type dataLoadedMsg struct {
	rows                   []query.AggregateRow
	filteredStats          *query.TotalStats // distinct message stats when search is active
	err                    error
	requestID              uint64 // To detect stale responses
	presentationGeneration uint64
}

// statsLoadedMsg is sent when stats are loaded.
type statsLoadedMsg struct {
	stats                  *query.TotalStats
	err                    error
	requestID              uint64
	presentationGeneration uint64
}

// accountsLoadedMsg is sent when accounts are loaded.
type accountsLoadedMsg struct {
	accounts []query.AccountInfo
	err      error
}

type collectionScopesLoadedMsg struct {
	scopes []query.CollectionScope
	err    error
}

// scopeLabelForLog returns a short, stable string describing the
// current account scope so it can be attached to log records.
// Uses "filtered" rather than the account identifier to avoid
// persisting email addresses in the log file.
func (m Model) scopeLabelForLog() string {
	if m.currentSourceScope().kind == sourceScopeCollection {
		return "filtered"
	}
	if m.accountFilter != nil {
		return "filtered"
	}
	return "all"
}

// updateCheckMsg is sent when the background update check completes.
type updateCheckMsg struct {
	version    string // Latest version if available
	isDevBuild bool
}

// AnalyticsNoticeMsg replaces the analytics notice shown while aggregate
// data loads. The command layer sends an empty notice after a background
// cache initialization switches the daemon from live SQL to DuckDB.
type AnalyticsNoticeMsg struct {
	Notice string
}

// loadData fetches aggregate data based on current view settings.
func (m Model) loadData() tea.Cmd {
	requestID := m.aggregateRequestID
	presentationGeneration := m.presentationGeneration
	scopeLabel := m.scopeLabelForLog()
	viewLabel := m.viewType.String()
	searchTerm := m.searchQuery
	return safeCmdWithPanic(
		func() tea.Msg {
			if m.currentSourceScope().isEmpty() {
				var filteredStats *query.TotalStats
				if m.searchQuery != "" {
					filteredStats = &query.TotalStats{}
				}
				return dataLoadedMsg{rows: []query.AggregateRow{}, filteredStats: filteredStats, requestID: requestID,
					presentationGeneration: presentationGeneration}
			}
			opts := query.AggregateOptions{
				SortField:             m.sortField,
				SortDirection:         m.sortDirection,
				Limit:                 m.aggregateLimit,
				TimeGranularity:       m.timeGranularity,
				WithAttachmentsOnly:   m.filters.attachmentsOnly,
				HideDeletedFromSource: m.filters.hideDeletedFromSource,
				SearchQuery:           m.searchQuery,
			}
			scope := m.currentSourceScope()
			opts.SourceID = scope.accountID
			opts.SourceIDs = copySourceIDs(scope.sourceIDs)

			start := time.Now()
			ctx := context.Background()
			var rows []query.AggregateRow
			var err error

			// Use an explicitly email-scoped filter at every aggregate level.
			// SubAggregate is also valid without drill criteria and, unlike the
			// generic Aggregate call, lets the TUI's mode constraint intersect
			// an explicit message_type search instead of being overridden by it.
			aggregateFilter := emailScopedMessageFilter(m.drillFilter)
			m.applyEmailSourceScope(&aggregateFilter)
			aggregateFilter.WithAttachmentsOnly = m.filters.attachmentsOnly
			aggregateFilter.HideDeletedFromSource = m.filters.hideDeletedFromSource
			rows, err = m.engine.SubAggregate(ctx, aggregateFilter, m.viewType, opts)
			if err != nil {
				slog.Warn("tui loadData failed",
					"view", viewLabel,
					"scope", scopeLabel,
					"has_search", searchTerm != "",
					"error", err.Error(),
					"duration_ms", time.Since(start).Milliseconds(),
				)
			} else {
				slog.Info("tui loadData ok",
					"view", viewLabel,
					"scope", scopeLabel,
					"has_search", searchTerm != "",
					"rows", len(rows),
					"duration_ms", time.Since(start).Milliseconds(),
				)
			}

			// When search is active, compute distinct message stats separately.
			// Summing row.Count across groups overcounts for 1:N views (Recipients, Labels)
			// where a message appears in multiple groups.
			var filteredStats *query.TotalStats
			if err == nil && opts.SearchQuery != "" {
				scopedQuery, noMatches := emailScopedSearchQuery(opts.SearchQuery)
				if noMatches {
					filteredStats = &query.TotalStats{}
				} else {
					statsOpts := query.StatsOptions{
						Filter:                &aggregateFilter,
						SourceID:              m.accountFilter,
						WithAttachmentsOnly:   m.filters.attachmentsOnly,
						HideDeletedFromSource: m.filters.hideDeletedFromSource,
						SearchQuery:           scopedQuery,
						SearchScope:           true,
						GroupBy:               m.viewType,
					}
					statsOpts.SourceID = m.currentSourceScope().accountID
					statsOpts.SourceIDs = copySourceIDs(m.currentSourceScope().sourceIDs)
					filteredStats, _ = m.engine.GetTotalStats(ctx, statsOpts)
				}
			}

			return dataLoadedMsg{
				rows: rows, filteredStats: filteredStats, err: err, requestID: requestID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return dataLoadedMsg{
				err: fmt.Errorf("query panic: %v", r), requestID: requestID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

// loadStats fetches total statistics.
func (m Model) loadStats() tea.Cmd {
	requestID := m.aggregateRequestID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			if m.currentSourceScope().isEmpty() {
				return statsLoadedMsg{stats: &query.TotalStats{}, requestID: requestID, presentationGeneration: presentationGeneration}
			}
			opts := query.StatsOptions{
				WithAttachmentsOnly:   m.filters.attachmentsOnly,
				HideDeletedFromSource: m.filters.hideDeletedFromSource,
			}
			opts.SourceID = m.currentSourceScope().accountID
			opts.SourceIDs = copySourceIDs(m.currentSourceScope().sourceIDs)
			stats, err := m.engine.GetTotalStats(context.Background(), opts)
			return statsLoadedMsg{stats: stats, err: err, requestID: requestID, presentationGeneration: presentationGeneration}
		},
		func(r any) tea.Msg {
			return statsLoadedMsg{err: fmt.Errorf("stats panic: %v", r), requestID: requestID, presentationGeneration: presentationGeneration}
		},
	)
}

// loadAccounts fetches the list of accounts.
func (m Model) loadAccounts() tea.Cmd {
	return safeCmdWithPanic(
		func() tea.Msg {
			ctx := context.Background()
			accounts, err := m.engine.ListAccounts(ctx)
			if err != nil {
				slog.Warn("tui loadAccounts: ListAccounts failed",
					"error", err)
				return accountsLoadedMsg{err: err}
			}
			slog.Info("tui loadAccounts ok",
				"accounts", len(accounts))
			return accountsLoadedMsg{accounts: accounts}
		},
		func(r any) tea.Msg {
			return accountsLoadedMsg{err: fmt.Errorf("accounts panic: %v", r)}
		},
	)
}

func (m Model) loadCollectionScopes() tea.Cmd {
	lister := m.collectionScopeLister
	return safeCmdWithPanic(
		func() tea.Msg {
			if lister == nil {
				return collectionScopesLoadedMsg{}
			}
			scopes, err := lister.ListCollectionScopes(context.Background())
			if err != nil {
				slog.Warn("tui loadCollectionScopes failed", "error", err)
				return collectionScopesLoadedMsg{err: err}
			}
			out := make([]query.CollectionScope, len(scopes))
			for i, scope := range scopes {
				out[i] = query.CollectionScope{Name: scope.Name, SourceIDs: copySourceIDs(scope.SourceIDs)}
			}
			return collectionScopesLoadedMsg{scopes: out}
		},
		func(r any) tea.Msg {
			return collectionScopesLoadedMsg{err: fmt.Errorf("collections panic: %v", r)}
		},
	)
}

// messagesLoadedMsg is sent when message list is loaded.
type messagesLoadedMsg struct {
	messages               []query.MessageSummary
	err                    error
	requestID              uint64 // To detect stale responses
	append                 bool   // True when appending paginated results to existing list
	presentationGeneration uint64
}

// messageDetailLoadedMsg is sent when message detail is loaded.
type messageDetailLoadedMsg struct {
	detail                 *query.MessageDetail
	err                    error
	requestID              uint64 // To detect stale responses
	presentationGeneration uint64
}

// searchResultsMsg is sent when search results are loaded.
type searchResultsMsg struct {
	messages               []query.MessageSummary
	totalCount             int64             // Total matching messages (for "N of M" display)
	stats                  *query.TotalStats // Aggregate stats for the search results (size, attachments, etc.)
	err                    error
	requestID              uint64 // To detect stale responses
	append                 bool   // True if these results should be appended (pagination)
	presentationGeneration uint64
}

// deletionPreparedMsg is sent when asynchronous bulk deletion target
// resolution has built a manifest or failed.
type deletionPreparedMsg struct {
	manifest  *deletion.Manifest
	err       error
	requestID uint64
}

// threadMessagesLoadedMsg is sent when thread messages are loaded.
type threadMessagesLoadedMsg struct {
	messages               []query.MessageSummary
	conversationID         int64
	truncated              bool // True if more messages exist but were limited
	err                    error
	requestID              uint64
	presentationGeneration uint64
}

// flashClearMsg clears the flash message after timeout.
type flashClearMsg struct{}

// spinnerTickMsg advances the loading spinner animation.
type spinnerTickMsg struct{}

// searchDebounceMsg fires after debounce delay to trigger inline search.
type searchDebounceMsg struct {
	query      string
	debounceID uint64
}

// exportResultMsg keeps the model's internal message name while sharing the
// action controller's public result type.
type exportResultMsg = ExportResultMsg

// inlineSearchDebounceDelay is the delay before executing inline search (fast mode).
const inlineSearchDebounceDelay = 100 * time.Millisecond

// inlineSearchHistoryLimit bounds session-only search history.
const inlineSearchHistoryLimit = 100

// deepSearchDebounceDelay is the delay before executing inline search (deep FTS mode).
const deepSearchDebounceDelay = 500 * time.Millisecond

// spinnerFrames are the Braille dot animation frames for the loading spinner.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerInterval is how fast the spinner animates.
const spinnerInterval = 80 * time.Millisecond

// flashDuration is how long flash messages are displayed.
const flashDuration = 4 * time.Second

// searchPageSize is the number of results per page for search pagination.
const searchPageSize = 100

// emailMessageType scopes the original TUI mode to email records. The shared
// query datasets also contain meetings and text messages, so every Email-mode
// query must carry this explicit constraint. Query engines interpret "email"
// as typed email plus legacy NULL/empty message_type rows.
const emailMessageType = "email"

// messageListPageSize is the number of results per page for message list pagination.
const messageListPageSize = 500

// headerFooterLines is the number of fixed lines reserved for the UI chrome:
// title bar (1) + breadcrumb (1) + table header (1) + separator (1) + footer (1).
const headerFooterLines = 5

// loadSearch executes the search query based on current mode.
func (m Model) loadSearch(queryStr string) tea.Cmd {
	return m.loadSearchWithOffset(queryStr, 0, false)
}

// loadSearchWithOffset executes the search query with pagination.
func (m Model) loadSearchWithOffset(queryStr string, offset int, appendResults bool) tea.Cmd {
	requestID := m.searchRequestID
	presentationGeneration := m.presentationGeneration
	modeLabel := "fast"
	switch m.searchMode {
	case searchModeFast:
	case searchModeDeep:
		modeLabel = "deep"
	case searchModeSemantic:
		modeLabel = "semantic"
	}
	scopeLabel := m.scopeLabelForLog()
	return safeCmdWithPanic(
		func() tea.Msg {
			ctx := context.Background()
			q := search.Parse(queryStr)
			searchFilter := emailScopedMessageFilter(m.searchFilter)
			m.applyEmailSourceScope(&searchFilter)
			if m.currentSourceScope().isEmpty() {
				return searchResultsMsg{messages: []query.MessageSummary{}, totalCount: 0, stats: &query.TotalStats{},
					requestID: requestID, append: appendResults, presentationGeneration: presentationGeneration}
			}

			start := time.Now()
			var results []query.MessageSummary
			var totalCount int64
			var stats *query.TotalStats
			var err error

			defer func() {
				if err != nil {
					slog.Warn("tui search failed",
						"query_len", len(queryStr),
						"mode", modeLabel,
						"scope", scopeLabel,
						"offset", offset,
						"error", err.Error(),
						"duration_ms", time.Since(start).Milliseconds(),
					)
					return
				}
				slog.Info("tui search ok",
					"query_len", len(queryStr),
					"mode", modeLabel,
					"scope", scopeLabel,
					"offset", offset,
					"results", len(results),
					"total", totalCount,
					"duration_ms", time.Since(start).Milliseconds(),
				)
			}()
			switch m.searchMode {
			case searchModeFast:
				// Fast search: single-scan with temp table materialization
				result, fastErr := m.engine.SearchFastWithStats(ctx, q, queryStr, searchFilter, m.viewType, searchPageSize, offset)
				if fastErr == nil {
					results = result.Messages
					totalCount = result.TotalCount
					if !appendResults {
						stats = result.Stats
					}
				}
				err = fastErr
			case searchModeDeep:
				// Deep search returns results and header metrics from one complete
				// view filter so names, conversations, and empty buckets stay scoped.
				result, deepErr := m.engine.SearchDeepWithStats(ctx, q, searchFilter, searchPageSize, offset)
				if deepErr == nil {
					results = result.Messages
					totalCount = result.TotalCount
					if !appendResults {
						stats = result.Stats
						if stats == nil {
							fallbackCount := totalCount
							if fallbackCount < 0 {
								fallbackCount = int64(len(results))
							}
							stats = &query.TotalStats{MessageCount: fallbackCount}
						}
					}
				}
				err = deepErr
			case searchModeSemantic:
				if m.semanticSearch == nil {
					err = errors.New("semantic search is not enabled")
					break
				}
				result, semanticErr := m.semanticSearch.SearchSemanticMessages(ctx, query.SemanticMessageSearchRequest{
					Query:  queryStr,
					Filter: searchFilter,
					Limit:  searchPageSize,
					Offset: offset,
				})
				err = semanticErr
				if semanticErr == nil {
					results = result.Messages
					totalCount = int64(offset + len(results))
					if result.HasMore {
						totalCount = -1
					}
					if !appendResults {
						messageCount := totalCount
						if messageCount < 0 {
							messageCount = int64(len(results))
						}
						stats = &query.TotalStats{MessageCount: messageCount}
					}
				}
			}

			return searchResultsMsg{
				messages:               results,
				totalCount:             totalCount,
				stats:                  stats,
				err:                    err,
				requestID:              requestID,
				append:                 appendResults,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return searchResultsMsg{
				err:                    fmt.Errorf("search panic: %v", r),
				requestID:              requestID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

// buildMessageFilter constructs a MessageFilter from the current model state.
func (m Model) buildMessageFilter() query.MessageFilter {
	// Start with drillFilter if set, otherwise build fresh filter
	var filter query.MessageFilter
	if m.hasDrillFilter() {
		filter = m.drillFilter
	}

	// Override sorting and pagination
	m.applyEmailSourceScope(&filter)
	filter.Sorting.Field = m.msgSortField
	filter.Sorting.Direction = m.msgSortDirection
	filter.WithAttachmentsOnly = m.filters.attachmentsOnly
	filter.HideDeletedFromSource = m.filters.hideDeletedFromSource
	filter.MessageType = emailMessageType

	// If not showing all messages and no drill filter, apply simple filter
	if !m.allMessages && !m.hasDrillFilter() {
		switch m.viewType {
		case query.ViewSenders:
			filter.Sender = m.filterKey
			if m.filterKey == "" {
				filter.SetEmptyTarget(query.ViewSenders)
			}
		case query.ViewRecipients:
			filter.Recipient = m.filterKey
			if m.filterKey == "" {
				filter.SetEmptyTarget(query.ViewRecipients)
			}
		case query.ViewDomains:
			filter.Domain = m.filterKey
			if m.filterKey == "" {
				filter.SetEmptyTarget(query.ViewDomains)
			}
		case query.ViewLabels:
			filter.Label = m.filterKey
			if m.filterKey == "" {
				filter.SetEmptyTarget(query.ViewLabels)
			}
		case query.ViewLists:
			filter.ListID = m.filterKey
		case query.ViewTime:
			filter.TimeRange.Period = m.filterKey
			filter.TimeRange.Granularity = m.timeGranularity
		default:
			// SenderNames/RecipientNames/Count aren't simple-filter targets.
		}
	}

	return filter
}

// loadMessages fetches messages based on current filter (first page).
func (m Model) loadMessages() tea.Cmd {
	return m.loadMessagesWithOffset(0, false)
}

// loadMessagesWithOffset fetches messages at the given offset. When appendMode
// is true, the results are appended to the existing message list.
func (m Model) loadMessagesWithOffset(offset int, appendMode bool) tea.Cmd {
	requestID := m.loadRequestID
	presentationGeneration := m.presentationGeneration
	scopeLabel := m.scopeLabelForLog()
	searchTerm := m.searchQuery
	return safeCmdWithPanic(
		func() tea.Msg {
			if m.currentSourceScope().isEmpty() {
				return messagesLoadedMsg{messages: []query.MessageSummary{}, requestID: requestID, append: appendMode,
					presentationGeneration: presentationGeneration}
			}
			filter := m.buildMessageFilter()
			filter.Pagination.Limit = messageListPageSize
			filter.Pagination.Offset = offset

			start := time.Now()
			messages, err := m.engine.ListMessages(context.Background(), filter)
			if err != nil {
				slog.Warn("tui loadMessages failed",
					"scope", scopeLabel,
					"has_search", searchTerm != "",
					"offset", offset,
					"append", appendMode,
					"error", err.Error(),
					"duration_ms", time.Since(start).Milliseconds(),
				)
			} else {
				slog.Info("tui loadMessages ok",
					"scope", scopeLabel,
					"has_search", searchTerm != "",
					"offset", offset,
					"append", appendMode,
					"count", len(messages),
					"duration_ms", time.Since(start).Milliseconds(),
				)
			}
			return messagesLoadedMsg{
				messages: messages, err: err, requestID: requestID, append: appendMode,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return messagesLoadedMsg{
				err: fmt.Errorf("messages panic: %v", r), requestID: requestID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

// hasDrillFilter returns true if drillFilter has any filter criteria set.
func (m Model) hasDrillFilter() bool {
	return m.drillFilter.Sender != "" ||
		m.drillFilter.SenderName != "" ||
		m.drillFilter.Recipient != "" ||
		m.drillFilter.RecipientName != "" ||
		m.drillFilter.Domain != "" ||
		m.drillFilter.Label != "" ||
		m.drillFilter.ConversationID != nil ||
		m.drillFilter.ListID != "" ||
		m.drillFilter.TimeRange.Period != "" ||
		m.drillFilter.HasEmptyTargets()
}

// drillFilterKey returns the key value from the drillFilter based on drillViewType.
func (m Model) drillFilterKey() string {
	if m.drillFilter.MatchesEmpty(m.drillViewType) {
		return "(empty)"
	}
	switch m.drillViewType {
	case query.ViewSenders:
		return m.drillFilter.Sender
	case query.ViewSenderNames:
		return m.drillFilter.SenderName
	case query.ViewRecipients:
		return m.drillFilter.Recipient
	case query.ViewRecipientNames:
		return m.drillFilter.RecipientName
	case query.ViewDomains:
		return m.drillFilter.Domain
	case query.ViewLabels:
		return m.drillFilter.Label
	case query.ViewLists:
		return m.drillFilter.ListID
	case query.ViewTime:
		return m.drillFilter.TimeRange.Period
	default:
		return ""
	}
}

// loadThreadMessages fetches all messages in a conversation/thread.
func (m Model) loadThreadMessages(conversationID int64) tea.Cmd {
	requestID := m.loadRequestID
	presentationGeneration := m.presentationGeneration
	threadLimit := m.threadMessageLimit
	return safeCmdWithPanic(
		func() tea.Msg {
			filter := query.MessageFilter{
				ConversationID: &conversationID,
				MessageType:    emailMessageType,
				Sorting:        query.MessageSorting{Field: query.MessageSortByDate, Direction: query.SortAsc},
				Pagination:     query.Pagination{Limit: threadLimit + 1}, // Request one extra to detect truncation
			}
			m.applyEmailSourceScope(&filter)
			if m.currentSourceScope().isEmpty() {
				return threadMessagesLoadedMsg{messages: []query.MessageSummary{}, conversationID: conversationID,
					requestID: requestID, presentationGeneration: presentationGeneration}
			}
			messages, err := m.engine.ListMessages(context.Background(), filter)

			// Check if truncated (more messages than limit)
			truncated := false
			if len(messages) > threadLimit {
				messages = messages[:threadLimit]
				truncated = true
			}

			return threadMessagesLoadedMsg{
				messages:               messages,
				conversationID:         conversationID,
				truncated:              truncated,
				err:                    err,
				requestID:              requestID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return threadMessagesLoadedMsg{
				err:                    fmt.Errorf("thread messages panic: %v", r),
				requestID:              requestID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

// emailScopedMessageFilter applies the Email-mode contract at the engine
// boundary. Keeping this out of hasDrillFilter means the mandatory mode scope
// does not make every top-level view look like a user-selected drill-down.
func emailScopedMessageFilter(filter query.MessageFilter) query.MessageFilter {
	filter.MessageType = emailMessageType
	return filter
}

// emailScopedSearchQuery intersects a parsed aggregate search with Email mode.
// A non-nil empty account scope is MergeFilterIntoQuery's match-nothing signal
// for conflicting message types.
func emailScopedSearchQuery(raw string) (string, bool) {
	merged := query.MergeFilterIntoQuery(
		search.Parse(raw),
		query.MessageFilter{MessageType: emailMessageType},
	)
	noMatches := merged.AccountIDs != nil && len(merged.AccountIDs) == 0
	return search.Format(merged), noMatches
}

// loadMessageDetail fetches a single message's full details.
func (m Model) loadMessageDetail(id int64) tea.Cmd {
	requestID := m.detailRequestID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			detail, err := m.engine.GetMessage(context.Background(), id)
			return messageDetailLoadedMsg{
				detail: detail, err: err, requestID: requestID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return messageDetailLoadedMsg{
				err: fmt.Errorf("message detail panic: %v", r), requestID: requestID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

// spinnerTick returns a command that fires a spinnerTickMsg after the spinner interval.
func spinnerTick() tea.Cmd {
	return tea.Tick(spinnerInterval, func(t time.Time) tea.Msg {
		return spinnerTickMsg{}
	})
}

// startSpinner returns a spinnerTick command if the spinner isn't already active,
// and marks it as active. Call this when loading begins.
func (m *Model) startSpinner() tea.Cmd {
	if m.spinnerActive {
		return nil
	}
	m.spinnerActive = true
	m.spinnerFrame = 0
	return spinnerTick()
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return m.handleKeyPress(msg)
	case tea.BackgroundColorMsg:
		m.styles = newStyles(msg.IsDark())
		m.markdownCache.setDark(msg.IsDark())
		return m, nil
	case tea.WindowSizeMsg:
		return m.handleWindowSize(msg)
	case dataLoadedMsg:
		return m.handleDataLoaded(msg)
	case statsLoadedMsg:
		return m.handleStatsLoaded(msg)
	case accountsLoadedMsg:
		return m.handleAccountsLoaded(msg)
	case collectionScopesLoadedMsg:
		return m.handleCollectionScopesLoaded(msg)
	case updateCheckMsg:
		return m.handleUpdateCheck(msg)
	case AnalyticsNoticeMsg:
		m.analyticsNotice = msg.Notice
		return m, nil
	case settingsLoadedMsg:
		return m.handleSettingsLoaded(msg)
	case settingsSavedMsg:
		return m.handleSettingsSaved(msg)
	// People messages are delegated before the shared Email handlers.
	case peopleSearchDebounceMsg:
		return m.handlePeopleSearchDebounce(msg)
	case peopleCompletionLoadedMsg:
		return m.handlePeopleCompletionLoaded(msg)
	case peopleDirectoryLoadedMsg:
		return m.handlePeopleDirectoryLoaded(msg)
	case peopleContactLoadedMsg:
		return m.handlePeopleContactLoaded(msg)
	case peopleRelationshipLoadedMsg:
		return m.handlePeopleRelationshipLoaded(msg)
	case peoplePromotedMsg:
		return m.handlePeoplePromoted(msg)
	case peopleAttributesLoadedMsg:
		return m.handlePeopleAttributesLoaded(msg)
	case peopleFieldCreatedMsg:
		return m.handlePeopleFieldCreated(msg)
	case peopleAttributeSetMsg:
		return m.handlePeopleAttributeSet(msg)
	case peopleInboxesLoadedMsg:
		return m.handlePeopleInboxesLoaded(msg)
	case peopleConversationsLoadedMsg:
		return m.handlePeopleConversationsLoaded(msg)
	case peopleConversationMessagesLoadedMsg:
		return m.handlePeopleConversationMessagesLoaded(msg)
	case peopleMessageLoadedMsg:
		return m.handlePeopleMessageLoaded(msg)
	case peopleMeetingsLoadedMsg:
		return m.handlePeopleMeetingsLoaded(msg)
	case peopleMeetingLoadedMsg:
		return m.handlePeopleMeetingLoaded(msg)
	case peopleFilesLoadedMsg:
		return m.handlePeopleFilesLoaded(msg)
	case peopleFileMessageLoadedMsg:
		return m.handlePeopleFileMessageLoaded(msg)
	case peopleFileExportedMsg:
		return m.handlePeopleFileExported(msg)
	case peopleActivityLoadedMsg:
		return m.handlePeopleActivityLoaded(msg)
	case peopleActivityMessageLoadedMsg:
		return m.handlePeopleActivityMessageLoaded(msg)
	case ExportResultMsg:
		return m.handleExportResult(msg)
	case messagesLoadedMsg:
		return m.handleMessagesLoaded(msg)
	case messageDetailLoadedMsg:
		return m.handleMessageDetailLoaded(msg)
	case threadMessagesLoadedMsg:
		return m.handleThreadMessagesLoaded(msg)
	case searchResultsMsg:
		return m.handleSearchResults(msg)
	case deletionPreparedMsg:
		return m.handleDeletionPrepared(msg)
	case flashClearMsg:
		return m.handleFlashClear()
	case searchDebounceMsg:
		return m.handleSearchDebounce(msg)
	case spinnerTickMsg:
		return m.handleSpinnerTick()
	// Text mode messages
	case textConversationsLoadedMsg:
		return m.handleTextConversationsLoaded(msg)
	case textAggregateLoadedMsg:
		return m.handleTextAggregateLoaded(msg)
	case textMessagesLoadedMsg:
		return m.handleTextMessagesLoaded(msg)
	case textMessageLoadedMsg:
		return m.handleTextMessageLoaded(msg)
	case textSearchResultMsg:
		return m.handleTextSearchResult(msg)
	case textStatsLoadedMsg:
		return m.handleTextStatsLoaded(msg)
	case meetingMessagesLoadedMsg:
		return m.handleMeetingMessagesLoaded(msg)
	case meetingSearchLoadedMsg:
		return m.handleMeetingSearchLoaded(msg)
	case meetingDetailLoadedMsg:
		return m.handleMeetingDetailLoaded(msg)
	}
	return m, nil
}

// finishModePresentation settles shared loading state only when the response
// belongs to the mode activation currently on screen.
func (m *Model) finishModePresentation(mode tuiMode, generation uint64) bool {
	if m.mode != mode || m.presentationGeneration != generation {
		return false
	}
	m.transitionBuffer = ""
	m.loading = false
	return true
}

// handleTextConversationsLoaded processes text conversations load completion.
func (m Model) handleTextConversationsLoaded(msg textConversationsLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.textRequestID {
		return m, nil
	}
	active := m.finishModePresentation(modeTexts, msg.presentationGeneration)
	if !active {
		return m, nil
	}
	if msg.err != nil {
		m.err = msg.err
		m.modal = modalError
		m.modalResult = msg.err.Error()
		return m, nil
	}
	m.textState.conversations = msg.conversations
	m.textState.stats = msg.stats
	// Clamp cursor/scrollOffset to new data bounds to prevent
	// out-of-range panics after account change or filter.
	m.textState.clampCursorToConversations()
	return m, nil
}

// handleTextAggregateLoaded processes text aggregate load completion.
func (m Model) handleTextAggregateLoaded(msg textAggregateLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.textRequestID {
		return m, nil
	}
	active := m.finishModePresentation(modeTexts, msg.presentationGeneration)
	if !active {
		return m, nil
	}
	if msg.err != nil {
		m.err = msg.err
		m.modal = modalError
		m.modalResult = msg.err.Error()
		return m, nil
	}
	m.textState.aggregateRows = msg.rows
	m.textState.stats = msg.stats
	// Clamp cursor/scrollOffset to new data bounds to prevent
	// out-of-range panics after account change or filter.
	m.textState.clampCursorToAggregates()
	return m, nil
}

// handleTextMessagesLoaded processes text messages load completion.
func (m Model) handleTextMessagesLoaded(msg textMessagesLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.textRequestID {
		return m, nil
	}
	active := m.finishModePresentation(modeTexts, msg.presentationGeneration)
	if !active {
		return m, nil
	}
	if msg.err != nil {
		m.err = msg.err
		m.modal = modalError
		m.modalResult = msg.err.Error()
		return m, nil
	}
	m.textState.messages = msg.messages
	m.textState.globalSearchTimeline = false
	if m.textState.filter.SearchQuery == "" {
		m.textState.unfilteredMessages = nil
	}
	m.textState.cursor = 0
	m.textState.scrollOffset = 0
	return m, nil
}

// handleTextSearchResult processes text search results.
func (m Model) handleTextSearchResult(msg textSearchResultMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.textRequestID {
		return m, nil
	}
	active := m.finishModePresentation(modeTexts, msg.presentationGeneration)
	if !active {
		return m, nil
	}
	if msg.err != nil {
		m.err = msg.err
		m.modal = modalError
		m.modalResult = msg.err.Error()
		return m, nil
	}
	// Show search results as a timeline
	m.textState.messages = msg.messages
	m.textState.level = textLevelTimeline
	m.textState.globalSearchTimeline = true
	m.textState.unfilteredMessages = nil
	m.textState.cursor = 0
	m.textState.scrollOffset = 0
	return m, nil
}

// handleTextStatsLoaded processes text stats load completion.
func (m Model) handleTextStatsLoaded(msg textStatsLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.err == nil {
		m.textState.stats = msg.stats
	}
	return m, nil
}

// handleWindowSize processes window resize events.
func (m Model) handleWindowSize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.transitionBuffer = "" // Clear frozen view on resize to re-render with new dimensions
	m.width = msg.Width
	m.height = msg.Height
	// Clamp dimensions to prevent panics from strings.Repeat with negative count
	if m.width < 0 {
		m.width = 0
	}
	if m.height < 0 {
		m.height = 0
	}
	m.pageSize = max(m.height-headerFooterLines, 1)
	if m.messageDetail != nil {
		m.reflowMessageDetail()
	}
	for mode := range modeCount {
		if m.parkedMessageReaders[mode].messageDetail != nil {
			m.parkedMessageReaders[mode] = m.reflowMessageReader(
				m.parkedMessageReaders[mode],
			)
		}
	}
	if m.meetingState.detail != nil {
		m.reflowMeetingDetail()
	}
	if m.peopleState.parkedMeetingState.detail != nil {
		m.peopleState.parkedMeetingState = m.reflowMeetingReader(
			m.peopleState.parkedMeetingState,
		)
	}
	return m, nil
}

// handleDataLoaded processes aggregate data load completion.
func (m Model) handleDataLoaded(msg dataLoadedMsg) (tea.Model, tea.Cmd) {
	// Ignore stale responses from previous loads
	if msg.requestID != m.aggregateRequestID {
		return m, nil
	}
	active := m.finishModePresentation(modeEmail, msg.presentationGeneration)
	if active {
		m.inlineSearchLoading = false
	}
	if msg.err != nil {
		if active {
			m.err = query.HintRepairEncoding(msg.err)
			m.modal = modalError
			m.modalResult = m.err.Error()
		}
		m.restorePosition = false // Clear flag on error to prevent stale state
		return m, nil
	}

	if active {
		m.err = nil
		if m.modal == modalError {
			m.modal = modalNone
		}
	}
	m.rows = msg.rows
	// Only reset position on fresh loads, not when restoring from breadcrumb
	if !m.restorePosition {
		m.cursor = 0
		m.scrollOffset = 0
	}
	m.restorePosition = false // Clear flag after use

	// When search filter is active, use distinct message stats from the
	// filtered stats query. This avoids inflated totals from 1:N views
	// (Recipients, Labels) where summing row.Count overcounts.
	if m.searchQuery != "" && msg.filteredStats != nil {
		m.contextStats = msg.filteredStats
	} else if m.searchQuery != "" {
		// Fallback if stats query failed: sum row counts
		m.contextStats = m.sumRowStats(msg.rows)
	} else if m.level == levelAggregates {
		// Clear contextStats when no search filter at top level
		m.contextStats = nil
	}
	return m, nil
}

// sumRowStats computes total stats by summing aggregate row counts.
func (m Model) sumRowStats(rows []query.AggregateRow) *query.TotalStats {
	var totalCount, totalSize, totalAttachments int64
	for _, row := range rows {
		totalCount += row.Count
		totalSize += row.TotalSize
		totalAttachments += row.AttachmentCount
	}
	return &query.TotalStats{
		MessageCount:    totalCount,
		TotalSize:       totalSize,
		AttachmentCount: totalAttachments,
	}
}

// handleStatsLoaded processes stats load completion.
func (m Model) handleStatsLoaded(msg statsLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.aggregateRequestID || msg.presentationGeneration != m.presentationGeneration {
		return m, nil
	}
	if msg.err == nil {
		m.stats = msg.stats
	}
	return m, nil
}

// handleAccountsLoaded processes accounts load completion.
func (m Model) handleAccountsLoaded(msg accountsLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.err == nil {
		m.accounts = msg.accounts
	}
	return m, nil
}

func (m Model) handleCollectionScopesLoaded(msg collectionScopesLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.err == nil {
		m.collectionScopes = msg.scopes
	}
	return m, nil
}

// handleUpdateCheck processes update check completion.
func (m Model) handleUpdateCheck(msg updateCheckMsg) (tea.Model, tea.Cmd) {
	m.updateAvailable = msg.version
	m.updateIsDevBuild = msg.isDevBuild
	return m, nil
}

// handleMessagesLoaded processes message list load completion.
func (m Model) handleMessagesLoaded(msg messagesLoadedMsg) (tea.Model, tea.Cmd) {
	// Ignore stale responses from previous loads
	if msg.requestID != m.loadRequestID {
		return m, nil
	}
	active := m.finishModePresentation(modeEmail, msg.presentationGeneration)
	if active {
		m.inlineSearchLoading = false
	}
	m.msgListLoadingMore = false
	if msg.err != nil {
		if active {
			m.err = query.HintRepairEncoding(msg.err)
			m.modal = modalError
			m.modalResult = m.err.Error()
		}
		m.restorePosition = false // Clear flag on error to prevent stale state
	} else {
		if active {
			m.err = nil
			if m.modal == modalError {
				m.modal = modalNone
			}
		}
		if msg.append {
			// Append paginated results to existing list
			m.messages = append(m.messages, msg.messages...)
			// Mark complete if append returned no new messages (end of data)
			if len(msg.messages) == 0 {
				m.msgListComplete = true
			}
		} else {
			m.messages = msg.messages
			m.msgListComplete = false // Reset on fresh load
			// Only reset position on fresh loads, not when restoring from breadcrumb
			if !m.restorePosition {
				m.cursor = 0
				m.scrollOffset = 0
			}
		}
		m.restorePosition = false // Clear flag after use
		// Update pagination offset
		m.msgListOffset = len(m.messages)
	}
	return m, nil
}

// handleMessageDetailLoaded processes message detail load completion.
func (m Model) handleMessageDetailLoaded(msg messageDetailLoadedMsg) (tea.Model, tea.Cmd) {
	// Ignore stale responses from previous loads
	if msg.requestID != m.detailRequestID {
		return m, nil
	}
	active := m.finishModePresentation(modeEmail, msg.presentationGeneration)
	if msg.err != nil {
		if active {
			m.err = query.HintRepairEncoding(msg.err)
			m.modal = modalError
			m.modalResult = m.err.Error()
		}
	} else {
		if active {
			m.err = nil
			if m.modal == modalError {
				m.modal = modalNone
			}
		}
		if m.mode == modeEmail {
			m.messageDetail = msg.detail
			m.detailScroll = 0
			m.pendingDetailSubject = ""
			m.reflowMessageDetail()
		} else {
			reader := m.parkedMessageReaders[modeEmail]
			reader.messageDetail = msg.detail
			reader.detailScroll = 0
			reader.pendingDetailSubject = ""
			m.parkedMessageReaders[modeEmail] = m.reflowMessageReader(reader)
		}
	}
	return m, nil
}

// handleThreadMessagesLoaded processes thread messages load completion.
func (m Model) handleThreadMessagesLoaded(msg threadMessagesLoadedMsg) (tea.Model, tea.Cmd) {
	// Ignore stale responses from previous loads
	if msg.requestID != m.loadRequestID {
		return m, nil
	}
	active := m.finishModePresentation(modeEmail, msg.presentationGeneration)
	if msg.err != nil {
		if active {
			m.err = query.HintRepairEncoding(msg.err)
			m.modal = modalError
			m.modalResult = m.err.Error()
		}
	} else {
		if active {
			m.err = nil
		}
		m.threadMessages = msg.messages
		m.threadConversationID = msg.conversationID
		m.threadTruncated = msg.truncated
		// Reset cursor/scroll for thread view
		m.threadCursor = 0
		m.threadScrollOffset = 0
	}
	return m, nil
}

// handleSearchResults processes search results load completion.
func (m Model) handleSearchResults(msg searchResultsMsg) (tea.Model, tea.Cmd) {
	// Ignore stale responses from previous searches
	if msg.requestID != m.searchRequestID {
		return m, nil
	}
	active := m.finishModePresentation(modeEmail, msg.presentationGeneration)
	if active {
		m.inlineSearchLoading = false
		m.searchLoadingMore = false
	}
	if msg.err != nil {
		if active {
			m.err = query.HintRepairEncoding(msg.err)
			m.modal = modalError
			m.modalResult = m.err.Error()
		}
		return m, nil
	}

	if active {
		m.err = nil
		if m.modal == modalError {
			m.modal = modalNone
		}
	}
	if msg.append {
		m.appendSearchResults(msg)
	} else {
		m.replaceSearchResults(msg)
	}
	return m, nil
}

// appendSearchResults appends paginated search results to existing results.
func (m *Model) appendSearchResults(msg searchResultsMsg) {
	m.messages = append(m.messages, msg.messages...)
	m.searchOffset += len(msg.messages)
	if msg.totalCount >= 0 {
		m.searchTotalCount = msg.totalCount
	}
	// Update contextStats when total is unknown so header reflects loaded count
	if m.searchTotalCount == -1 && m.contextStats != nil {
		m.contextStats.MessageCount = int64(len(m.messages))
	}
}

// replaceSearchResults replaces the current results with new search results.
func (m *Model) replaceSearchResults(msg searchResultsMsg) {
	m.messages = msg.messages
	m.searchOffset = len(msg.messages)
	m.searchTotalCount = msg.totalCount
	m.cursor = 0
	m.scrollOffset = 0

	// Set contextStats for search results to update header metrics.
	// When fresh stats are provided (msg.stats != nil), always use them —
	// they reflect the current search accurately. Only fall back to
	// preserving existing drill-down stats when no fresh stats are available
	// (e.g. deep/FTS search which doesn't compute aggregate stats).
	switch {
	case msg.totalCount > 0:
		if msg.stats != nil {
			// Use fresh aggregate stats from SearchFastWithStats
			m.contextStats = msg.stats
		} else if m.contextStats != nil && (m.contextStats.TotalSize > 0 || m.contextStats.AttachmentCount > 0) {
			// No fresh stats — preserve drill-down stats, only update MessageCount
			m.contextStats.MessageCount = msg.totalCount
		} else {
			m.contextStats = &query.TotalStats{MessageCount: msg.totalCount}
		}
	case msg.totalCount == -1:
		// Unknown total, use loaded count
		if msg.stats != nil {
			m.contextStats = msg.stats
			m.contextStats.MessageCount = int64(len(msg.messages))
		} else if m.contextStats != nil && (m.contextStats.TotalSize > 0 || m.contextStats.AttachmentCount > 0) {
			m.contextStats.MessageCount = int64(len(msg.messages))
		} else {
			m.contextStats = &query.TotalStats{MessageCount: int64(len(msg.messages))}
		}
	default:
		// Zero results: clear stale contextStats from previous view
		m.contextStats = &query.TotalStats{MessageCount: 0}
	}
	// Transition to message list view showing search results
	m.level = levelMessageList
}

// handleFlashClear processes flash message clear timeout.
func (m Model) handleFlashClear() (tea.Model, tea.Cmd) {
	// Clear flash message if it hasn't been updated since the timer started
	if time.Now().After(m.flashExpiresAt) || m.flashExpiresAt.IsZero() {
		m.flashMessage = ""
	}
	return m, nil
}

// handleExportResult processes attachment export completion.
func (m Model) handleExportResult(msg exportResultMsg) (tea.Model, tea.Cmd) {
	m.loading = false
	m.modal = modalExportResult
	m.modalResultTitle = msg.Title
	if msg.Err != nil && msg.Result == "" {
		m.modalResult = msg.Err.Error()
	} else {
		m.modalResult = msg.Result
	}
	return m, nil
}

// handleSearchDebounce processes debounced inline search triggers.
func (m Model) handleSearchDebounce(msg searchDebounceMsg) (tea.Model, tea.Cmd) {
	// Ignore stale debounce timers (user typed more since timer started)
	if msg.debounceID != m.inlineSearchDebounce {
		return m, nil
	}
	// Execute inline search for live updates
	if !m.inlineSearchActive {
		return m, nil
	}

	if err := m.searchInputValidationError(msg.query); err != nil {
		m.invalidateInlineSearchRequests()
		m.inlineSearchLoading = false
		m.inlineSearchError = err.Error()
		return m, nil
	}

	m.inlineSearchError = ""
	m.searchQuery = msg.query
	if m.searchQuery == "" {
		m.contextStats = nil
	}
	m.inlineSearchLoading = true
	spinCmd := m.startSpinner()

	if m.level == levelMessageList {
		// Message list: use search engine for live results
		m.syncSearchScope()
		m.invalidateInlineSearchRequests()
		if msg.query == "" {
			// Empty query: reload unfiltered messages
			return m, tea.Batch(spinCmd, m.loadMessages())
		}
		m.prepareSearchReplacement()
		return m, tea.Batch(spinCmd, m.loadSearch(msg.query))
	}
	// Aggregate views: reload aggregates with search filter
	m.aggregateRequestID++
	return m, tea.Batch(spinCmd, m.loadData())
}

// handleSpinnerTick processes spinner animation ticks.
func (m Model) handleSpinnerTick() (tea.Model, tea.Cmd) {
	// Only advance if still loading (any loading state)
	if m.isLoading() {
		m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
		return m, spinnerTick()
	}
	m.spinnerActive = false
	return m, nil
}

// handleKeyPress processes keyboard input.
func (m Model) handleKeyPress(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.settings.active {
		return m.handleSettingsKeyPress(msg)
	}
	if msg.String() == "," && m.settingsShortcutAvailable() {
		return m.openSettings()
	}
	if m.mode == modePeople {
		return m.handlePeopleKeyPress(msg)
	}
	// Route to Texts mode handler when active
	if m.mode == modeTexts {
		return m.handleTextKeyPress(msg)
	}
	if m.mode == modeMeetings {
		return m.handleMeetingKeyPress(msg)
	}

	// Handle modal first (error modals must dismiss even during search)
	if m.modal != modalNone {
		return m.handleModalKeys(msg)
	}

	// Resolving every deletion match is view-independent. Keep the current
	// scope stable until it finishes, while still allowing cancellation and
	// the normal quit flow from aggregate and message-list views alike.
	if m.deletionLoading {
		switch msg.String() {
		case keyNameEsc:
			m.cancelDeletionResolution()
			return m.showFlash("Deletion match resolution canceled")
		case "q", keyNameCtrlC:
			m.cancelDeletionResolution()
		default:
			return m, nil
		}
	}

	// Handle inline search (takes priority over view)
	if m.inlineSearchActive {
		return m.handleInlineSearchKeys(msg)
	}

	// Handle based on current view level
	switch m.level {
	case levelAggregates, levelDrillDown:
		return m.handleAggregateKeys(msg)
	case levelMessageList:
		return m.handleMessageListKeys(msg)
	case levelMessageDetail:
		return m.handleMessageDetailKeys(msg)
	case levelThreadView:
		return m.handleThreadViewKeys(msg)
	}
	return m, nil
}

// ensureThreadCursorVisible adjusts scroll offset to keep cursor visible in thread view.

// navigateDetailPrev navigates to the previous message in list/thread order.
// Left arrow moves towards the first item (lower index).

// navigateDetailNext navigates to the next message in list/thread order.
// Right arrow moves towards the last item (higher index).

// showFlash displays a temporary flash message.
func (m Model) showFlash(message string) (tea.Model, tea.Cmd) {
	m.flashMessage = message
	m.flashExpiresAt = time.Now().Add(flashDuration)
	return m, tea.Tick(flashDuration, func(t time.Time) tea.Msg {
		return flashClearMsg{}
	})
}

// detailPageSize returns the page size for detail view (2 more than table views).
func (m *Model) detailPageSize() int {
	return m.pageSize + 2
}

// clampDetailScroll ensures detailScroll stays within valid bounds.
func (m *Model) clampDetailScroll() {
	maxScroll := max(m.detailLineCount-m.detailPageSize(), 0)
	if m.detailScroll > maxScroll {
		m.detailScroll = maxScroll
	}
}

func (m *Model) reflowMessageDetail() {
	m.updateDetailLineCount()
	if m.detailSearchQuery != "" {
		m.findDetailMatches()
		if m.detailSearchMatchIndex >= len(m.detailSearchMatches) {
			if len(m.detailSearchMatches) > 0 {
				m.detailSearchMatchIndex = len(m.detailSearchMatches) - 1
			} else {
				m.detailSearchMatchIndex = 0
			}
		}
	}
	m.clampDetailScroll()
}

func (m Model) reflowMessageReader(reader messageReaderState) messageReaderState {
	m.messageReaderState = reader
	m.reflowMessageDetail()
	return m.messageReaderState
}

func (m *Model) reflowMeetingDetail() {
	matchIndex := m.meetingState.detailSearchMatchIndex
	if m.meetingState.detailSearchQuery != "" {
		m.findMeetingDetailMatches()
		if len(m.meetingState.detailSearchMatches) > 0 {
			m.meetingState.detailSearchMatchIndex = min(
				matchIndex, len(m.meetingState.detailSearchMatches)-1,
			)
		}
	}
	m.clampMeetingDetailScroll()
}

func (m Model) reflowMeetingReader(reader meetingState) meetingState {
	m.meetingState = reader
	m.reflowMeetingDetail()
	return m.meetingState
}

// findDetailMatches finds all lines matching the detail search query.
func (m *Model) findDetailMatches() {
	m.detailSearchMatches = nil
	if m.detailSearchQuery == "" || m.messageDetail == nil {
		return
	}
	lines := m.buildDetailLines()
	query := strings.ToLower(m.detailSearchQuery)
	for i, line := range lines {
		if strings.Contains(strings.ToLower(line), query) {
			m.detailSearchMatches = append(m.detailSearchMatches, i)
		}
	}
}

// scrollToDetailMatch scrolls the detail view to show the current match.
func (m *Model) scrollToDetailMatch() {
	if len(m.detailSearchMatches) == 0 || m.detailSearchMatchIndex >= len(m.detailSearchMatches) {
		return
	}
	targetLine := m.detailSearchMatches[m.detailSearchMatchIndex]
	pageSize := m.detailPageSize()
	// Center the match in the viewport
	m.detailScroll = max(targetLine-pageSize/2, 0)
	m.clampDetailScroll()
}

// updateDetailLineCount recalculates the line count for scroll bounds.
// This is called when message detail is loaded or window is resized.
func (m *Model) updateDetailLineCount() {
	if m.messageDetail == nil {
		m.detailLineCount = 0
		return
	}
	lines := m.buildDetailLines()
	m.detailLineCount = len(lines)
}

// goBack returns to the previous view level.

// handleModalKeys handles keys when a modal is displayed.

// stageForDeletion prepares messages for deletion via the ActionController.
func (m Model) stageForDeletion() (tea.Model, tea.Cmd) {
	if m.currentSourceScope().isEmpty() {
		m.modal = modalDeleteResult
		m.modalResult = "no messages selected"
		return m, nil
	}
	return m.stageForDeletionContext(false)
}

func (m Model) stageAllMatchesForDeletion() (tea.Model, tea.Cmd) {
	return m.stageAllMatchesForDeletionContext(m.deletionContext(true))
}

func (m Model) stageAllMatchesForDeletionContext(ctx DeletionContext) (tea.Model, tea.Cmd) {
	if m.deletionLoading {
		return m, nil
	}

	m.deletionRequestID++
	requestID := m.deletionRequestID
	m.deletionLoading = true
	resolveContext, cancel := context.WithCancel(context.Background())
	m.deletionCancel = cancel
	spinCmd := m.startSpinner()
	resolveCmd := safeCmdWithPanic(
		func() tea.Msg {
			manifest, err := m.actions.StageForDeletionContext(resolveContext, ctx)
			return deletionPreparedMsg{manifest: manifest, err: err, requestID: requestID}
		},
		func(r any) tea.Msg {
			return deletionPreparedMsg{
				err: fmt.Errorf("bulk deletion target resolution panic: %v", r), requestID: requestID,
			}
		},
	)
	return m, tea.Batch(spinCmd, resolveCmd)
}

func (m Model) stageForDeletionContext(allMatches bool) (tea.Model, tea.Cmd) {
	manifest, err := m.actions.StageForDeletion(m.deletionContext(allMatches))
	return m.applyPreparedDeletion(manifest, err)
}

func (m Model) deletionContext(allMatches bool) DeletionContext {
	var drillFilter *query.MessageFilter
	if m.hasDrillFilter() {
		f := m.drillFilter
		drillFilter = &f
	}
	dctx := DeletionContext{
		AggregateSelection: m.selection.aggregateKeys,
		MessageSelection:   m.selection.messageIDs,
		AggregateViewType:  m.selection.aggregateViewType,
		AccountFilter:      m.accountFilter,
		SourceIDs:          copySourceIDs(m.currentSourceScope().sourceIDs),
		Accounts:           m.accounts,
		TimeGranularity:    m.timeGranularity,
		Messages:           m.messages,
		DrillFilter:        drillFilter,
		AllMatches:         allMatches,
		SearchQuery:        m.searchQuery,
		SearchMode:         m.searchMode,
		MatchFilter:        m.currentSearchFilter(),
	}
	if allMatches {
		// Space selections are a different command scope. Keep them out of both
		// resolution and the manifest audit record for uppercase D.
		dctx.AggregateSelection = nil
		dctx.MessageSelection = nil
	}
	return dctx
}

func (m Model) handleDeletionPrepared(msg deletionPreparedMsg) (tea.Model, tea.Cmd) {
	if msg.requestID != m.deletionRequestID {
		return m, nil
	}
	m.finishDeletionResolution()
	return m.applyPreparedDeletion(msg.manifest, msg.err)
}

func (m *Model) finishDeletionResolution() {
	if m.deletionCancel != nil {
		m.deletionCancel()
		m.deletionCancel = nil
	}
	m.deletionLoading = false
}

func (m *Model) cancelDeletionResolution() {
	if !m.deletionLoading {
		return
	}
	m.deletionRequestID++
	m.finishDeletionResolution()
}

func (m Model) applyPreparedDeletion(manifest *deletion.Manifest, err error) (tea.Model, tea.Cmd) {
	if err != nil {
		m.modal = modalDeleteResult
		m.modalResult = err.Error()
		return m, nil
	}
	m.pendingManifest = manifest
	m.modal = modalDeleteConfirm
	return m, nil
}

// confirmDeletion saves the manifest and shows result.
func (m Model) confirmDeletion() (tea.Model, tea.Cmd) {
	if m.pendingManifest == nil {
		m.modal = modalNone
		return m, nil
	}

	// Save manifest via ActionController
	if err := m.actions.SaveManifest(m.pendingManifest); err != nil {
		m.modal = modalDeleteResult
		m.modalResult = fmt.Sprintf("Error: %v", err)
		m.pendingManifest = nil
		return m, nil
	}

	// Show success
	m.modal = modalDeleteResult
	m.modalResult = fmt.Sprintf("Staged %d messages for deletion.\nBatch ID: %s\nInspect: msgvault delete-staged --list\nDurable execution consent in invoking CLI config: [deletion] remote_enabled = true\nExecute: msgvault delete-staged\nOne-command alternative: MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged",
		len(m.pendingManifest.GmailIDs), m.pendingManifest.ID)

	// Clear selection
	m.selection.aggregateKeys = make(map[string]bool)
	m.selection.messageIDs = make(map[int64]bool)
	m.pendingManifest = nil

	return m, nil
}

// hasSelection returns true if any items are selected.
func (m Model) hasSelection() bool {
	return len(m.selection.aggregateKeys) > 0 || len(m.selection.messageIDs) > 0
}

// selectionCount returns the number of selected items.
func (m Model) selectionCount() int {
	return len(m.selection.aggregateKeys) + len(m.selection.messageIDs)
}

// ensureCursorVisible adjusts scroll offset to keep cursor in view.

// pushBreadcrumb saves the current view state to the navigation history.

// navigateList handles common list navigation keys (up, down, pgup, pgdown, home, end).
// Returns true if the key was handled.

// openAccountSelector opens the account selector modal with cursor at current selection.

// openFilterModal opens the filter toggle modal.

// activateInlineSearch activates the inline search bar with fast search mode.

// toggleAggregateSelection toggles selection for the current aggregate row.

// selectVisibleAggregates selects all visible aggregate rows.

// clearAllSelections clears both aggregate and message selections.

// View implements tea.Model.
func (m Model) View() tea.View {
	var content string
	if m.quitting {
		content = ""
	} else if m.width == 0 {
		content = "Loading..."
	} else if m.settings.active {
		// Settings is a shell layer above content transitions. Keep the frozen
		// content frame intact underneath so Esc restores it exactly.
		content = m.renderSettingsView()
	} else if m.transitionBuffer != "" {
		// If view is frozen (during level transitions), return the cached view
		// to prevent flashing while async data loads complete.
		content = m.transitionBuffer
	} else {
		content = m.renderView()
	}

	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

// renderView renders the current view based on the active level.
// Separated from View() so transitions can capture the current output
// before changing state (for the transitionBuffer pattern).
func (m Model) renderView() string {
	if m.settings.active {
		return m.renderSettingsView()
	}
	if m.mode == modePeople {
		return m.renderPeopleView()
	}
	if m.mode == modeTexts {
		return m.renderTextView()
	}
	if m.mode == modeMeetings {
		return m.renderMeetingView()
	}

	switch m.level {
	case levelAggregates, levelDrillDown:
		return fmt.Sprintf("%s\n%s\n%s",
			m.headerView(),
			m.aggregateTableView(),
			m.footerView(),
		)
	case levelMessageList:
		return fmt.Sprintf("%s\n%s\n%s",
			m.headerView(),
			m.messageListView(),
			m.footerView(),
		)
	case levelMessageDetail:
		return fmt.Sprintf("%s\n%s\n%s",
			m.headerView(),
			m.messageDetailView(),
			m.footerView(),
		)
	case levelThreadView:
		return fmt.Sprintf("%s\n%s\n%s",
			m.headerView(),
			m.threadView(),
			m.footerView(),
		)
	}

	return ""
}

// exportAttachments exports selected attachments to a zip file asynchronously.
func (m Model) exportAttachments() (tea.Model, tea.Cmd) {
	if m.messageDetail == nil || len(m.messageDetail.Attachments) == 0 {
		m.modal = modalNone
		return m.showFlash("No attachments to export")
	}

	cmd := m.actions.ExportAttachments(m.messageDetail, m.exportSelection)
	if cmd == nil {
		return m.showFlash("No attachments selected")
	}

	m.modal = modalNone
	m.loading = true
	m.exportSelection = nil
	return m, cmd
}
