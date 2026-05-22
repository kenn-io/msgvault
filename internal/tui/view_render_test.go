package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"go.kenn.io/msgvault/internal/query"
)

// TestFooterPositionDisplay verifies footer position indicator in message list view.
func TestFooterPositionDisplay(t *testing.T) {
	tests := []struct {
		name         string
		msgCount     int
		cursor       int
		contextStats *query.TotalStats
		globalStats  *query.TotalStats
		allMessages  bool
		wantContains []string
		wantMissing  []string
	}{
		{
			name:         "shows cursor/total",
			msgCount:     100,
			cursor:       49,
			wantContains: []string{"50/100"},
		},
		{
			name:         "shows N of M when total > loaded",
			msgCount:     100,
			cursor:       49,
			contextStats: &query.TotalStats{MessageCount: 500},
			wantContains: []string{"50 of 500"},
			wantMissing:  []string{"50/100"},
		},
		{
			name:         "shows N/M when all loaded",
			msgCount:     50,
			cursor:       24,
			contextStats: &query.TotalStats{MessageCount: 50},
			wantContains: []string{"25/50"},
		},
		{
			name:         "falls back to loaded count when no context stats",
			msgCount:     75,
			cursor:       49,
			wantContains: []string{"50/75"},
			wantMissing:  []string{" of "},
		},
		{
			name:         "uses loaded count when context stats smaller",
			msgCount:     100,
			cursor:       49,
			contextStats: &query.TotalStats{MessageCount: 50},
			wantContains: []string{"50/100"},
		},
		{
			name:         "uses global stats for all messages view",
			msgCount:     500,
			cursor:       99,
			globalStats:  &query.TotalStats{MessageCount: 175000},
			allMessages:  true,
			wantContains: []string{"100 of 175000"},
			wantMissing:  []string{"/500"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := NewBuilder().WithMessages(makeMessages(tt.msgCount)...).
				WithPageSize(20).WithSize(100, 30).
				WithLevel(levelMessageList)

			if tt.contextStats != nil {
				builder = builder.WithContextStats(tt.contextStats)
			}
			if tt.globalStats != nil {
				builder = builder.WithStats(tt.globalStats)
			}

			model := builder.Build()
			model.cursor = tt.cursor
			model.allMessages = tt.allMessages

			footer := model.footerView()
			for _, s := range tt.wantContains {
				if !strings.Contains(footer, s) {
					t.Errorf("footer missing %q, got: %q", s, footer)
				}
			}
			for _, s := range tt.wantMissing {
				if strings.Contains(footer, s) {
					t.Errorf("footer should not contain %q, got: %q", s, footer)
				}
			}
		})
	}
}

// TestTabCyclesViewTypeAtAggregates verifies Tab still cycles view types.

// TestHeaderContextStats verifies header shows contextual stats when drilled down.
func TestHeaderContextStats(t *testing.T) {
	globalStats := &query.TotalStats{MessageCount: 10000, TotalSize: 50000000, AttachmentCount: 500}

	tests := []struct {
		name         string
		width        int
		contextStats *query.TotalStats
		wantContains []string
		wantMissing  []string
	}{
		{
			name:         "shows context stats not global",
			width:        100,
			contextStats: &query.TotalStats{MessageCount: 100, TotalSize: 500000},
			wantContains: []string{"100 msgs"},
			wantMissing:  []string{"10000 msgs"},
		},
		{
			name:         "shows attachment count",
			width:        120,
			contextStats: &query.TotalStats{MessageCount: 100, TotalSize: 500000, AttachmentCount: 42},
			wantContains: []string{"42 attchs"},
		},
		{
			name:         "shows zero attachment count",
			width:        120,
			contextStats: &query.TotalStats{MessageCount: 100, TotalSize: 500000, AttachmentCount: 0},
			wantContains: []string{"0 attchs"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewBuilder().WithSize(tt.width, 20).WithLevel(levelMessageList).
				WithStats(globalStats).
				WithContextStats(tt.contextStats).
				Build()

			header := model.headerView()
			for _, s := range tt.wantContains {
				if !strings.Contains(header, s) {
					t.Errorf("header missing %q, got: %q", s, header)
				}
			}
			for _, s := range tt.wantMissing {
				if strings.Contains(header, s) {
					t.Errorf("header should not contain %q", s)
				}
			}
		})
	}
}

// TestHelpModalOpensWithQuestionMark verifies '?' opens the help modal.
func TestHelpModalOpensWithQuestionMark(t *testing.T) {
	model := NewBuilder().Build()

	// Press '?'
	newModel, _ := model.Update(key('?'))
	m := newModel.(Model)

	if m.modal != modalHelp {
		t.Errorf("expected modalHelp after '?', got %v", m.modal)
	}
}

