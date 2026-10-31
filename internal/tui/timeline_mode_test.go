package tui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/timeline"
)

type timelineModeBackend struct {
	requests []timeline.Request
	contexts []context.Context
	result   *timeline.Timeline
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
				People:  []timeline.Person{{PersonID: 42, Name: "Example Contact"}},
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
	assert := assert.New(t)
	require := require.New(t)
	assert.Equal(modeTimeline, nextMode(modePeople, true, true, true))
	assert.Equal(modeTimeline, nextMode(modeMeetings, false, false, true))
	assert.Equal(modePeople, nextMode(modeMeetings, false, true, true))
	assert.Equal(modeEmail, nextMode(modeTimeline, true, true, true))
	assert.Equal(modeEmail, nextMode(modeMeetings, false, false, false))
	m := NewBuilder().Build()
	m.mode = modeMeetings
	m.timelineBackend = &timelineModeBackend{}
	updated, _, handled := m.handleGlobalKeys(key('m'))
	require.True(handled)
	assert.Equal(modeTimeline, updated.mode)
}
func TestTimelineKeysMoveDaysAndSwitchModes(t *testing.T) {
	for _, tc := range []struct {
		key                rune
		mode               timeline.Mode
		date, around, with string
	}{
		{key: '[', mode: timeline.ModeDay, date: "2027-03-13"},
		{key: ']', mode: timeline.ModeDay, date: "2027-03-15"},
		{key: 'a', mode: timeline.ModeAnchor, around: "burst:1:7:2027-03-14"},
		{key: 'w', mode: timeline.ModeRelationship, with: "person:42"},
	} {
		t.Run(string(tc.key), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			m := timelineModeModel()
			updated, cmd := sendKey(t, m, key(tc.key))
			require.NotNil(cmd)
			assert.Equal(tc.mode, updated.timelineState.request.Mode)
			assert.Equal(tc.date, updated.timelineState.request.Date)
			assert.Equal(tc.around, updated.timelineState.request.Around)
			assert.Equal(tc.with, updated.timelineState.request.With)
			assert.Equal("json", updated.timelineState.request.Format)
			returned, _ := sendKey(t, updated, keyEsc())
			assert.Equal(m.timelineState.result, returned.timelineState.result)
			assert.Equal(m.timelineState.request, returned.timelineState.request)
		})
	}
}
func TestTimelineModeLocalKeysWinOverGlobalA(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := timelineModeModel()
	updated, cmd := sendKey(t, m, key('a'))
	require.NotNil(cmd)
	assert.Equal(timeline.ModeAnchor, updated.timelineState.request.Mode)
	assert.Equal(modalNone, updated.modal)
	assert.Equal(allSourceScope(), updated.sourceScope)
	for _, k := range []tea.KeyPressMsg{key('?'), key('m')} {
		next, _ := sendKey(t, m, k)
		if k.String() == "?" {
			assert.Equal(modalHelp, next.modal)
		} else {
			assert.Equal(modeEmail, next.mode)
		}
	}
}
func TestTimelineCancelsAndDiscardsStaleResults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := timelineModeModel()
	first := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeDay, Date: "2027-03-13"}, true)
	require.NotNil(first)
	old := first().(timelineLoadedMsg)
	backend := m.timelineBackend.(*timelineModeBackend)
	require.Len(backend.contexts, 1)
	second := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeDay, Date: "2027-03-15"}, true)
	require.NotNil(second)
	assert.ErrorIs(backend.contexts[0].Err(), context.Canceled)
	m = sendMsg(t, m, old)
	assert.True(m.timelineState.loading)
	assert.Equal("2027-03-15", m.timelineState.request.Date)
	active := second().(timelineLoadedMsg)
	m = sendMsg(t, m, active)
	assert.False(m.timelineState.loading)
	assert.Equal(backend.result, m.timelineState.result)
	third := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeAnchor, Around: "msg:9"}, true)
	late := third().(timelineLoadedMsg)
	next, _, handled := m.handleGlobalKeys(key('m'))
	require.True(handled)
	assert.ErrorIs(backend.contexts[2].Err(), context.Canceled)
	before := next.timelineState.result
	next = sendMsg(t, next, late)
	assert.Equal(before, next.timelineState.result)
	assert.Equal(modeEmail, next.mode)
}
func TestTimelineReadersPreserveOtherModes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := timelineModeModel()
	emailDetail := &query.MessageDetail{ID: 100, Subject: "Email reader"}
	m.parkedMessageReaders[modeEmail] = messageReaderState{messageDetail: emailDetail, detailScroll: 4, detailSearchQuery: "saved"}
	m.timelineState.result.Days[0].Bursts[0].Open = []timeline.Open{{Tool: "get_message", Args: map[string]any{"id": int64(101)}}}
	m.rebuildTimelineRows()
	opened, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	assert.Equal(timelineReaderMessage, opened.timelineState.reader)
	loaded := sendMsg(t, opened, timelineReaderLoadedMsg{
		requestID: opened.timelineState.requestID, generation: opened.presentationGeneration,
		detail: &query.MessageDetail{ID: 101, Subject: "Timeline reader"},
	})
	require.NotNil(loaded.messageDetail)
	assert.Contains(stripANSI(loaded.renderView()), "Timeline reader")
	back, _ := sendKey(t, loaded, keyEsc())
	assert.Equal(timelineReaderNone, back.timelineState.reader)
	assert.Equal(0, back.timelineState.cursor)
	email, _, handled := back.handleGlobalKeys(key('m'))
	require.True(handled)
	assert.Same(emailDetail, email.messageDetail)
	assert.Equal(4, email.detailScroll)
	assert.Equal("saved", email.detailSearchQuery)
}
func TestTimelineOpenUsesEmittedTargets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := timelineModeModel()
	opened, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	assert.Equal(timelineReaderThread, opened.timelineState.reader)
	assert.Equal(int64(7), opened.timelineState.readerID)
	m.timelineState.result.Days[0].Bursts[0].HiddenText = true
	m.rebuildTimelineRows()
	hidden, cmd := sendKey(t, m, keyEnter())
	assert.Nil(cmd)
	assert.Equal(timelineReaderNone, hidden.timelineState.reader)
}

