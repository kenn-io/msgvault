package tui

import (
    "context"
    "testing"
    "time"

    tea "charm.land/bubbletea/v2"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    "go.kenn.io/msgvault/internal/query"
    "go.kenn.io/msgvault/internal/timeline"
)

type timelineModeBackend struct {
    requests []timeline.Request
    contexts []context.Context
    result *timeline.Timeline
}
func (b *timelineModeBackend) Timeline(ctx context.Context, req timeline.Request) (*timeline.Timeline, string, error) {
    b.requests = append(b.requests, req)
    b.contexts = append(b.contexts, ctx)
    return b.result, "", nil
}
func timelineModeFixture() *timeline.Timeline {
    at := time.Date(2027, 3, 14, 8, 30, 0, 0, time.UTC)
    return &timeline.Timeline{
        Mode: timeline.ModeDay, Timezone: "America/Vancouver", Now: at,
        Window: timeline.Window{From: "2027-03-14", To: "2027-03-14"},
        Days: []timeline.Day{{Date: "2027-03-14", Bursts: []timeline.Entry{{
            Kind: "burst", Key: "burst:1:7:2027-03-14", Ref: "conv:7",
            AnchorRef: "burst:1:7:2027-03-14", Time: at,
            Open: []timeline.Open{{Tool: "list_messages", Args: map[string]any{"conversation_id": int64(7)}}},
            Burst: &timeline.BurstEntry{Channel: "email", Inbound: 1,
                People: []timeline.Person{{PersonID: 42, Name: "Example Contact"}},
                Snippet: timeline.UntrustedText{Text: "Project update"}},
        }}}},
    }
}
func timelineModeModel() Model {
    m := NewBuilder().WithSize(120, 30).Build()
    m.mode = modeTimeline
    m.timelineBackend = &timelineModeBackend{result: timelineModeFixture()}
    m.timelineState.result = timelineModeFixture()
    m.timelineState.request = timeline.Request{Mode: timeline.ModeDay, Date: "2027-03-14", Format: "json"}
    m.rebuildTimelineRows()
    m.loading = false
    return m
}
func TestNextModeIncludesTimelineWhenAvailable(t *testing.T) {
    assert.Equal(t, modeTimeline, nextMode(modePeople, true, true, true))
    assert.Equal(t, modeTimeline, nextMode(modeMeetings, false, false, true))
    assert.Equal(t, modePeople, nextMode(modeMeetings, false, true, true))
    assert.Equal(t, modeEmail, nextMode(modeTimeline, true, true, true))
    assert.Equal(t, modeEmail, nextMode(modeMeetings, false, false, false))
    m := NewBuilder().Build()
    m.mode = modeMeetings
    m.timelineBackend = &timelineModeBackend{}
    updated, _, handled := m.handleGlobalKeys(key('m'))
    require.True(t, handled)
    assert.Equal(t, modeTimeline, updated.mode)
}
func TestTimelineKeysMoveDaysAndSwitchModes(t *testing.T) {
    for _, tc := range []struct{ key rune; mode timeline.Mode; date, around, with string }{
        {key: '[', mode: timeline.ModeDay, date: "2027-03-13"},
        {key: ']', mode: timeline.ModeDay, date: "2027-03-15"},
        {key: 'a', mode: timeline.ModeAnchor, around: "burst:1:7:2027-03-14"},
        {key: 'w', mode: timeline.ModeRelationship, with: "person:42"},
    } {
        t.Run(string(tc.key), func(t *testing.T) {
            m := timelineModeModel()
            updated, cmd := sendKey(t, m, key(tc.key))
            require.NotNil(t, cmd)
            assert.Equal(t, tc.mode, updated.timelineState.request.Mode)
            assert.Equal(t, tc.date, updated.timelineState.request.Date)
            assert.Equal(t, tc.around, updated.timelineState.request.Around)
            assert.Equal(t, tc.with, updated.timelineState.request.With)
            assert.Equal(t, "json", updated.timelineState.request.Format)
            returned, _ := sendKey(t, updated, keyEsc())
            assert.Equal(t, m.timelineState.result, returned.timelineState.result)
            assert.Equal(t, m.timelineState.request, returned.timelineState.request)
        })
    }
}
func TestTimelineModeLocalKeysWinOverGlobalA(t *testing.T) {
    m := timelineModeModel()
    updated, cmd := sendKey(t, m, key('a'))
    require.NotNil(t, cmd)
    assert.Equal(t, timeline.ModeAnchor, updated.timelineState.request.Mode)
    assert.Equal(t, modalNone, updated.modal)
    assert.Equal(t, allSourceScope(), updated.sourceScope)
    for _, k := range []tea.KeyPressMsg{key('?'), key('m')} {
        next, _ := sendKey(t, m, k)
        if k.String() == "?" { assert.Equal(t, modalHelp, next.modal) } else { assert.Equal(t, modeEmail, next.mode) }
    }
}
func TestTimelineCancelsAndDiscardsStaleResults(t *testing.T) {
    m := timelineModeModel()
    first := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeDay, Date: "2027-03-13"}, true)
    require.NotNil(t, first)
    old := first().(timelineLoadedMsg)
    backend := m.timelineBackend.(*timelineModeBackend)
    require.Len(t, backend.contexts, 1)
    second := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeDay, Date: "2027-03-15"}, true)
    require.NotNil(t, second)
    assert.ErrorIs(t, backend.contexts[0].Err(), context.Canceled)
    m = sendMsg(t, m, old)
    assert.True(t, m.timelineState.loading)
    assert.Equal(t, "2027-03-15", m.timelineState.request.Date)
    active := second().(timelineLoadedMsg)
    m = sendMsg(t, m, active)
    assert.False(t, m.timelineState.loading)
    assert.Equal(t, backend.result, m.timelineState.result)
    third := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeAnchor, Around: "msg:9"}, true)
    late := third().(timelineLoadedMsg)
    next, _, handled := m.handleGlobalKeys(key('m'))
    require.True(t, handled)
    assert.ErrorIs(t, backend.contexts[2].Err(), context.Canceled)
    before := next.timelineState.result
    next = sendMsg(t, next, late)
    assert.Equal(t, before, next.timelineState.result)
    assert.Equal(t, modeEmail, next.mode)
}
func TestTimelineReadersPreserveOtherModes(t *testing.T) {
    m := timelineModeModel()
    emailDetail := &query.MessageDetail{ID: 100, Subject: "Email reader"}
    m.parkedMessageReaders[modeEmail] = messageReaderState{messageDetail: emailDetail, detailScroll: 4, detailSearchQuery: "saved"}
    m.timelineState.result.Days[0].Bursts[0].Open = []timeline.Open{{Tool: "get_message", Args: map[string]any{"id": int64(101)}}}
    m.rebuildTimelineRows()
    opened, cmd := sendKey(t, m, keyEnter())
    require.NotNil(t, cmd)
    assert.Equal(t, timelineReaderMessage, opened.timelineState.reader)
    loaded := sendMsg(t, opened, timelineReaderLoadedMsg{
        requestID: opened.timelineState.requestID, generation: opened.presentationGeneration,
        detail: &query.MessageDetail{ID: 101, Subject: "Timeline reader"},
    })
    require.NotNil(t, loaded.messageDetail)
    assert.Contains(t, stripANSI(loaded.renderView()), "Timeline reader")
    back, _ := sendKey(t, loaded, keyEsc())
    assert.Equal(t, timelineReaderNone, back.timelineState.reader)
    assert.Equal(t, 0, back.timelineState.cursor)
    email, _, handled := back.handleGlobalKeys(key('m'))
    require.True(t, handled)
    assert.Same(t, emailDetail, email.messageDetail)
    assert.Equal(t, 4, email.detailScroll)
    assert.Equal(t, "saved", email.detailSearchQuery)
}
func TestTimelineOpenUsesEmittedTargets(t *testing.T) {
    m := timelineModeModel()
    opened, cmd := sendKey(t, m, keyEnter())
    require.NotNil(t, cmd)
    assert.Equal(t, timelineReaderThread, opened.timelineState.reader)
    assert.Equal(t, int64(7), opened.timelineState.readerID)
    m.timelineState.result.Days[0].Bursts[0].HiddenText = true
    m.rebuildTimelineRows()
    hidden, cmd := sendKey(t, m, keyEnter())
    assert.Nil(t, cmd)
    assert.Equal(t, timelineReaderNone, hidden.timelineState.reader)
}

