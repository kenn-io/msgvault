package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/timeline"
)

func TestTimelineViewRendersSharedMarkdownRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := timelineModeModel()
	want := strings.Split(strings.TrimSuffix(timeline.Markdown(m.timelineState.result), "\n"), "\n")
	assert.Equal(want, m.timelineState.lines)
	view := stripANSI(m.renderView())
	for _, line := range want {
		if line != "" {
			assert.Contains(view, line)
		}
	}
	require.Len(m.timelineState.entries, 1)
	assert.Equal("burst:1:7:2027-03-14", m.timelineState.entries[0].entry.AnchorRef)
	assert.Contains(view, "[Timeline]")
}
func TestTimelineRowsKeepEveryEntryOnCombinedCallLines(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	m := timelineModeModel()
	m.timelineState.result.Days[0].Bursts = nil
	m.timelineState.result.Days[0].Calls = []timeline.Entry{
		{Kind: "call", Ref: "msg:20", AnchorRef: "msg:20", Time: m.timelineState.result.Now, Call: &timeline.CallEntry{Outcome: "missed"},
			Open: []timeline.Open{{Tool: "get_message", Args: map[string]any{"id": int64(20)}}}},
		{Kind: "call", Ref: "msg:21", AnchorRef: "msg:21", Time: m.timelineState.result.Now, Call: &timeline.CallEntry{Outcome: "missed"},
			Open: []timeline.Open{{Tool: "get_message", Args: map[string]any{"id": int64(21)}}}},
	}
	m.rebuildTimelineRows()
	require.Len(m.timelineState.entries, 2)
	assert.Equal(m.timelineState.entries[0].line, m.timelineState.entries[1].line)
	originalLines := append([]string(nil), m.timelineState.lines...)
	firstView := stripANSI(m.renderView())
	moved, _ := sendKey(t, m, key('j'))
	secondView := stripANSI(moved.renderView())
	assert.NotEqual(firstView, secondView, "selecting another call on the same line must be visible")
	assert.Contains(firstView, "Selected: msg:20")
	assert.Contains(secondView, "Selected: msg:21")
	assert.Equal(originalLines, moved.timelineState.lines, "shared Markdown body stays verbatim")
	for i, selected := range []Model{m, moved} {
		id := int64(20 + i)
		selected.engine = newMockEngine(MockConfig{Detail: &query.MessageDetail{ID: id}})
		opened, cmd := sendKey(t, selected, keyEnter())
		require.NotNil(cmd)
		loaded := sendMsg(t, opened, cmd())
		require.NotNil(loaded.messageDetail)
		assert.Equal(id, loaded.messageDetail.ID)
		around, cmd := sendKey(t, selected, key('a'))
		require.NotNil(cmd)
		assert.Equal([]string{"msg:20", "msg:21"}[i], around.timelineState.request.Around)
	}
}

func TestTimelineSelectedRefSanitizesTerminalControls(t *testing.T) {
	assert := assert.New(t)
	m := timelineModeModel()
	m.timelineState.result.Days[0].Bursts[0].Ref = "conv:7\x1b]52;c;unsafe\x07"
	m.rebuildTimelineRows()
	view := m.renderView()
	assert.Contains(stripANSI(view), "Selected: conv:7")
	assert.NotContains(view, "\x1b]52;")
}

func TestTimelineHiddenRowsOnlyShowSharedWarning(t *testing.T) {
	assert := assert.New(t)
	m := timelineModeModel()
	m.timelineState.result.Days[0].Bursts[0].HiddenText = true
	m.rebuildTimelineRows()
	view := stripANSI(m.renderView())
	assert.Contains(view, "[hidden text]")
	assert.NotContains(view, "Project update")
	assert.NotContains(view, "Example Contact")
	assert.NotContains(view, "\x1b]52;")
}

func TestTimelineRowsMapRepeatedRefsToTheirOwnRenderedOccurrences(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := timelineModeModel()
	first := m.timelineState.result.Days[0].Bursts[0]
	first.Burst = &timeline.BurstEntry{Channel: "email", Inbound: 1,
		Snippet: timeline.UntrustedText{Text: "First day update"}}
	second := first
	second.AnchorRef = "burst:1:7:2027-03-15"
	second.Time = first.Time.AddDate(0, 0, 1)
	second.Burst = &timeline.BurstEntry{Channel: "email", Inbound: 2,
		Snippet: timeline.UntrustedText{Text: "Second day update"}}
	m.timelineState.result.Window.To = "2027-03-15"
	m.timelineState.result.Days = []timeline.Day{
		{Date: "2027-03-14", Bursts: []timeline.Entry{first}},
		{Date: "2027-03-15", Bursts: []timeline.Entry{second}},
	}
	m.rebuildTimelineRows()
	require.Len(m.timelineState.entries, 2)
	assert.Equal(strings.Split(strings.TrimSuffix(timeline.Markdown(m.timelineState.result), "\n"), "\n"), m.timelineState.lines)
	firstRow, secondRow := m.timelineState.entries[0], m.timelineState.entries[1]
	assert.NotEqual(firstRow.line, secondRow.line)
	assert.Contains(m.timelineState.lines[firstRow.line], "First day update")
	assert.Contains(m.timelineState.lines[secondRow.line], "Second day update")
	moved, _ := sendKey(t, m, key('j'))
	assert.Equal(secondRow.line, moved.timelineState.entries[moved.timelineState.cursor].line)
	around, cmd := sendKey(t, moved, key('a'))
	require.NotNil(cmd)
	assert.Equal(second.AnchorRef, around.timelineState.request.Around)
}

func TestTimelineRowsIgnoreRefLikeSenderText(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := timelineModeModel()
	target := m.timelineState.result.Days[0].Bursts[0]
	decoy := target
	decoy.Ref, decoy.AnchorRef = "conv:8", "burst:1:8:2027-03-14"
	decoy.Burst = &timeline.BurstEntry{Channel: "email", Inbound: 1,
		Snippet: timeline.UntrustedText{Text: "Sender says · c7 · TIMELINE_SELECTION_0END"}}
	target.Time = target.Time.Add(time.Hour)
	m.timelineState.result.Days[0].Bursts = []timeline.Entry{decoy, target}
	m.rebuildTimelineRows()
	require.Len(m.timelineState.entries, 2)
	assert.Equal(strings.Split(strings.TrimSuffix(timeline.Markdown(m.timelineState.result), "\n"), "\n"), m.timelineState.lines)
	decoyRow, targetRow := m.timelineState.entries[0], m.timelineState.entries[1]
	assert.NotEqual(decoyRow.line, targetRow.line)
	assert.Contains(m.timelineState.lines[decoyRow.line], "Sender says")
	assert.Contains(m.timelineState.lines[targetRow.line], "Project update")
	assert.NotContains(m.timelineState.lines[targetRow.line], "Sender says")
	moved, _ := sendKey(t, m, key('j'))
	around, cmd := sendKey(t, moved, key('a'))
	require.NotNil(cmd)
	assert.Equal(target.AnchorRef, around.timelineState.request.Around)
}
