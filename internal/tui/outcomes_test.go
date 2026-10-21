package tui

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnterAfterFailedAggregatePreviewWaitsForReload(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := NewBuilder().Build()
	m.width, m.height = 100, 24
	m.level = levelAggregates
	m.inlineSearchActive = true
	m.inlineSearchLoading = true
	m.searchQuery = "needle"
	m.searchInput.SetValue("needle")
	var events []string
	m.reportAction = func(_ context.Context, event, _, _ string) error {
		if event == "search_run" || event == "search_empty" {
			events = append(events, event)
		}
		return nil
	}
	updated, cmd := m.Update(dataLoadedMsg{requestID: m.aggregateRequestID, err: errors.New("synthetic preview failure")})
	m = asModel(t, updated)
	require.Equal(modalError, m.modal)
	_ = runBatchCommand(t, cmd)
	updated, _ = m.Update(keyEsc())
	m = asModel(t, updated)
	previousRequest := m.aggregateRequestID
	updated, cmd = m.Update(keyEnter())
	m = asModel(t, updated)
	results := runBatchCommand(t, cmd)
	assert.Empty(events, "stale empty rows must not count as an accepted search")
	assert.Greater(m.aggregateRequestID, previousRequest)
	for _, result := range results {
		if loaded, ok := result.(dataLoadedMsg); ok {
			require.NoError(loaded.err)
			updated, cmd = m.Update(loaded)
			m = asModel(t, updated)
			_ = runBatchCommand(t, cmd)
		}
	}
	assert.Equal([]string{"search_run", "search_empty"}, events)
}

func TestDelayedEmailFailureDuringTextsSearchRetainsEmailQuery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := NewBuilder().Build()
	m.width, m.height = 100, 24
	m.textEngine = meetingModeTextEngine{}
	m.searchQuery = "email needle"
	delayed := dataLoadedMsg{
		requestID: m.aggregateRequestID, presentationGeneration: m.presentationGeneration,
		err: errors.New("synthetic delayed email failure"),
	}
	m = cycleToMode(t, m, modeTexts)
	updated, _ := m.Update(key('/'))
	m = asModel(t, updated)
	require.True(m.inlineSearchActive)
	m.searchInput.SetValue("text needle")
	updated, _ = m.Update(delayed)
	m = asModel(t, updated)
	assert.Equal("email needle", m.searchQuery)
	assert.Equal("text needle", m.searchInput.Value())
	assert.Equal(modalNone, m.modal)
	updated, _ = m.Update(keyEsc())
	m = cycleToMode(t, asModel(t, updated), modeEmail)
	assert.Equal("email needle", m.searchQuery, "returning to Email keeps its filter")
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

func TestInlineValidationErrorShownOnce(t *testing.T) {
	m := NewBuilder().Build()
	m.width, m.height = 100, 24
	m.inlineSearchActive = true
	m.searchMode = searchModeSemantic
	m.searchInput.SetValue("from:alice@example.com")
	var kinds []string
	m.reportAction = func(_ context.Context, event, property, value string) error {
		if event == "error_shown" {
			kinds = append(kinds, value)
		}
		return nil
	}
	for range 2 {
		updated, cmd := m.Update(keyEnter())
		m = asModel(t, updated)
		if cmd != nil {
			_ = runBatchCommand(t, cmd)
		}
		require.Contains(t, m.inlineSearchError, "semantic search requires free text")
		assert.Equal(t, []string{"search"}, kinds)
	}
}
