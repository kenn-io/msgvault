package tui

import (
    "strings"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    "go.kenn.io/msgvault/internal/timeline"
)

func TestTimelineViewRendersSharedMarkdownRows(t *testing.T) {
    m := timelineModeModel()
    want := strings.Split(strings.TrimSuffix(timeline.Markdown(m.timelineState.result), "\n"), "\n")
    assert.Equal(t, want, m.timelineState.lines)
    view := stripANSI(m.renderView())
    for _, line := range want {
        if line != "" { assert.Contains(t, view, line) }
    }
    require.Len(t, m.timelineState.entries, 1)
    assert.Equal(t, "burst:1:7:2027-03-14", m.timelineState.entries[0].entry.AnchorRef)
    assert.Contains(t, view, "[Timeline]")
}
func TestTimelineRowsKeepEveryEntryOnCombinedCallLines(t *testing.T) {
    m := timelineModeModel()
    m.timelineState.result.Days[0].Calls = []timeline.Entry{
        {Kind: "call", Ref: "msg:20", AnchorRef: "msg:20", Call: &timeline.CallEntry{Outcome: "missed"}},
        {Kind: "call", Ref: "msg:21", AnchorRef: "msg:21", Call: &timeline.CallEntry{Outcome: "missed"}},
    }
    m.rebuildTimelineRows()
    require.Len(t, m.timelineState.entries, 3)
    assert.Equal(t, m.timelineState.entries[1].line, m.timelineState.entries[2].line)
    moved, _ := sendKey(t, m, key('j'))
    moved, _ = sendKey(t, moved, key('j'))
    around, cmd := sendKey(t, moved, key('a'))
    require.NotNil(t, cmd)
    assert.Equal(t, "msg:21", around.timelineState.request.Around)
}
func TestTimelineHiddenRowsOnlyShowSharedWarning(t *testing.T) {
    m := timelineModeModel()
    m.timelineState.result.Days[0].Bursts[0].HiddenText = true
    m.rebuildTimelineRows()
    view := stripANSI(m.renderView())
    assert.Contains(t, view, "[hidden text]")
    assert.NotContains(t, view, "Project update")
    assert.NotContains(t, view, "Example Contact")
    assert.NotContains(t, view, "\x1b]52;")
}

func TestTimelineRowsMapRepeatedRefsToTheirOwnRenderedOccurrences(t *testing.T) {
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
    require.Len(t, m.timelineState.entries, 2)
    assert.Equal(t, strings.Split(strings.TrimSuffix(timeline.Markdown(m.timelineState.result), "\n"), "\n"), m.timelineState.lines)
    firstRow, secondRow := m.timelineState.entries[0], m.timelineState.entries[1]
    assert.NotEqual(t, firstRow.line, secondRow.line)
    assert.Contains(t, m.timelineState.lines[firstRow.line], "First day update")
    assert.Contains(t, m.timelineState.lines[secondRow.line], "Second day update")
    moved, _ := sendKey(t, m, key('j'))
    assert.Equal(t, secondRow.line, moved.timelineState.entries[moved.timelineState.cursor].line)
    around, cmd := sendKey(t, moved, key('a'))
    require.NotNil(t, cmd)
    assert.Equal(t, second.AnchorRef, around.timelineState.request.Around)
}

func TestTimelineRowsIgnoreRefLikeSenderText(t *testing.T) {
    m := timelineModeModel()
    target := m.timelineState.result.Days[0].Bursts[0]
    decoy := target
    decoy.Ref, decoy.AnchorRef = "conv:8", "burst:1:8:2027-03-14"
    decoy.Burst = &timeline.BurstEntry{Channel: "email", Inbound: 1,
        Snippet: timeline.UntrustedText{Text: "Sender says · c7 · TIMELINE_SELECTION_0END"}}
    target.Time = target.Time.Add(time.Hour)
    m.timelineState.result.Days[0].Bursts = []timeline.Entry{decoy, target}
    m.rebuildTimelineRows()
    require.Len(t, m.timelineState.entries, 2)
    assert.Equal(t, strings.Split(strings.TrimSuffix(timeline.Markdown(m.timelineState.result), "\n"), "\n"), m.timelineState.lines)
    decoyRow, targetRow := m.timelineState.entries[0], m.timelineState.entries[1]
    assert.NotEqual(t, decoyRow.line, targetRow.line)
    assert.Contains(t, m.timelineState.lines[decoyRow.line], "Sender says")
    assert.Contains(t, m.timelineState.lines[targetRow.line], "Project update")
    assert.NotContains(t, m.timelineState.lines[targetRow.line], "Sender says")
    moved, _ := sendKey(t, m, key('j'))
    around, cmd := sendKey(t, moved, key('a'))
    require.NotNil(t, cmd)
    assert.Equal(t, target.AnchorRef, around.timelineState.request.Around)
}
