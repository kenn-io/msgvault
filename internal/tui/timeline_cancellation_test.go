package tui

import (
	"errors"
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
			requested := m.timelineState.request
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

func TestTimelineDayStepAcrossSkippedMidnight(t *testing.T) {
	for _, tc := range []struct {
		key  rune
		date string
	}{{'[', "2026-09-05"}, {']', "2026-09-07"}} {
		t.Run(string(tc.key), func(t *testing.T) {
			m := timelineModeModel()
			m.timelineState.result.Timezone = "America/Santiago"
			m.timelineState.result.Window = timeline.Window{From: "2026-09-06", To: "2026-09-06"}
			m, cmd := sendKey(t, m, key(tc.key))
			defer m.cancelTimelineRequest()
			require.NotNil(t, cmd)
			assert.Equal(t, tc.date, m.timelineState.request.Date)
		})
	}
}