func TestTimelineMeetingReaderSearchAndBackPreserveMeetings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	detail := &query.MessageDetail{ID: 55, Subject: "Example planning", BodyText: "First line\n\nneedle in archived transcript"}
	m := NewBuilder().WithDetail(detail).WithSize(100, 24).Build()
	m.mode = modeTimeline
	m.timelineBackend = &timelineModeBackend{}
	m.timelineState.result = timelineModeFixture()
	m.timelineState.result.Days[0].Bursts = nil
	m.timelineState.result.Days[0].Meetings = []timeline.Entry{{
		Kind: "transcript", Ref: "msg:55", AnchorRef: "msg:55", Time: m.timelineState.result.Now,
		Meeting: &timeline.MeetingEntry{Title: timeline.UntrustedText{Text: "Example planning"}},
		Open:    []timeline.Open{{Tool: "get_meeting_context", Args: map[string]any{"message_ids": []int64{55}}}},
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
	require.NotNil(cmd)
	assert.Equal(timelineReaderMeeting, opened.timelineState.reader)
	assert.Equal(meetingLevelDetail, opened.timelineState.meeting.level)
	loaded := sendMsg(t, opened, cmd())
	require.NotNil(loaded.timelineState.meeting.detail)
	assert.Equal(int64(55), loaded.timelineState.meeting.detail.ID)
	assert.Contains(stripANSI(loaded.renderView()), "needle in archived transcript")
	searching, focus := sendKey(t, loaded, key('/'))
	require.NotNil(focus)
	assert.True(searching.timelineState.meeting.detailSearchActive)
	searching.timelineState.meeting.detailSearchInput.SetValue("needle")
	matched, _ := sendKey(t, searching, keyEnter())
	assert.False(matched.timelineState.meeting.detailSearchActive)
	assert.Equal("needle", matched.timelineState.meeting.detailSearchQuery)
	assert.NotEmpty(matched.timelineState.meeting.detailSearchMatches)
	back, _ := sendKey(t, matched, keyEsc())
	assert.Equal(timelineReaderNone, back.timelineState.reader)
	assert.Equal(0, back.timelineState.cursor)
	assert.Equal(before.level, back.meetingState.level)
	assert.Same(parked, back.meetingState.detail)
	assert.Equal(before.detailScroll, back.meetingState.detailScroll)
	assert.Equal(before.detailSearchInput.Value(), back.meetingState.detailSearchInput.Value())
	assert.Equal(before.detailSearchQuery, back.meetingState.detailSearchQuery)
	assert.Equal(before.detailSearchMatches, back.meetingState.detailSearchMatches)
}

func TestTimelineBackAfterRapidRequestsRestoresSettledDay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	m := timelineModeModel()
	originalRequest, originalResult := m.timelineState.request, m.timelineState.result
	first := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeDay, Date: "2027-03-15"}, true)
	require.NotNil(first)
	_ = first() // Produce a result, but do not deliver it to Update.
	second := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeDay, Date: "2027-03-16"}, true)
	require.NotNil(second)
	_ = second() // Neither request has settled in the model.
	backend := m.timelineBackend.(*timelineModeBackend)
	require.Len(backend.contexts, 2)
	assert.ErrorIs(backend.contexts[0].Err(), context.Canceled)
	require.Len(m.timelineState.history, 1)
	back, _ := sendKey(t, m, keyEsc())
	assert.ErrorIs(backend.contexts[1].Err(), context.Canceled)
	assert.Equal(originalRequest, back.timelineState.request)
	assert.Same(originalResult, back.timelineState.result)
	assert.Empty(back.timelineState.history)
	assert.False(back.timelineState.needsLoad)
	assert.False(back.timelineState.loading)
	assert.Equal(0, back.timelineState.cursor)
}