// TestHelpModalClosesOnAnyKey verifies help modal closes on any key.
func TestHelpModalClosesOnAnyKey(t *testing.T) {
	model := NewBuilder().WithModal(modalHelp).Build()

	// Press any key (e.g., Enter)
	newModel, _ := model.Update(keyEnter())
	m := newModel.(Model)

	if m.modal != modalNone {
		t.Errorf("expected modalNone after pressing key in help, got %v", m.modal)
	}
}

// TestVKeyReversesSortOrder verifies 'v' reverses sort direction.
func TestVKeyReversesSortOrder(t *testing.T) {
	model := NewBuilder().WithRows(query.AggregateRow{Key: "test", Count: 1}).Build()
	model.sortDirection = query.SortDesc

	// Press 'v'
	newModel, _ := model.Update(key('v'))
	m := newModel.(Model)

	if m.sortDirection != query.SortAsc {
		t.Errorf("expected SortAsc after 'v', got %v", m.sortDirection)
	}

	// Press 'v' again
	newModel2, _ := m.Update(key('v'))
	m2 := newModel2.(Model)

	if m2.sortDirection != query.SortDesc {
		t.Errorf("expected SortDesc after second 'v', got %v", m2.sortDirection)
	}
}

// TestSearchSetsContextStats verifies search results set contextStats for header metrics.

// TestHighlightedColumnsAligned verifies that highlighting search terms in
// aggregate rows doesn't break column alignment.
func TestHighlightedColumnsAligned(t *testing.T) {
	rows := []query.AggregateRow{
		{Key: "alice@example.com", Count: 42, TotalSize: 1024000, AttachmentSize: 512},
		{Key: "bob@example.com", Count: 7, TotalSize: 2048, AttachmentSize: 0},
	}
	model := NewBuilder().WithRows(rows...).
		WithLevel(levelAggregates).WithSize(100, 24).Build()

	// Render without search
	noSearchOutput := model.aggregateTableView()
	noSearchLines := strings.Split(noSearchOutput, "\n")

	// Render with search highlighting "alice"
	model.searchQuery = "alice"
	highlightOutput := model.aggregateTableView()
	highlightLines := strings.Split(highlightOutput, "\n")

	// Compare visible widths — should be identical for each corresponding line
	for i := 0; i < len(noSearchLines) && i < len(highlightLines); i++ {
		noSearchWidth := lipgloss.Width(noSearchLines[i])
		highlightWidth := lipgloss.Width(highlightLines[i])
		if noSearchWidth != highlightWidth {
			t.Errorf("line %d: width without search=%d, with highlight=%d\n  no search: %q\n  highlight: %q",
				i, noSearchWidth, highlightWidth, noSearchLines[i], highlightLines[i])
		}
	}
}

// TestViewTypeRestoredAfterEscFromSubAggregate verifies viewType is restored when
// navigating back from sub-aggregate to message list.

// === Header View Tests ===

// TestHeaderShowsTitleBar verifies the title bar shows msgvault with version.
func TestHeaderShowsTitleBar(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		wantVersion bool   // should version appear in title
		wantText    string // expected version text in brackets
	}{
		{"tagged version", "v0.1.0", true, "[v0.1.0]"},
		{"dev version hidden", "dev", false, ""},
		{"empty version hidden", "", false, ""},
		{"unknown version hidden", "unknown", false, ""},
		{"prerelease version", "v1.0.0-rc1", true, "[v1.0.0-rc1]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := NewBuilder().WithSize(100, 20).WithViewType(query.ViewSenders).Build()
			model.version = tt.version

			header := model.headerView()
			lines := strings.Split(header, "\n")

			if len(lines) < 2 {
				t.Fatalf("expected 2 header lines, got %d", len(lines))
			}

			if !strings.Contains(lines[0], "msgvault") {
				t.Errorf("expected title bar to contain 'msgvault', got: %s", lines[0])
			}
			if tt.wantVersion {
				if !strings.Contains(lines[0], tt.wantText) {
					t.Errorf("expected title bar to contain %q, got: %s", tt.wantText, lines[0])
				}
			} else {
				if strings.Contains(lines[0], "[") {
					t.Errorf("expected no version in title bar, got: %s", lines[0])
				}
			}
			if !strings.Contains(lines[0], "All Accounts") {
				t.Errorf("expected title bar to contain 'All Accounts', got: %s", lines[0])
			}
		})
	}
}