func TestTimelineMeetingReaderSearchAndBackPreserveMeetings(t *testing.T) {
    detail := &query.MessageDetail{ID: 55, Subject: "Example planning", BodyText: "First line\n\nneedle in archived transcript"}
    m := NewBuilder().WithDetail(detail).WithSize(100, 24).Build()
    m.mode = modeTimeline
    m.timelineBackend = &timelineModeBackend{}
    m.timelineState.result = timelineModeFixture()
    m.timelineState.result.Days[0].Bursts = nil
    m.timelineState.result.Days[0].Meetings = []timeline.Entry{{
        Kind: "transcript", Ref: "msg:55", AnchorRef: "msg:55", Time: m.timelineState.result.Now,
        Meeting: &timeline.MeetingEntry{Title: timeline.UntrustedText{Text: "Example planning"}},
        Open: []timeline.Open{{Tool: "get_meeting_context", Args: map[string]any{"message_ids": []int64{55}}}},
    }}
    m.timelineState.request = timeline.Request{Mode: timeline.ModeDay, Date: "2027-03-14", Format: "json"}
    m.rebuildTimelineRows()
    parked := &query.MessageDetail{ID: 200, Subject: "Parked Meetings reader"}
    m.meetingState.level = meetingLevelDetail
    m.meetingState.detail = parked
    m.meetingState.detailScroll = 3
    m.meetingState.detailSearchInput.SetValue("parked search")
    m.meetingState.detailSearchQuery = "parked search"
    m.meetingState.detailSearchMatches = []int{2}
    m.meetingState.detailSearchMatchIndex = 0
    before := m.meetingState

    opened, cmd := sendKey(t, m, keyEnter())
    require.NotNil(t, cmd)
    assert.Equal(t, timelineReaderMeeting, opened.timelineState.reader)
    assert.Equal(t, meetingLevelDetail, opened.timelineState.meeting.level)
    loaded := sendMsg(t, opened, cmd())
    require.NotNil(t, loaded.timelineState.meeting.detail)
    assert.Equal(t, int64(55), loaded.timelineState.meeting.detail.ID)
    assert.Contains(t, stripANSI(loaded.renderView()), "needle in archived transcript")
    searching, focus := sendKey(t, loaded, key('/'))
    require.NotNil(t, focus)
    assert.True(t, searching.timelineState.meeting.detailSearchActive)
    searching.timelineState.meeting.detailSearchInput.SetValue("needle")
    matched, _ := sendKey(t, searching, keyEnter())
    assert.False(t, matched.timelineState.meeting.detailSearchActive)
    assert.Equal(t, "needle", matched.timelineState.meeting.detailSearchQuery)
    assert.NotEmpty(t, matched.timelineState.meeting.detailSearchMatches)
    back, _ := sendKey(t, matched, keyEsc())
    assert.Equal(t, timelineReaderNone, back.timelineState.reader)
    assert.Equal(t, 0, back.timelineState.cursor)
    assert.Equal(t, before.level, back.meetingState.level)
    assert.Same(t, parked, back.meetingState.detail)
    assert.Equal(t, before.detailScroll, back.meetingState.detailScroll)
    assert.Equal(t, before.detailSearchInput.Value(), back.meetingState.detailSearchInput.Value())
    assert.Equal(t, before.detailSearchQuery, back.meetingState.detailSearchQuery)
    assert.Equal(t, before.detailSearchMatches, back.meetingState.detailSearchMatches)
}

func TestTimelineBackAfterRapidRequestsRestoresSettledDay(t *testing.T) {
    m := timelineModeModel()
    originalRequest, originalResult := m.timelineState.request, m.timelineState.result
    first := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeDay, Date: "2027-03-15"}, true)
    require.NotNil(t, first)
    _ = first() // Produce a result, but do not deliver it to Update.
    second := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeDay, Date: "2027-03-16"}, true)
    require.NotNil(t, second)
    _ = second() // Neither request has settled in the model.
    backend := m.timelineBackend.(*timelineModeBackend)
    require.Len(t, backend.contexts, 2)
    assert.ErrorIs(t, backend.contexts[0].Err(), context.Canceled)
    require.Len(t, m.timelineState.history, 1)
    back, _ := sendKey(t, m, keyEsc())
    assert.ErrorIs(t, backend.contexts[1].Err(), context.Canceled)
    assert.Equal(t, originalRequest, back.timelineState.request)
    assert.Same(t, originalResult, back.timelineState.result)
    assert.Empty(t, back.timelineState.history)
    assert.False(t, back.timelineState.needsLoad)
    assert.False(t, back.timelineState.loading)
    assert.Equal(t, 0, back.timelineState.cursor)
}