func TestTimelineNextPagePreservesRequestAndBackRestoresSelection(t *testing.T) {
	for _, mode := range []timeline.Mode{timeline.ModeDay, timeline.ModeRelationship, timeline.ModeAnchor} {
		t.Run(string(mode), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			m := timelineModeModel()
			firstPage := timelineModeFixture()
			firstPage.Cursor = "opaque-next-page"
			extra := firstPage.Days[0].Bursts[0]
			extra.Ref, extra.AnchorRef = "conv:8", "burst:1:8:2027-03-14"
			firstPage.Days[0].Bursts = append(firstPage.Days[0].Bursts, extra)
			backend := m.timelineBackend.(*timelineModeBackend)
			backend.result = firstPage
			request := timeline.Request{Mode: mode, Tiers: []int{1, 2}, IncludeCold: true, IncludeNoise: true, Budget: 4096, Format: "json"}
			switch mode {
			case timeline.ModeDay:
				request.Date = "2027-03-14"
			case timeline.ModeRelationship:
				request.With, request.From, request.To = "person:42", "2027-03-12", "2027-03-14"
				request.Brief = true
			case timeline.ModeAnchor:
				request.Around, request.Before, request.After = "msg:90", 2, 3
				request.BeforeSet, request.AfterSet, request.Conversation = true, true, true
			}
			firstPage.Mode = mode
			cmd := m.startTimelineRequest(request, false)
			require.NotNil(cmd)
			m = sendMsg(t, m, cmd())
			m.timelineState.cursor, m.timelineState.scroll = 1, 2
			beforeLines := append([]string(nil), m.timelineState.lines...)
			assert.Contains(stripANSI(m.renderView()), "n next page")
			secondPage := timelineModeFixture()
			secondPage.Days[0].Bursts[0].Ref = "conv:9"
			secondPage.Days[0].Bursts[0].AnchorRef = "burst:1:9:2027-03-14"
			backend.result = secondPage
			next, cmd := sendKey(t, m, key('n'))
			require.NotNil(cmd)
			next = sendMsg(t, next, cmd())
			require.Len(backend.requests, 2)
			request.Cursor = "opaque-next-page"
			assert.Equal(request, backend.requests[1])
			require.Len(next.timelineState.entries, 1)
			assert.Equal("conv:9", next.highlightedTimelineEntry().Ref)
			assert.NotContains(stripANSI(next.renderView()), "n next page")
			back, cmd := sendKey(t, next, keyEsc())
			assert.Nil(cmd)
			request.Cursor = ""
			assert.Equal(request, back.timelineState.request)
			assert.Same(firstPage, back.timelineState.result)
			assert.Equal(1, back.timelineState.cursor)
			assert.Equal(2, back.timelineState.scroll)
			assert.Equal(beforeLines, back.timelineState.lines)
			assert.Equal("conv:8", back.highlightedTimelineEntry().Ref)
		})
	}
}