// TestHeaderDisplay consolidates header display tests into table-driven cases.
func TestHeaderDisplay(t *testing.T) {
	accountID := int64(2)
	tests := []struct {
		name         string
		setup        func() Model
		line         int
		wantContains []string
		wantMissing  []string
	}{
		{
			name: "shows selected account name",
			setup: func() Model {
				return NewBuilder().WithSize(100, 20).
					WithAccounts(
						query.AccountInfo{ID: 1, Identifier: "alice@gmail.com"},
						query.AccountInfo{ID: 2, Identifier: "bob@gmail.com"},
					).
					WithAccountFilter(&accountID).Build()
			},
			line:         0,
			wantContains: []string{"bob@gmail.com"},
		},
		{
			name: "shows view type on line 2",
			setup: func() Model {
				return NewBuilder().WithSize(100, 20).WithViewType(query.ViewSenders).
					WithStats(standardStats()).Build()
			},
			line:         1,
			wantContains: []string{"Sender", "1000 msgs"},
		},
		{
			name: "drill-down uses compact prefix",
			setup: func() Model {
				m := NewBuilder().WithSize(100, 20).
					WithLevel(levelMessageList).WithViewType(query.ViewRecipients).Build()
				m.drillViewType = query.ViewSenders
				m.drillFilter = query.MessageFilter{Sender: "alice@example.com"}
				m.filterKey = "alice@example.com"
				return m
			},
			line:         1,
			wantContains: []string{"S:"},
			wantMissing:  []string{"From:"},
		},
		{
			name: "sub-aggregate shows drill context",
			setup: func() Model {
				m := NewBuilder().WithSize(100, 20).
					WithLevel(levelDrillDown).WithViewType(query.ViewRecipients).
					WithContextStats(&query.TotalStats{MessageCount: 100, TotalSize: 500000}).Build()
				m.drillViewType = query.ViewSenders
				m.drillFilter = query.MessageFilter{Sender: "alice@example.com"}
				return m
			},
			line:         1,
			wantContains: []string{"S:", "alice@example.com", "(by Recipient)", "100 msgs"},
		},
		{
			name: "shows attachment filter indicator",
			setup: func() Model {
				m := NewBuilder().WithSize(100, 20).Build()
				m.filters.attachmentsOnly = true
				return m
			},
			line:         0,
			wantContains: []string{"[Attachments]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := tt.setup()
			header := model.headerView()
			lines := strings.Split(header, "\n")

			if len(lines) <= tt.line {
				t.Fatalf("header has only %d lines, need line %d", len(lines), tt.line)
			}

			line := lines[tt.line]
			for _, s := range tt.wantContains {
				if !strings.Contains(line, s) {
					t.Errorf("header line %d missing %q, got: %q", tt.line, s, line)
				}
			}
			for _, s := range tt.wantMissing {
				if strings.Contains(line, s) {
					t.Errorf("header line %d should not contain %q, got: %q", tt.line, s, line)
				}
			}
		})
	}
}

// TestViewFitsTerminalHeight verifies View() output fits exactly in terminal height
// when pageSize is calculated via WindowSizeMsg. This catches bugs where header
// line count changes but pageSize calculation isn't updated.
func TestViewFitsTerminalHeight(t *testing.T) {
	model := NewBuilder().
		WithRows(standardRows...).
		WithViewType(query.ViewSenders).
		WithStats(standardStats()).
		Build()

	// Simulate WindowSizeMsg to trigger pageSize calculation (the real code path)
	terminalHeight := 40
	model = resizeModel(t, model, 100, terminalHeight)

	view := model.View()
	lines := strings.Split(view, "\n")
	actualLines := countViewLines(view)

	t.Logf("Terminal height: %d, View lines: %d, pageSize: %d", terminalHeight, actualLines, model.pageSize)
	t.Logf("First line: %q", lines[0])
	t.Logf("Last non-empty line: %q", lines[actualLines-1])

	// View should fit exactly in terminal height
	assertViewFitsHeight(t, view, terminalHeight)

	// First line must be title bar
	if !strings.Contains(lines[0], "msgvault") {
		t.Errorf("First line should be title bar with 'msgvault', got: %q", lines[0])
	}
}

// TestViewFitsTerminalHeightDuringLoading verifies View() output fits during loading state.
func TestViewFitsTerminalHeightDuringLoading(t *testing.T) {
	model := NewBuilder().
		WithRows(standardRows...).
		WithViewType(query.ViewSenders).
		WithStats(standardStats()).
		WithLoading(true).
		Build()

	terminalHeight := 40
	model = resizeModel(t, model, 100, terminalHeight)

	view := model.View()
	lines := strings.Split(view, "\n")
	actualLines := countViewLines(view)

	t.Logf("Terminal height: %d, View lines: %d, pageSize: %d (loading=%v)", terminalHeight, actualLines, model.pageSize, model.loading)

	assertViewFitsHeight(t, view, terminalHeight)
	if !strings.Contains(lines[0], "msgvault") {
		t.Errorf("First line should be title bar, got: %q", lines[0])
	}
}

// TestViewFitsTerminalHeightWithInlineSearch verifies View() output fits with inline search active.
func TestViewFitsTerminalHeightWithInlineSearch(t *testing.T) {
	model := NewBuilder().
		WithRows(standardRows...).
		WithViewType(query.ViewSenders).
		WithStats(standardStats()).
		Build()
	model.inlineSearchActive = true // Enable inline search

	terminalHeight := 40
	model = resizeModel(t, model, 100, terminalHeight)

	view := model.View()
	lines := strings.Split(view, "\n")
	actualLines := countViewLines(view)

	t.Logf("Terminal height: %d, View lines: %d, pageSize: %d (inlineSearch=%v)", terminalHeight, actualLines, model.pageSize, model.inlineSearchActive)

	assertViewFitsHeight(t, view, terminalHeight)
	if !strings.Contains(lines[0], "msgvault") {
		t.Errorf("First line should be title bar, got: %q", lines[0])
	}
}

