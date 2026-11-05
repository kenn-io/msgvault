package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/timeline"
)

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