func TestTimelineNextPageWithoutCursorDoesNothing(t *testing.T) {
	assert := assert.New(t)
	m := timelineModeModel()
	next, cmd := sendKey(t, m, key('n'))
	assert.Nil(cmd)
	assert.Equal(m.timelineState.request, next.timelineState.request)
	assert.Empty(next.timelineState.history)
}

func TestTimelineBurstOpensExactAnchorBeyondThreadLimit(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	m := timelineModeModel()
	m.threadMessageLimit = 2
	var requested []int64
	engine := &querytest.MockEngine{ListResults: []query.MessageSummary{
		{ID: 1, ConversationID: 7, Subject: "Oldest"},
		{ID: 2, ConversationID: 7, Subject: "Older"},
		{ID: 3, ConversationID: 7, Subject: "Selected recent anchor"},
	}}
	engine.GetMessageFunc = func(_ context.Context, id int64) (*query.MessageDetail, error) {
		requested = append(requested, id)
		return &query.MessageDetail{ID: id, ConversationID: 7, Subject: "Selected recent anchor", BodyText: "Exact selected body"}, nil
	}
	m.engine = engine
	m.timelineState.result.Days[0].Bursts[0].Open = []timeline.Open{
		{Tool: "list_messages", Args: map[string]any{"conversation_id": int64(7)}},
		{Tool: "get_message", Args: map[string]any{"id": int64(3)}},
	}
	m.rebuildTimelineRows()
	opened, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	loaded := sendMsg(t, opened, cmd())
	assert.Equal([]int64{3}, requested)
	assert.Equal(timelineReaderMessage, loaded.timelineState.reader)
	require.NotNil(loaded.messageDetail)
	assert.Equal(int64(3), loaded.messageDetail.ID)
	assert.Contains(stripANSI(loaded.renderView()), "Exact selected body")
}

func TestTimelineNextPageBackCancelsAndRejectsLateResponse(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	m := timelineModeModel()
	m.timelineState.result.Cursor = "opaque-next-page"
	firstPage, firstRequest := m.timelineState.result, m.timelineState.request
	backend := m.timelineBackend.(*timelineModeBackend)
	next, cmd := sendKey(t, m, key('n'))
	require.NotNil(cmd)
	late := cmd()
	pending, duplicate := sendKey(t, next, key('n'))
	assert.Nil(duplicate, "a pending page has no continuation to follow")
	back, _ := sendKey(t, pending, keyEsc())
	require.Len(backend.contexts, 1)
	assert.ErrorIs(backend.contexts[0].Err(), context.Canceled)
	back = sendMsg(t, back, late)
	assert.Same(firstPage, back.timelineState.result)
	assert.Equal(firstRequest, back.timelineState.request)
	assert.False(back.timelineState.loading)
}
