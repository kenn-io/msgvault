package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/timeline"
)

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
	assertions.Contains(view, "r to retry")
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
	m.timelineState.scroll = 1
	m.timelineState.result.Cursor = "second-page"
	m, cmd := sendKey(t, m, key('n'))
	requirements.NotNil(cmd)
	m = sendMsg(t, m, cmd())
	m, cmd = sendKey(t, m, key('r'))
	defer m.cancelTimelineRequest()
	requirements.NotNil(cmd)
	m, _ = sendKey(t, m, keyEsc())
	assertions.Equal(previous, m.timelineState.request)
	assertions.Equal(1, m.timelineState.scroll)
	assertions.Empty(m.timelineState.history)
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
