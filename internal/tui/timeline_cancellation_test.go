package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/timeline"
)

func TestTimelineModeExitSettlesLoading(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	m := timelineModeModel()
	m.engine = &querytest.MockEngine{}
	cmd := m.startTimelineRequest(m.timelineState.request, false)
	requirements.NotNil(cmd)
	late := cmd()
	backend, ok := m.timelineBackend.(*timelineModeBackend)
	requirements.True(ok)
	away, cmd := sendKey(t, m, key('m'))
	requirements.Equal(modeEmail, away.mode)
	requirements.ErrorIs(backend.contexts[0].Err(), context.Canceled)
	assertions.False(away.timelineState.loading)
	away = sendMsg(t, away, late)
	requirements.NotNil(cmd)
	batch, ok := cmd().(tea.BatchMsg)
	requirements.True(ok)
	for _, load := range batch {
		msg := load()
		if _, ok := msg.(dataLoadedMsg); ok {
			away = sendMsg(t, away, msg)
		}
	}
	assertions.False(away.loading)
}

func TestTimelineRefreshFailureRestoresLastResult(t *testing.T) {
	m := timelineModeModel()
	original := m.timelineState.result
	originalRequest := m.timelineState.request
	m.timelineState.scroll = 1
	refreshed, cmd := sendKey(t, m, key('r'))
	defer refreshed.cancelTimelineRequest()
	require := require.New(t)
	require.NotNil(cmd)
	msg, ok := cmd().(timelineLoadedMsg)
	require.True(ok)
	failure := errors.New("daemon temporarily unavailable")
	msg.result, msg.err = nil, failure
	refreshed = sendMsg(t, refreshed, msg)
	assert := assert.New(t)
	assert.Contains(stripANSI(refreshed.renderView()), failure.Error())
	restored, _ := sendKey(t, refreshed, keyEsc())
	require.NoError(restored.timelineState.err)
	assert.Same(original, restored.timelineState.result)
	assert.Equal(originalRequest, restored.timelineState.request)
	assert.Equal(1, restored.timelineState.scroll)
	assert.NotEmpty(restored.timelineState.entries)
	assert.Contains(stripANSI(restored.renderView()), "Project update")
}

func TestTimelineInitialLoadFailureKeepsErrorAfterEscape(t *testing.T) {
	m := timelineModeModel()
	m.timelineState.result = nil
	cmd := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeDay}, false)
	defer m.cancelTimelineRequest()
	require := require.New(t)
	require.NotNil(cmd)
	msg, ok := cmd().(timelineLoadedMsg)
	require.True(ok)
	failure := errors.New("daemon temporarily unavailable")
	msg.result, msg.err = nil, failure
	m = sendMsg(t, m, msg)
	m, _ = sendKey(t, m, keyEsc())
	require.ErrorIs(m.timelineState.err, failure)
	assert := assert.New(t)
	assert.Contains(stripANSI(m.renderView()), failure.Error())
}

func TestTimelineFailedNavigationEscapeKeepsPageAndHistory(t *testing.T) {
	for _, cancelKey := range []tea.KeyPressMsg{keyEsc(), {Code: tea.KeyBackspace}} {
		t.Run(cancelKey.String(), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			m := timelineModeModel()
			previous, previousRequest := m.timelineState.result, m.timelineState.request
			m.timelineState.result.Cursor = "second-page"
			m, cmd := sendKey(t, m, key('n'))
			requirements.NotNil(cmd)
			m = sendMsg(t, m, cmd())
			requirements.Len(m.timelineState.history, 1)
			displayed, displayedRequest := m.timelineState.result, m.timelineState.request
			m.timelineState.scroll = 1
			m, cmd = sendKey(t, m, key(']'))
			defer m.cancelTimelineRequest()
			requirements.NotNil(cmd)
			msg, ok := cmd().(timelineLoadedMsg)
			requirements.True(ok)
			msg.result, msg.err = nil, errors.New("day unavailable")
			m = sendMsg(t, m, msg)
			m, _ = sendKey(t, m, cancelKey)
			assertions.Same(displayed, m.timelineState.result)
			assertions.Equal(displayedRequest, m.timelineState.request)
			assertions.Equal(1, m.timelineState.scroll)
			assertions.Len(m.timelineState.history, 1)
			assertions.False(m.timelineState.needsLoad)
			requirements.NoError(m.timelineState.err)
			m, _ = sendKey(t, m, cancelKey)
			assertions.Same(previous, m.timelineState.result)
			assertions.Equal(previousRequest, m.timelineState.request)
			assertions.Empty(m.timelineState.history)
		})
	}
}

func TestTimelineInitialLoadCancellationIsVisible(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	m := timelineModeModel()
	defer m.cancelTimelineRequest()
	m.timelineState.result = nil
	cmd := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeDay}, false)
	requirements.NotNil(cmd)
	m, _ = sendKey(t, m, keyEsc())
	view := stripANSI(m.renderView())
	assertions.Contains(strings.ToLower(view), "cancelled")
	retry, cmd := sendKey(t, m, key('r'))
	defer retry.cancelTimelineRequest()
	requirements.NotNil(cmd)
	assertions.True(retry.timelineState.loading)
}