// TestViewFitsTerminalHeightAtMessageList verifies View() output fits at message list level.
func TestViewFitsTerminalHeightAtMessageList(t *testing.T) {
	msgs := []query.MessageSummary{
		{ID: 1, Subject: "Test 1", FromEmail: "alice@example.com", SizeEstimate: 1000},
		{ID: 2, Subject: "Test 2", FromEmail: "bob@example.com", SizeEstimate: 2000},
	}

	model := NewBuilder().
		WithMessages(msgs...).
		WithLevel(levelMessageList).
		WithStats(standardStats()).
		WithContextStats(&query.TotalStats{MessageCount: 2, TotalSize: 3000, AttachmentCount: 0}).
		Build()
	model.filterKey = "alice@example.com"

	terminalHeight := 40
	model = resizeModel(t, model, 100, terminalHeight)

	view := model.View()
	lines := strings.Split(view, "\n")
	actualLines := countViewLines(view)

	t.Logf("Terminal height: %d, View lines: %d, pageSize: %d (level=MessageList)", terminalHeight, actualLines, model.pageSize)

	assertViewFitsHeight(t, view, terminalHeight)
	if !strings.Contains(lines[0], "msgvault") {
		t.Errorf("First line should be title bar, got: %q", lines[0])
	}
}

// TestViewFitsTerminalHeightStartupSequence simulates the real startup sequence
// to verify line counts at each stage of initialization.
func TestViewFitsTerminalHeightStartupSequence(t *testing.T) {
	terminalHeight := 40
	terminalWidth := 100

	// Stage 1: Before WindowSizeMsg (width=0)
	model := NewBuilder().
		WithLoading(true).
		WithSize(0, 0).
		Build()

	view1 := model.View()
	t.Logf("Stage 1 (before resize): View = %q", view1)
	if view1 != "Loading..." {
		t.Errorf("Stage 1: Expected 'Loading...', got %q", view1)
	}

	// Stage 2: After WindowSizeMsg (width/height set, loading=true, no data)
	model = resizeModel(t, model, terminalWidth, terminalHeight)

	view2 := model.View()
	lines2 := strings.Split(view2, "\n")
	actualLines2 := countViewLines(view2)
	t.Logf("Stage 2 (after resize, loading=true, no data): lines=%d, pageSize=%d", actualLines2, model.pageSize)
	t.Logf("  First line: %q", truncateTestString(lines2[0], 60))
	t.Logf("  Last line: %q", truncateTestString(lines2[actualLines2-1], 60))

	if actualLines2 != terminalHeight {
		t.Errorf("Stage 2: View has %d lines but terminal height is %d (loading, no data)", actualLines2, terminalHeight)
	}

	// Stage 3: After stats load (still loading=true, no data)
	model.stats = standardStats()

	view3 := model.View()
	actualLines3 := countViewLines(view3)
	t.Logf("Stage 3 (stats loaded, loading=true): lines=%d", actualLines3)

	if actualLines3 != terminalHeight {
		t.Errorf("Stage 3: View has %d lines but terminal height is %d (stats loaded)", actualLines3, terminalHeight)
	}

	// Stage 4: After data loads (loading=false, rows populated)
	model.loading = false
	model.rows = []query.AggregateRow{
		{Key: "alice@example.com", Count: 100, TotalSize: 500000},
		{Key: "bob@example.com", Count: 50, TotalSize: 250000},
	}

	view4 := model.View()
	lines4 := strings.Split(view4, "\n")
	actualLines4 := countViewLines(view4)
	t.Logf("Stage 4 (data loaded): lines=%d", actualLines4)
	t.Logf("  First line: %q", truncateTestString(lines4[0], 60))

	if actualLines4 != terminalHeight {
		t.Errorf("Stage 4: View has %d lines but terminal height is %d (data loaded)", actualLines4, terminalHeight)
	}

	// Ensure first line is always title bar at stages 2-4
	for i, lines := range [][]string{lines2, strings.Split(view3, "\n"), lines4} {
		if !strings.Contains(lines[0], "msgvault") {
			t.Errorf("Stage %d: First line should contain 'msgvault', got: %q", i+2, lines[0])
		}
	}
}

