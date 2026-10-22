package tui

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
