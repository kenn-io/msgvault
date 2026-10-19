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

func TestSubmittedEmptySearch(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mode          tuiMode
		loadedPreview bool
	}{
		{"pending", modeEmail, false},
		{"preview already loaded", modeEmail, true},
		{"texts", modeTexts, false},
		{"meetings", modeMeetings, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewBuilder().Build()
			m.mode = tc.mode
			m.width, m.height = 100, 24
			m.level = levelAggregates
			m.inlineSearchActive = true
			m.searchInput.SetValue("needle")
			if tc.loadedPreview {
				m.searchQuery = "needle"
			}
			if tc.mode == modeMeetings {
				m.inlineSearchActive = false
				m.meetingState.searchActive = true
				m.meetingState.searchInput.SetValue("needle")
			}
			var events []string
			m.reportAction = func(_ context.Context, event, _, _ string) error {
				if event == "search_empty" {
					events = append(events, event)
				}
				return nil
			}
			updated, cmd := m.Update(keyEnter())
			m = asModel(t, updated)
			require.False(t, m.inlineSearchActive)
			if tc.loadedPreview {
				_ = runBatchCommand(t, cmd)
			} else {
				var result tea.Msg
				switch tc.mode {
				case modeTexts:
					result = textSearchResultMsg{requestID: m.textRequestID, query: "needle"}
				case modeMeetings:
					result = meetingSearchLoadedMsg{requestID: m.meetingState.searchRequestID, query: "needle"}
				default:
					result = dataLoadedMsg{requestID: m.aggregateRequestID, searchQuery: "needle"}
				}
				updated, cmd = m.Update(result)
				m = asModel(t, updated)
				_ = runBatchCommand(t, cmd)
			}
			assert.Equal(t, []string{"search_empty"}, events)
			if tc.mode == modeTexts || tc.mode == modeMeetings {
				return
			}
			events = nil
			updated, _ = m.Update(keyTab())
			m = asModel(t, updated)
			_, cmd = m.Update(dataLoadedMsg{requestID: m.aggregateRequestID, searchQuery: "needle"})
			if cmd != nil {
				_ = runBatchCommand(t, cmd)
			}
			assert.NotContains(t, events, "search_empty", "a reload consumes no new submission")
		})
	}
}

func TestErrorShownOnModalTransition(t *testing.T) {
	for _, tc := range []struct {
		kind string
		msg  tea.Msg
	}{
		{"search", searchResultsMsg{err: errors.New("synthetic")}},
		{"message", messageDetailLoadedMsg{err: errors.New("synthetic")}},
		{"other", textConversationsLoadedMsg{err: errors.New("synthetic")}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			m := NewBuilder().Build()
			m.width, m.height = 100, 24
			if tc.kind == "other" {
				m.mode = modeTexts
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