// truncateTestString truncates a string for test output display.
func truncateTestString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// TestViewFitsTerminalHeightWithBadData verifies View() handles data with
// embedded newlines or other problematic characters without adding extra lines.
func TestViewFitsTerminalHeightWithBadData(t *testing.T) {
	// Data with embedded newlines and other special characters
	rows := []query.AggregateRow{
		{Key: "alice@example.com", Count: 100, TotalSize: 500000},
		{Key: "bob\n@example.com", Count: 50, TotalSize: 250000},       // Embedded newline!
		{Key: "charlie\r\n@example.com", Count: 25, TotalSize: 100000}, // CRLF
		{Key: "david\t@example.com", Count: 10, TotalSize: 50000},      // Tab
	}

	model := NewBuilder().
		WithRows(rows...).
		WithViewType(query.ViewSenders).
		WithStats(standardStats()).
		Build()

	terminalHeight := 40
	model = resizeModel(t, model, 100, terminalHeight)

	view := model.View()
	lines := strings.Split(view, "\n")
	actualLines := countViewLines(view)

	t.Logf("Terminal height: %d, View lines: %d (with bad data)", terminalHeight, actualLines)

	if actualLines > terminalHeight {
		t.Errorf("View has %d lines but terminal height is %d - bad data caused extra lines!", actualLines, terminalHeight)
		// Log the problematic lines for debugging
		for i, line := range lines {
			if i >= terminalHeight {
				t.Logf("  Extra line %d: %q", i, truncateTestString(line, 60))
			}
		}
	}
}

// TestViewFitsVariousTerminalSizes tests that View fits for common terminal sizes.
func TestViewFitsVariousTerminalSizes(t *testing.T) {
	sizes := []struct {
		width, height int
	}{
		{80, 24},  // Standard
		{100, 27}, // User's actual terminal
		{100, 30}, // Larger
		{100, 55}, // User's other terminal
		{120, 40}, // Wide
		{80, 10},  // Very short
		{200, 50}, // Very wide and tall
	}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			model := NewBuilder().
				WithRows(standardRows...).
				WithViewType(query.ViewSenders).
				WithStats(standardStats()).
				Build()

			model = resizeModel(t, model, size.width, size.height)

			view := model.View()
			lines := strings.Split(view, "\n")
			actualLines := countViewLines(view)

			if actualLines != size.height {
				t.Errorf("View has %d lines but terminal height is %d (pageSize=%d)", actualLines, size.height, model.pageSize)
			}

			// Check no line exceeds width
			for i, line := range lines {
				if lipgloss.Width(line) > size.width {
					t.Errorf("Line %d exceeds width: %d > %d", i, lipgloss.Width(line), size.width)
				}
			}
		})
	}
}

// TestViewDuringSpinnerAnimation verifies line count during spinner animation.
func TestViewDuringSpinnerAnimation(t *testing.T) {
	rows := []query.AggregateRow{
		{Key: "alice@example.com", Count: 100, TotalSize: 500000},
	}

	model := NewBuilder().
		WithRows(rows...).
		WithViewType(query.ViewSenders).
		WithStats(standardStats()).
		WithLoading(true).
		Build()

	terminalWidth := 100
	terminalHeight := 24
	model = resizeModel(t, model, terminalWidth, terminalHeight)

	// Simulate multiple spinner frames
	for frame := 0; frame < 10; frame++ {
		model.spinnerFrame = frame

		view := model.View()
		lines := strings.Split(view, "\n")
		actualLines := countViewLines(view)

		if actualLines != terminalHeight {
			t.Errorf("Frame %d: View has %d lines but terminal height is %d", frame, actualLines, terminalHeight)
		}

		// Check line widths
		for i, line := range lines {
			if lipgloss.Width(line) > terminalWidth {
				t.Errorf("Frame %d, Line %d exceeds width: %d > %d", frame, i, lipgloss.Width(line), terminalWidth)
			}
		}
	}
}

// TestHeaderLineFitsWidth verifies the header line2 doesn't exceed terminal width
// even when breadcrumb + stats are very long.
func TestHeaderLineFitsWidth(t *testing.T) {
	rows := []query.AggregateRow{
		{Key: "alice@example.com", Count: 100, TotalSize: 500000},
	}

	model := NewBuilder().
		WithRows(rows...).
		WithViewType(query.ViewSenders).
		// Very long stats string
		WithStats(&query.TotalStats{MessageCount: 999999999, TotalSize: 999999999999, AttachmentCount: 999999}).
		Build()

	terminalWidth := 80 // Narrower terminal
	terminalHeight := 40
	model = resizeModel(t, model, terminalWidth, terminalHeight)

	view := model.View()
	lines := strings.Split(view, "\n")
	actualLines := countViewLines(view)

	t.Logf("Terminal: %dx%d, View lines: %d", terminalWidth, terminalHeight, actualLines)

	assertViewFitsHeight(t, view, terminalHeight)

	// Check that no line exceeds terminal width
	for i, line := range lines[:min(5, len(lines))] {
		lineWidth := lipgloss.Width(line)
		if lineWidth > terminalWidth {
			t.Errorf("Line %d has width %d but terminal width is %d: %q", i, lineWidth, terminalWidth, truncateTestString(line, 60))
		}
	}
}

