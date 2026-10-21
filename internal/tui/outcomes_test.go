package tui

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

func TestModeSwitchDoesNotSubmitRestoredTextSearch(t *testing.T) {
	require := require.New(t)
	m := NewBuilder().Build()
	m.width, m.height = 100, 24
	m.textEngine = meetingModeTextEngine{}
	m.textState.level = textLevelTimeline
	m.textState.selectedConvID = 701
	m.textState.filter = query.TextFilter{SearchQuery: "needle"}
	var events []string
	m.reportAction = func(_ context.Context, event, _, _ string) error { events = append(events, event); return nil }
	m, _ = sendKey(t, m, key('/'))
	m.searchInput.SetValue("needle")
	m, _ = sendKey(t, m, keyEnter())
	require.True(m.inlineSearchLoading)
	m, cmd := sendKey(t, m, key('m'))
	require.Equal(modeTexts, m.mode)
	require.NotNil(cmd)
	for _, msg := range runBatchCommand(t, cmd) {
		if reply, ok := msg.(textMessagesLoadedMsg); ok {
			require.NoError(reply.err)
			require.Equal("needle", reply.query)
			updated, report := m.Update(reply)
			m = asModel(t, updated)
			if report != nil {
				_ = runBatchCommand(t, report)
			}
		}
	}
	assert.Equal(t, []string{"search_run"}, events)
}

func TestFailedAggregatePreviewDoesNotCountEmptyOnEnter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := NewBuilder().Build()
	m.width, m.height = 100, 24
	m.inlineSearchActive = true
	m.searchQuery = "needle"
	m.searchInput.SetValue("needle")
	var events []string
	m.reportAction = func(_ context.Context, event, _, _ string) error { events = append(events, event); return nil }
	updated, cmd := m.Update(dataLoadedMsg{err: errors.New("synthetic preview failure")})
	m = asModel(t, updated)
	_ = runBatchCommand(t, cmd)
	require.Equal(modalError, m.modal)
	assert.Equal("needle", m.searchQuery)
	updated, _ = m.Update(keyEsc())
	m = asModel(t, updated)
	previousRequest := m.aggregateRequestID
	updated, cmd = m.Update(keyEnter())
	m = asModel(t, updated)
	if cmd != nil {
		_ = runBatchCommand(t, cmd)
	}
	assert.Equal(previousRequest, m.aggregateRequestID)
	assert.Equal([]string{"error_shown"}, events, "a failed preview has no accepted empty result")
}

func TestSupersededSubmissionDoesNotCountLaterPreview(t *testing.T) {
	for name, failed := range map[string]bool{"superseded": false, "errored": true} {
		t.Run(name, func(t *testing.T) {
			m := NewBuilder().Build()
			m.width, m.height = 100, 24
			m.level, m.inlineSearchActive = levelAggregates, true
			m.searchInput.SetValue("needle")
			var events []string
			m.reportAction = func(_ context.Context, event, _, _ string) error { events = append(events, event); return nil }
			updated, _ := m.Update(keyEnter())
			m = asModel(t, updated)
			oldID := m.aggregateRequestID
			if failed {
				updated, cmd := m.Update(dataLoadedMsg{requestID: oldID, searchQuery: "needle", err: errors.New("synthetic")})
				m = asModel(t, updated)
				if cmd != nil {
					_ = runBatchCommand(t, cmd)
				}
				updated, _ = m.Update(keyEsc())
				m = asModel(t, updated)
			}
			updated, _ = m.Update(key('/'))
			m = asModel(t, updated)
			m.searchInput.SetValue("needle")
			updated, _ = m.Update(searchDebounceMsg{query: "needle", debounceID: m.inlineSearchDebounce})
			m = asModel(t, updated)
			updated, cmd := m.Update(dataLoadedMsg{requestID: oldID, searchQuery: "needle"})
			m = asModel(t, updated)
			if cmd != nil {
				_ = runBatchCommand(t, cmd)
			}
			updated, cmd = m.Update(dataLoadedMsg{requestID: m.aggregateRequestID, searchQuery: "needle"})
			m = asModel(t, updated)
			if cmd != nil {
				_ = runBatchCommand(t, cmd)
			}
			assert.NotContains(t, events, "search_empty")
		})
	}
}

func TestErrorShownOnModalTransition(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		mode tuiMode
		msg  tea.Msg
	}{
		{"search", "search", modeEmail, searchResultsMsg{err: errors.New("synthetic")}},
		{"global texts search", "search", modeTexts, textSearchResultMsg{err: errors.New("synthetic")}},
		{"message", "message", modeEmail, messageDetailLoadedMsg{err: errors.New("synthetic")}},
		{"texts browse", "other", modeTexts, textConversationsLoadedMsg{err: errors.New("synthetic")}},
		{"messages search", "search", modeEmail, messagesLoadedMsg{err: errors.New("synthetic")}},
		{"archive browse", "other", modeEmail, dataLoadedMsg{err: errors.New("synthetic")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewBuilder().Build()
			m.width, m.height, m.mode = 100, 24, tc.mode
			if tc.kind == "search" {
				m.searchQuery = "needle"
			}
			var kinds []string
			m.reportAction = func(_ context.Context, event, property, value string) error {
				assert.Equal(t, "error_shown", event)
				assert.Equal(t, "kind", property)
				kinds = append(kinds, value)
				return nil
			}
			for range 2 {
				updated, cmd := m.Update(tc.msg)
				m = asModel(t, updated)
				if cmd != nil {
					_ = runBatchCommand(t, cmd)
				}
			}
			assert.Equal(t, []string{tc.kind}, kinds)
		})
	}
}

func TestInvalidTypedPreviewReportsOnlyOnEnter(t *testing.T) {
	for _, tc := range []struct {
		query string
		mode  searchModeKind
	}{{`after:"`, searchModeFast}, {"from:x", searchModeSemantic}} {
		t.Run(tc.query, func(t *testing.T) {
			m := NewBuilder().Build()
			m.width, m.height, m.inlineSearchActive, m.searchMode = 100, 24, true, tc.mode
			m.searchInput.SetValue(tc.query)
			var kinds []string
			m.reportAction = func(_ context.Context, event, property, value string) error {
				assert.Equal(t, "error_shown", event)
				assert.Equal(t, "kind", property)
				kinds = append(kinds, value)
				return nil
			}
			for i, msg := range []tea.Msg{searchDebounceMsg{query: tc.query, debounceID: m.inlineSearchDebounce}, keyEnter()} {
				updated, cmd := m.Update(msg)
				m = asModel(t, updated)
				if cmd != nil {
					_ = runBatchCommand(t, cmd)
				}
				require.NotEmpty(t, m.inlineSearchError)
				if i == 0 {
					assert.Empty(t, kinds)
				} else {
					assert.Equal(t, []string{"search"}, kinds)
				}
			}
		})
	}
}
