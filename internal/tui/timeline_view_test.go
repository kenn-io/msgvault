package tui

import (
	"encoding/json/jsontext"
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
	m.rebuildTimelineRows()
	markdown, _ := timeline.MarkdownWithEntries(m.timelineState.result)
	want := strings.Split(strings.TrimSuffix(markdown, "\n"), "\n")
	assert.Equal(want, m.timelineState.lines)
	view := stripANSI(m.renderView())
	for _, line := range want {
		if line != "" {
			assert.Contains(view, line)
		}
	}
	require.Len(m.timelineState.entries, 1)
	assert.Equal("burst:1:7:2027-03-14", m.timelineState.entries[0].Entry.AnchorRef)
	assert.Contains(view, "[Timeline]")
}

func TestTimelineViewHidesCursorAndKeepsNextPage(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	m := timelineModeModel()
	m.timelineState.result.Cursor = "opaque-continuation-token"
	m.rebuildTimelineRows()
	view := stripANSI(m.renderView())
	assertions.NotContains(view, "cursor:")
	assertions.NotContains(view, "opaque-continuation-token")
	assertions.Contains(view, "n next page")
	requirements.Len(m.timelineState.entries, 1)
	row := m.timelineState.entries[0]
	assertions.Contains(m.timelineState.lines[row.Line], row.Entry.AnchorRef)
	next, cmd := sendKey(t, m, key('n'))
	requirements.NotNil(cmd)
	assertions.Equal("opaque-continuation-token", next.timelineState.pending.Cursor)
}
func TestTimelineRowsKeepEveryEntryOnCombinedCallLines(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	m := timelineModeModel()
	m.timelineState.result.Days[0].Bursts = nil
	m.timelineState.result.Days[0].Calls = []timeline.Entry{
		{Kind: "call", Ref: "msg:20", AnchorRef: "msg:20", Time: m.timelineState.result.Now, Call: &timeline.CallEntry{Outcome: "missed"},
			Open: []timeline.Open{{Tool: "get_message", Args: map[string]any{"id": jsontext.Value("20")}}}},
		{Kind: "call", Ref: "msg:21", AnchorRef: "msg:21", Time: m.timelineState.result.Now, Call: &timeline.CallEntry{Outcome: "missed"},
			Open: []timeline.Open{{Tool: "get_message", Args: map[string]any{"id": jsontext.Value("21")}}}},
	}
	m.rebuildTimelineRows()
	require.Len(m.timelineState.entries, 2)
	assert.Equal(m.timelineState.entries[0].Line, m.timelineState.entries[1].Line)
	moved, _ := sendKey(t, m, key('j'))
	assert.Equal("msg:21", moved.highlightedTimelineEntry().Ref)
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
		assert.Equal([]string{"msg:20", "msg:21"}[i], around.timelineState.pending.Around)
	}
}

func TestTimelineRowSanitizesTerminalControls(t *testing.T) {
	assertions := assert.New(t)
	m := timelineModeModel()
	m.timelineState.result.Mode = timeline.ModeRelationship
	m.timelineState.result.History = m.timelineState.result.Days[0].Bursts
	m.timelineState.result.Days = nil
	m.timelineState.result.Person = &timeline.PersonHeader{Channels: []string{"email\x1b]52;c;unsafe\x07"}}
	m.rebuildTimelineRows()
	assertions.Contains(strings.Join(m.timelineState.lines, "\n"), "\x1b]52;c;unsafe\x07", "the shared Markdown must reach the terminal sanitizer")
	view := m.renderView()
	assertions.Contains(stripANSI(view), "email")
	assertions.NotContains(view, "\x1b]52;")
	assertions.NotContains(view, "unsafe")
}

func TestTimelineRowsMapRepeatedRefsToTheirOwnRenderedOccurrences(t *testing.T) {
	for _, tc := range []struct {
		name, firstText, firstLabel, secondText, firstRef, firstAnchor, secondAnchor string
		nextDay                                                                      bool
	}{
		{"repeated refs", "First day update", "First day update", "Second day update", "conv:7", "burst:1:7:2027-03-14", "burst:1:7:2027-03-15", true},
		{"ref-like sender", "Sender says · c7 · TIMELINE_SELECTION_0END", "Sender says", "Project update", "conv:8", "burst:1:8:2027-03-14", "burst:1:7:2027-03-14", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			m := timelineModeModel()
			first := m.timelineState.result.Days[0].Bursts[0]
			first.Ref, first.AnchorRef = tc.firstRef, tc.firstAnchor
			first.Burst = &timeline.BurstEntry{Channel: "email", Inbound: 1, Snippet: timeline.UntrustedText{Text: tc.firstText}}
			second := m.timelineState.result.Days[0].Bursts[0]
			second.AnchorRef = tc.secondAnchor
			if tc.nextDay {
				second.Time = first.Time.AddDate(0, 0, 1)
				second.Burst = &timeline.BurstEntry{Channel: "email", Inbound: 2, Snippet: timeline.UntrustedText{Text: tc.secondText}}
				m.timelineState.result.Window.To = "2027-03-15"
				m.timelineState.result.Days = []timeline.Day{
					{Date: "2027-03-14", Bursts: []timeline.Entry{first}},
					{Date: "2027-03-15", Bursts: []timeline.Entry{second}},
				}
			} else {
				second.Time = first.Time.Add(time.Hour)
				m.timelineState.result.Days[0].Bursts = []timeline.Entry{first, second}
			}
			m.rebuildTimelineRows()
			require.Len(m.timelineState.entries, 2)
			firstRow, secondRow := m.timelineState.entries[0], m.timelineState.entries[1]
			assert.NotEqual(firstRow.Line, secondRow.Line)
			assert.Contains(m.timelineState.lines[firstRow.Line], tc.firstLabel)
			assert.Contains(m.timelineState.lines[secondRow.Line], tc.secondText)
			assert.NotContains(m.timelineState.lines[secondRow.Line], tc.firstLabel)
			moved, _ := sendKey(t, m, key('j'))
			assert.Equal(secondRow.Line, moved.timelineState.entries[moved.timelineState.cursor].Line)
			around, cmd := sendKey(t, moved, key('a'))
			require.NotNil(cmd)
			assert.Equal(second.AnchorRef, around.timelineState.pending.Around)
		})
	}
}