// TestFooterShowsTotalUniqueWhenAvailable verifies that the footer shows
// "N of M" format when TotalUnique is set and greater than loaded rows.
func TestFooterShowsTotalUniqueWhenAvailable(t *testing.T) {
	// Set up rows with TotalUnique set (simulating a query that returns more rows than loaded)
	rows := []query.AggregateRow{
		{Key: "alice@example.com", Count: 100, TotalSize: 500000, TotalUnique: 1000},
		{Key: "bob@example.com", Count: 50, TotalSize: 250000, TotalUnique: 1000},
	}

	model := NewBuilder().
		WithRows(rows...).
		WithViewType(query.ViewSenders).
		WithSize(100, 30).
		WithPageSize(20).
		Build()

	footer := model.footerView()

	// When TotalUnique is set and greater than loaded rows, should show "N of M"
	if !strings.Contains(footer, "1 of 1000") {
		t.Errorf("Footer should show '1 of 1000' when TotalUnique (1000) > loaded rows (2), got: %q", footer)
	}
}

// TestFooterShowsLoadedCountWhenNoTotalUnique verifies that the footer falls back
// to showing loaded count when TotalUnique is not set (zero value).
func TestFooterShowsLoadedCountWhenNoTotalUnique(t *testing.T) {
	// Set up rows without TotalUnique (zero value)
	model := NewBuilder().
		WithRows(standardRows...).
		WithViewType(query.ViewSenders).
		WithSize(100, 30).
		WithPageSize(20).
		Build()

	footer := model.footerView()

	// When TotalUnique is not set, should show loaded count format
	if !strings.Contains(footer, "1/2") {
		t.Errorf("Footer should show '1/2' when TotalUnique is not set, got: %q", footer)
	}
}

// TestViewTypePrefixFallback verifies viewTypePrefix handles all ViewType values.
func TestViewTypePrefixFallback(t *testing.T) {
	// Test known view types return expected prefixes
	tests := []struct {
		vt       query.ViewType
		expected string
	}{
		{query.ViewSenders, "S"},
		{query.ViewRecipients, "R"},
		{query.ViewRecipientNames, "RN"},
		{query.ViewDomains, "D"},
		{query.ViewLabels, "L"},
		{query.ViewTime, "T"},
	}

	for _, tc := range tests {
		got := viewTypePrefix(tc.vt)
		if got != tc.expected {
			t.Errorf("viewTypePrefix(%v) = %q, want %q", tc.vt, got, tc.expected)
		}
	}

	// Test unknown view type - should return first char of String()
	// Note: ViewType(999).String() returns "ViewType(999)" so we get "V"
	// The "?" fallback in viewTypePrefix is defensive code for the edge case
	// where String() returns empty, which doesn't happen with Go's stringer.
	unknown := query.ViewType(999)
	got := viewTypePrefix(unknown)
	expectedFirstChar := string(unknown.String()[0]) // "V" from "ViewType(999)"
	if got != expectedFirstChar {
		t.Errorf("viewTypePrefix(%v) = %q, want %q (first char of String())", unknown, got, expectedFirstChar)
	}
}

// TestDetailNavigationPrevNext verifies left/right arrow navigation in message detail view.
// Left = previous in list (lower index), Right = next in list (higher index).

// TestLayoutFitsTerminalHeight verifies views render correctly without blank lines
// or truncated footers at various terminal heights.
func TestLayoutFitsTerminalHeight(t *testing.T) {
	rows := []query.AggregateRow{
		{Key: "alice@example.com", Count: 5},
		{Key: "bob@example.com", Count: 3},
	}

	tests := []struct {
		name   string
		height int
		level  viewLevel
	}{
		{"aggregate_small", 10, levelAggregates},
		{"aggregate_normal", 24, levelAggregates},
		{"messagelist_small", 10, levelMessageList},
		{"messagelist_normal", 24, levelMessageList},
		{"detail_small", 10, levelMessageDetail},
		{"detail_normal", 24, levelMessageDetail},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := NewBuilder().WithRows(rows...).
				WithSize(100, tc.height).WithPageSize(tc.height - 5).
				WithLevel(tc.level).Build()

			// Set up messages for message list/detail views
			if tc.level == levelMessageList || tc.level == levelMessageDetail {
				model.messages = []query.MessageSummary{
					{ID: 1, Subject: "Test message"},
				}
			}
			if tc.level == levelMessageDetail {
				model.messageDetail = &query.MessageDetail{
					ID:       1,
					Subject:  "Test message",
					BodyText: "Test body content",
				}
				model.detailLineCount = 10
			}

			view := model.View()
			lines := strings.Split(view, "\n")

			// View should have exactly height lines (or height-1 if last line has no newline)
			if len(lines) < tc.height-1 || len(lines) > tc.height+1 {
				t.Errorf("expected ~%d lines, got %d", tc.height, len(lines))
			}

			// Footer should be present (contains navigation hints)
			// All views have navigation hints separated by │
			if !strings.Contains(view, "│") {
				lastLine := lines[len(lines)-1]
				t.Errorf("footer with navigation hints not found in view, last line: %q", lastLine)
			}

			// No excessive blank lines at the end
			blankCount := 0
			for i := len(lines) - 1; i >= 0 && strings.TrimSpace(lines[i]) == ""; i-- {
				blankCount++
			}
			if blankCount > 1 {
				t.Errorf("found %d trailing blank lines, expected at most 1", blankCount)
			}
		})
	}
}