func TestTimelineFailedNavigationKeepsPageAndRetriesRequest(t *testing.T) {
	for _, keyName := range []rune{'n', '[', ']'} {
		t.Run(string(keyName), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			m := timelineModeModel()
			m.timelineState.result.Cursor = "next-page"
			original := m.timelineState.result
			m.timelineState.scroll = 1
			m, cmd := sendKey(t, m, key(keyName))
			defer m.cancelTimelineRequest()
			requirements.NotNil(cmd)
			requested := m.timelineState.pending
			msg, ok := cmd().(timelineLoadedMsg)
			requirements.True(ok)
			msg.result, msg.err = nil, errors.New("temporarily unavailable")
			m = sendMsg(t, m, msg)
			assertions.Same(original, m.timelineState.result)
			assertions.Equal(1, m.timelineState.scroll)
			assertions.Contains(stripANSI(m.renderView()), "Project update")
			m, cmd = sendKey(t, m, key('r'))
			defer m.cancelTimelineRequest()
			requirements.NotNil(cmd)
			cmd()
			backend, ok := m.timelineBackend.(*timelineModeBackend)
			requirements.True(ok)
			assertions.Equal(requested, backend.requests[len(backend.requests)-1])
		})
	}
}

func TestTimelineFailedDayThenNextUsesDisplayedRequest(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	m := timelineModeModel()
	m.timelineState.result.Cursor = "displayed-page-cursor"
	displayed := m.timelineState.request
	m, cmd := sendKey(t, m, key(']'))
	defer m.cancelTimelineRequest()
	requirements.NotNil(cmd)
	msg, ok := cmd().(timelineLoadedMsg)
	requirements.True(ok)
	msg.result, msg.err = nil, errors.New("day unavailable")
	m = sendMsg(t, m, msg)
	m, cmd = sendKey(t, m, key('n'))
	defer m.cancelTimelineRequest()
	requirements.NotNil(cmd)
	cmd()
	backend, ok := m.timelineBackend.(*timelineModeBackend)
	requirements.True(ok)
	displayed.Cursor = "displayed-page-cursor"
	assertions.Equal(displayed, backend.requests[len(backend.requests)-1])
}

func TestTimelineCancelledRefreshEscapeRestoresPreviousPage(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	m := timelineModeModel()
	previous := m.timelineState.request
	previousResult := m.timelineState.result
	backend, ok := m.timelineBackend.(*timelineModeBackend)
	requirements.True(ok)
	m.timelineState.scroll = 1
	m.timelineState.result.Cursor = "second-page"
	m, cmd := sendKey(t, m, key('n'))
	requirements.NotNil(cmd)
	m = sendMsg(t, m, cmd())
	requirements.Len(m.timelineState.history, 1)
	m, cmd = sendKey(t, m, key('r'))
	defer m.cancelTimelineRequest()
	requirements.NotNil(cmd)
	late := cmd()
	m, _ = sendKey(t, m, keyEsc())
	assertions.Len(m.timelineState.history, 1)
	m, _ = sendKey(t, m, keyEsc())
	requirements.Len(backend.contexts, 2)
	requirements.ErrorIs(backend.contexts[1].Err(), context.Canceled)
	assertions.Same(previousResult, m.timelineState.result)
	assertions.Equal(previous, m.timelineState.request)
	assertions.Equal(1, m.timelineState.scroll)
	assertions.Empty(m.timelineState.history)
	m = sendMsg(t, m, late)
	assertions.Same(previousResult, m.timelineState.result)
	assertions.Equal(previous, m.timelineState.request)
	assertions.Equal(1, m.timelineState.scroll)
}

func TestTimelineCancelledDayKeepsDisplayedPageAndHistory(t *testing.T) {
	for _, cancelKey := range []tea.KeyPressMsg{keyEsc(), {Code: tea.KeyBackspace}} {
		t.Run(cancelKey.String(), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			m := timelineModeModel()
			m.timelineState.result.Cursor = "second-page"
			m, cmd := sendKey(t, m, key('n'))
			requirements.NotNil(cmd)
			m = sendMsg(t, m, cmd())
			requirements.Len(m.timelineState.history, 1)
			displayed := m.timelineState.result
			displayedRequest := m.timelineState.request
			m.timelineState.scroll = 1
			m, cmd = sendKey(t, m, key(']'))
			defer m.cancelTimelineRequest()
			requirements.NotNil(cmd)
			late := cmd()
			m, _ = sendKey(t, m, cancelKey)
			assertions.Same(displayed, m.timelineState.result)
			assertions.Equal(displayedRequest, m.timelineState.request)
			assertions.Equal(1, m.timelineState.scroll)
			assertions.Len(m.timelineState.history, 1)
			assertions.False(m.timelineState.loading)
			assertions.False(m.timelineState.needsLoad)
			assertions.Contains(stripANSI(m.renderView()), "Project update")
			m = sendMsg(t, m, late)
			assertions.Same(displayed, m.timelineState.result)
			assertions.Equal(displayedRequest, m.timelineState.request)
			assertions.Equal(1, m.timelineState.scroll)
			assertions.Len(m.timelineState.history, 1)
		})
	}
}

func TestTimelineHistoryStopsAtTwenty(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	m := timelineModeModel()
	for i := range 25 {
		req := m.timelineState.request
		req.Cursor = fmt.Sprintf("page-%d", i)
		cmd := m.startTimelineRequest(req, true)
		requirements.NotNil(cmd)
		m = sendMsg(t, m, cmd())
	}
	assertions.Len(m.timelineState.history, 20)
	assertions.Equal("page-4", m.timelineState.history[0].request.Cursor)
}