// TestScrollClampingAfterResize verifies detailScroll is clamped when max changes.

// TestModalCompositingPreservesANSI verifies that modal overlay doesn't corrupt ANSI sequences.
// Note: This test mutates the global lipgloss color profile. Do not add t.Parallel().
func TestModalCompositingPreservesANSI(t *testing.T) {
	forceColorProfile(t)

	model := NewBuilder().
		WithRows(
			query.AggregateRow{Key: "alice@example.com", Count: 100, TotalSize: 1000000},
			query.AggregateRow{Key: "bob@example.com", Count: 50, TotalSize: 500000},
			query.AggregateRow{Key: "charlie@example.com", Count: 25, TotalSize: 250000},
		).
		WithSize(80, 24).WithPageSize(19).
		WithModal(modalQuitConfirm).Build()

	// Render the view with quit modal - this uses overlayModal
	view := model.View()

	// Basic sanity checks
	if len(view) == 0 {
		t.Fatal("View rendered empty output")
	}

	// The view should contain modal content
	if !strings.Contains(view, "Quit") && !strings.Contains(view, "quit") {
		t.Errorf("Modal content not found in view, view length: %d", len(view))
	}

	// Validate ANSI sequences using regex
	// Valid SGR sequences: ESC [ (optional params: digits and semicolons) m
	// Valid cursor sequences: ESC [ (params) H/J/K/A/B/C/D/f/s/u
	ansiRegex := regexp.MustCompile(`\x1b\[[0-9;]*[mHJKABCDfsu]`)

	// Remove all valid sequences
	stripped := ansiRegex.ReplaceAllString(view, "")

	// If any raw ESC remains, a sequence was corrupted/truncated
	if strings.Contains(stripped, "\x1b") {
		// Find the corrupted sequence for debugging
		escIdx := strings.Index(stripped, "\x1b")
		context := stripped[escIdx:min(escIdx+20, len(stripped))]
		t.Errorf("Found corrupted or incomplete ANSI sequence: %q", context)
	}

	// Ensure we actually had sequences (styled content expected)
	if !ansiRegex.MatchString(view) {
		t.Error("Expected ANSI sequences in output with ANSI profile enabled, found none")
	}
}

// TestSubAggregateAKeyJumpsToMessages verifies 'a' key in sub-aggregate view
// jumps to message list with the drill filter applied.

func TestExportAttachmentsModal(t *testing.T) {
	model := NewBuilder().
		WithDetail(&query.MessageDetail{
			ID:      1,
			Subject: "Test Email",
			Attachments: []query.AttachmentInfo{
				{ID: 1, Filename: "file1.pdf", Size: 1024, ContentHash: "abc123"},
				{ID: 2, Filename: "file2.txt", Size: 512, ContentHash: "def456"},
			},
		}).
		WithLevel(levelMessageDetail).
		WithPageSize(10).WithSize(100, 20).Build()

	// Press 'e' to open export modal
	m := applyDetailKey(t, model, key('e'))

	if m.modal != modalExportAttachments {
		t.Errorf("expected modalExportAttachments, got %v", m.modal)
	}

	// Should have all attachments selected by default
	if len(m.exportSelection) != 2 {
		t.Errorf("expected 2 attachments in selection, got %d", len(m.exportSelection))
	}
	if !m.exportSelection[0] || !m.exportSelection[1] {
		t.Error("expected all attachments to be selected by default")
	}

	// Test navigation - move cursor down
	m, _ = applyModalKey(t, m, key('j'))
	if m.exportCursor != 1 {
		t.Errorf("expected exportCursor = 1, got %d", m.exportCursor)
	}

	// Test toggle selection with space
	m, _ = applyModalKey(t, m, key(' '))
	if m.exportSelection[1] {
		t.Error("expected attachment 1 to be deselected after space")
	}

	// Test select none
	m, _ = applyModalKey(t, m, key('n'))
	if m.exportSelection[0] || m.exportSelection[1] {
		t.Error("expected all attachments to be deselected after 'n'")
	}

	// Test select all
	m, _ = applyModalKey(t, m, key('a'))
	if !m.exportSelection[0] || !m.exportSelection[1] {
		t.Error("expected all attachments to be selected after 'a'")
	}

	// Test cancel with Esc
	m, _ = applyModalKey(t, m, keyEsc())
	if m.modal != modalNone {
		t.Errorf("expected modalNone after Esc, got %v", m.modal)
	}
	if m.exportSelection != nil {
		t.Error("expected exportSelection to be cleared after Esc")
	}
}

func TestExportAttachmentsNoAttachments(t *testing.T) {
	model := NewBuilder().
		WithDetail(&query.MessageDetail{
			ID:          1,
			Subject:     "Test Email",
			Attachments: []query.AttachmentInfo{}, // No attachments
		}).
		WithLevel(levelMessageDetail).
		WithPageSize(10).WithSize(100, 20).Build()

	// Press 'e' should show flash message, not modal
	m := applyDetailKey(t, model, key('e'))

	if m.modal == modalExportAttachments {
		t.Error("expected modal NOT to open when no attachments")
	}
	if m.flashMessage != "No attachments to export" {
		t.Errorf("expected flash message 'No attachments to export', got '%s'", m.flashMessage)
	}
}

// TestRenderExportAttachmentsModalEdgeCases tests the export modal renderer
// handles edge cases gracefully (nil detail, empty attachments).
func TestRenderExportAttachmentsModalEdgeCases(t *testing.T) {
	t.Run("nil messageDetail shows no-attachments message", func(t *testing.T) {
		model := NewBuilder().
			WithLevel(levelMessageDetail).
			WithPageSize(10).WithSize(100, 20).Build()
		model.modal = modalExportAttachments
		model.messageDetail = nil

		content := model.renderExportAttachmentsModal()

		if content == "" {
			t.Error("expected non-empty modal content when messageDetail is nil")
		}
		if !strings.Contains(content, "Export Attachments") {
			t.Error("expected modal title in content")
		}
		if !strings.Contains(content, "No attachments") {
			t.Errorf("expected 'No attachments' message, got: %s", content)
		}
	})

	t.Run("empty attachments shows no-attachments message", func(t *testing.T) {
		model := NewBuilder().
			WithDetail(&query.MessageDetail{
				ID:          1,
				Subject:     "Test Email",
				Attachments: []query.AttachmentInfo{},
			}).
			WithLevel(levelMessageDetail).
			WithPageSize(10).WithSize(100, 20).Build()
		model.modal = modalExportAttachments

		content := model.renderExportAttachmentsModal()

		if content == "" {
			t.Error("expected non-empty modal content when attachments is empty")
		}
		if !strings.Contains(content, "Export Attachments") {
			t.Error("expected modal title in content")
		}
		if !strings.Contains(content, "No attachments") {
			t.Errorf("expected 'No attachments' message, got: %s", content)
		}
	})

	t.Run("with attachments shows normal list", func(t *testing.T) {
		model := NewBuilder().
			WithDetail(&query.MessageDetail{
				ID:      1,
				Subject: "Test Email",
				Attachments: []query.AttachmentInfo{
					{ID: 1, Filename: "doc.pdf", Size: 1024},
					{ID: 2, Filename: "image.png", Size: 2048},
				},
			}).
			WithLevel(levelMessageDetail).
			WithPageSize(10).WithSize(100, 20).Build()
		model.modal = modalExportAttachments
		model.exportSelection = map[int]bool{0: true, 1: true}

		content := model.renderExportAttachmentsModal()

		if !strings.Contains(content, "doc.pdf") {
			t.Error("expected 'doc.pdf' in content")
		}
		if !strings.Contains(content, "image.png") {
			t.Error("expected 'image.png' in content")
		}
		if strings.Contains(content, "No attachments") {
			t.Error("should not show 'No attachments' message when attachments exist")
		}
	})
}

// --- Helper method unit tests ---

// TestHeaderUpdateNoticeUnicode verifies update notice alignment with Unicode account names.
func TestHeaderUpdateNoticeUnicode(t *testing.T) {
	accountID := int64(1)
	model := NewBuilder().WithSize(100, 20).
		WithAccounts(query.AccountInfo{ID: 1, Identifier: "日本語ユーザー@example.com"}).
		WithAccountFilter(&accountID).Build()
	model.version = "abc1234"
	model.updateAvailable = "v1.2.3"

	header := model.headerView()
	lines := strings.Split(header, "\n")

	if !strings.Contains(lines[0], "v1.2.3") {
		t.Errorf("expected update notice in header, got: %s", lines[0])
	}
	// Verify the line doesn't exceed terminal width (lipgloss.Width accounts for wide chars)
	lineWidth := lipgloss.Width(lines[0])
	if lineWidth > 100 {
		t.Errorf("header line 1 width %d exceeds terminal width 100", lineWidth)
	}
}

// TestHeaderUpdateNoticeNarrowTerminal verifies update notice is omitted when terminal is too narrow.
func TestHeaderUpdateNoticeNarrowTerminal(t *testing.T) {
	model := NewBuilder().WithSize(40, 20).Build()
	model.version = "abc1234"
	model.updateAvailable = "v1.2.3"

	header := model.headerView()
	lines := strings.Split(header, "\n")

	// At 40 chars wide, the update notice shouldn't fit and should be omitted
	// (title + account already uses ~30 chars, notice needs ~25 more)
	lineWidth := lipgloss.Width(lines[0])
	if lineWidth > 40 {
		t.Errorf("header line 1 width %d exceeds narrow terminal width 40", lineWidth)
	}
}
