package tui

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
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
	old, ok := first().(timelineLoadedMsg)
	require.True(ok)
	backend, ok := m.timelineBackend.(*timelineModeBackend)
	require.True(ok)
	require.Len(backend.contexts, 1)
	second := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeDay, Date: "2027-03-15"}, true)
	require.NotNil(second)
	require.ErrorIs(backend.contexts[0].Err(), context.Canceled)
	m = sendMsg(t, m, old)
	assert.True(m.timelineState.loading)
	assert.Equal("2027-03-15", m.timelineState.request.Date)
	active, ok := second().(timelineLoadedMsg)
	require.True(ok)
	m = sendMsg(t, m, active)
	assert.False(m.timelineState.loading)
	assert.Equal(backend.result, m.timelineState.result)
	third := m.startTimelineRequest(timeline.Request{Mode: timeline.ModeAnchor, Around: "msg:9"}, true)
	late, ok := third().(timelineLoadedMsg)
	require.True(ok)
	next, _, handled := m.handleGlobalKeys(key('m'))
	require.True(handled)
	require.ErrorIs(backend.contexts[2].Err(), context.Canceled)
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
	m.timelineState.result.Days[0].Bursts[0].Open[0].Args["conversation_id"] = jsontext.Value("9007199254740993")
	m.rebuildTimelineRows()
	opened, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	assert.Equal(timelineReaderThread, opened.timelineState.reader)
	assert.Equal(int64(9007199254740993), opened.timelineState.readerID)
	m.timelineState.result.Days[0].Bursts[0].HiddenText = true
	m.rebuildTimelineRows()
	hidden, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	assert.Equal(timelineReaderThread, hidden.timelineState.reader)
	assert.Equal(int64(9007199254740993), hidden.timelineState.readerID)
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
	backend, ok := m.timelineBackend.(*timelineModeBackend)
	require.True(ok)
	require.Len(backend.contexts, 2)
	require.ErrorIs(backend.contexts[0].Err(), context.Canceled)
	require.Len(m.timelineState.history, 1)
	back, _ := sendKey(t, m, keyEsc())
	require.ErrorIs(backend.contexts[1].Err(), context.Canceled)
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
			backend, ok := m.timelineBackend.(*timelineModeBackend)
			require.True(ok)
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
	backend, ok := m.timelineBackend.(*timelineModeBackend)
	require.True(ok)
	next, cmd := sendKey(t, m, key('n'))
	require.NotNil(cmd)
	late := cmd()
	pending, duplicate := sendKey(t, next, key('n'))
	assert.Nil(duplicate, "a pending page has no continuation to follow")
	back, _ := sendKey(t, pending, keyEsc())
	require.Len(backend.contexts, 1)
	require.ErrorIs(backend.contexts[0].Err(), context.Canceled)
	back = sendMsg(t, back, late)
	assert.Same(firstPage, back.timelineState.result)
	assert.Equal(firstRequest, back.timelineState.request)
	assert.False(back.timelineState.loading)
}

func TestTimelineMeetingSearchAcceptsTextAndCursorEditing(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	m := timelineModeModel()
	detail := &query.MessageDetail{ID: 55, Subject: "Archived transcript", BodyText: "hello and helxlyo"}
	m.engine = newMockEngine(MockConfig{Detail: detail})
	m.timelineState.result.Days[0].Bursts = nil
	m.timelineState.result.Days[0].Meetings = []timeline.Entry{{Kind: "transcript", Ref: "msg:55", AnchorRef: "msg:55",
		Meeting: &timeline.MeetingEntry{Title: timeline.UntrustedText{Text: "Archived transcript"}},
		Open:    []timeline.Open{{Tool: "get_meeting_context", Args: map[string]any{"message_ids": []int64{55}}}}}}
	m.rebuildTimelineRows()
	parked := m.meetingState
	m, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	m = sendMsg(t, m, cmd())
	m, _ = sendKey(t, m, key('/'))
	for _, r := range "hello" {
		m, _ = sendKey(t, m, key(r))
	}
	assert.Equal("hello", m.timelineState.meeting.detailSearchInput.Value())
	lines := strings.Split(stripANSI(m.renderView()), "\n")
	footer := lines[len(lines)-1]
	assert.Contains(footer, "hello")
	assert.Contains(footer, "Enter find")
	assert.NotContains(footer, "Enter open")
	m, _ = sendKey(t, m, keyLeft())
	m, _ = sendKey(t, m, keyLeft())
	m, _ = sendKey(t, m, key('x'))
	m, _ = sendKey(t, m, keyRight())
	m, _ = sendKey(t, m, key('y'))
	assert.Equal("helxlyo", m.timelineState.meeting.detailSearchInput.Value())
	lines = strings.Split(stripANSI(m.renderView()), "\n")
	assert.Contains(lines[len(lines)-1], "helxlyo")
	m, _ = sendKey(t, m, keyEnter())
	assert.Equal("helxlyo", m.timelineState.meeting.detailSearchQuery)
	assert.NotEmpty(m.timelineState.meeting.detailSearchMatches)
	assert.NotContains(stripANSI(m.renderView()), "[Find]/")
	m, _ = sendKey(t, m, key('/'))
	assert.Contains(stripANSI(m.renderView()), "[Find]/")
	m, _ = sendKey(t, m, keyEsc())
	assert.NotContains(stripANSI(m.renderView()), "[Find]/")
	assert.Equal(timelineReaderMeeting, m.timelineState.reader)
	m, _ = sendKey(t, m, keyEsc())
	assert.Equal(timelineReaderNone, m.timelineState.reader)
	assert.Equal("msg:55", m.highlightedTimelineEntry().Ref)
	assert.Equal(parked, m.meetingState)
}

type timelineCalendarFixtureBackend struct {
	timelineModeBackend

	calendarRequests []string
	calendarContexts []context.Context
	calendarDetail   store.CalendarOccurrenceDetail
	calendarErr      error
}

func (b *timelineCalendarFixtureBackend) GetCalendarOccurrenceContext(ctx context.Context, ref string) (store.CalendarOccurrenceDetail, error) {
	b.calendarRequests = append(b.calendarRequests, ref)
	b.calendarContexts = append(b.calendarContexts, ctx)
	return b.calendarDetail, b.calendarErr
}

func timelineCalendarModel(ref string) (Model, *timelineCalendarFixtureBackend) {
	m := timelineModeModel()
	backend := &timelineCalendarFixtureBackend{calendarDetail: store.CalendarOccurrenceDetail{
		CalendarOccurrence: store.CalendarOccurrence{Ref: ref, MessageID: 42, Subject: "Archived planning", Status: "confirmed",
			Start: m.timelineState.result.Now, End: m.timelineState.result.Now.Add(time.Hour),
			Attendees: []calevent.Attendee{{DisplayName: "Example Guest", Email: "guest@example.com", RSVP: "accepted"}}},
		Content: calevent.Content{Location: "Example room", JoinURL: "https://example.com/meeting", Description: "Archived agenda\x1b]52;c;unsafe\x07"},
	}}
	m.timelineBackend = backend
	m.timelineState.result.Days[0].Bursts = nil
	m.timelineState.result.Days[0].Meetings = []timeline.Entry{{Kind: "meeting", Ref: ref, AnchorRef: ref,
		Meeting: &timeline.MeetingEntry{Title: timeline.UntrustedText{Text: "Archived planning"}},
		Open:    []timeline.Open{{Tool: "get_calendar_event", Args: map[string]any{"ref": ref}}}}}
	m.rebuildTimelineRows()
	return m, backend
}

func TestTimelineCalendarOpensExactOccurrenceAndReturns(t *testing.T) {
	for _, ref := range []string{"evt:42", "evt:42@20270314T083000.123456789Z", "evt:42@20270314"} {
		t.Run(ref, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			m, backend := timelineCalendarModel(ref)
			result, request := m.timelineState.result, m.timelineState.request
			opened, cmd := sendKey(t, m, keyEnter())
			require.NotNil(cmd)
			loaded := sendMsg(t, opened, cmd())
			assert.Equal([]string{ref}, backend.calendarRequests)
			view := loaded.renderView()
			for _, text := range []string{"Archived calendar occurrence", ref, "Archived planning", "Example room", "guest@example.com", "accepted", "https://example.com/meeting", "Archived agenda"} {
				assert.Contains(stripANSI(view), text)
			}
			assert.NotContains(view, "\x1b]52;")
			assert.Nil(loaded.messageDetail, "calendar occurrence must not masquerade as a message")
			back, cmd := sendKey(t, loaded, keyEsc())
			assert.Nil(cmd)
			assert.Equal(timelineReaderNone, back.timelineState.reader)
			assert.Same(result, back.timelineState.result)
			assert.Equal(request, back.timelineState.request)
			assert.Equal(0, back.timelineState.cursor)
		})
	}
}

func TestTimelineCalendarReaderScrollsWithinBounds(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	m, backend := timelineCalendarModel("evt:42")
	backend.calendarDetail.Description = strings.Repeat("Agenda line\n", 60) + "Last agenda line"
	opened, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	loaded := sendMsg(t, opened, cmd())
	assert.NotContains(stripANSI(loaded.renderView()), "Last agenda line")
	end, _ := sendKey(t, loaded, key('G'))
	assert.Contains(stripANSI(end.renderView()), "Last agenda line")
	again, _ := sendKey(t, end, key('j'))
	assert.Equal(stripANSI(end.renderView()), stripANSI(again.renderView()))
	home, _ := sendKey(t, again, tea.KeyPressMsg{Code: tea.KeyHome})
	assert.Equal(stripANSI(loaded.renderView()), stripANSI(home.renderView()))
}

func TestTimelineLinkedCalendarStillOpensTranscript(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	m, backend := timelineCalendarModel("evt:42")
	m.engine = newMockEngine(MockConfig{Detail: &query.MessageDetail{ID: 55, BodyText: "Linked transcript body"}})
	entry := &m.timelineState.result.Days[0].Meetings[0]
	entry.Open = append(entry.Open, timeline.Open{Tool: "get_meeting_context", Args: map[string]any{"message_ids": []int64{55}}})
	m.rebuildTimelineRows()
	opened, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	loaded := sendMsg(t, opened, cmd())
	assert.Empty(backend.calendarRequests)
	assert.Equal(timelineReaderMeeting, loaded.timelineState.reader)
	assert.Contains(stripANSI(loaded.renderView()), "Linked transcript body")
}

func (b *timelineModeBackend) GetCalendarOccurrenceContext(context.Context, string) (store.CalendarOccurrenceDetail, error) {
	return store.CalendarOccurrenceDetail{}, errors.New("calendar fixture not configured")
}

func TestTimelineCalendarRetryKeepsExactRef(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	m, backend := timelineCalendarModel("evt:42@20270314T083000.123456789Z")
	backend.calendarErr = errors.New("calendar temporarily unavailable\x1b]52;c;unsafe\x07")
	opened, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	failed := sendMsg(t, opened, cmd())
	assert.Contains(stripANSI(failed.renderView()), "r retry")
	assert.Contains(stripANSI(failed.renderView()), "calendar temporarily unavailable")
	assert.NotContains(failed.renderView(), "\x1b]52;")
	backend.calendarErr = nil
	retry, cmd := sendKey(t, failed, key('r'))
	require.NotNil(cmd)
	loaded := sendMsg(t, retry, cmd())
	assert.Equal([]string{"evt:42@20270314T083000.123456789Z", "evt:42@20270314T083000.123456789Z"}, backend.calendarRequests)
	assert.Contains(stripANSI(loaded.renderView()), "Archived planning")
	assert.NoError(loaded.timelineState.err)
}

func TestTimelineCalendarBackCancelsAndRejectsLateResult(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	m, backend := timelineCalendarModel("evt:42")
	opened, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	late := cmd()
	back, _ := sendKey(t, opened, keyEsc())
	require.Len(backend.calendarContexts, 1)
	require.ErrorIs(backend.calendarContexts[0].Err(), context.Canceled)
	back = sendMsg(t, back, late)
	assert.Equal(timelineReaderNone, back.timelineState.reader)
	assert.Nil(back.timelineState.calendar)
	assert.False(back.loading)
	assert.Equal("evt:42", back.highlightedTimelineEntry().Ref)
}

func TestTimelineCalendarModeExitCancelsAndReentryResumes(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	m, backend := timelineCalendarModel("evt:42@20270314T083000Z")
	opened, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	late := cmd()
	away, _ := sendKey(t, opened, key('m'))
	require.Len(backend.calendarContexts, 1)
	require.ErrorIs(backend.calendarContexts[0].Err(), context.Canceled)
	away = sendMsg(t, away, late)
	assert.Nil(away.timelineState.calendar)
	require.Equal(modeEmail, away.mode)
	meetings, _ := sendKey(t, away, key('m'))
	require.Equal(modeMeetings, meetings.mode)
	resumed, cmd := sendKey(t, meetings, key('m'))
	require.Equal(modeTimeline, resumed.mode)
	require.NotNil(cmd)
	loaded := sendMsg(t, resumed, cmd())
	assert.Equal([]string{"evt:42@20270314T083000Z", "evt:42@20270314T083000Z"}, backend.calendarRequests)
	assert.Contains(stripANSI(loaded.renderView()), "Archived planning")
}

func TestTimelineCalendarRejectsMismatchedResponse(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	m, backend := timelineCalendarModel("evt:42")
	backend.calendarDetail.Ref = "evt:43"
	opened, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	failed := sendMsg(t, opened, cmd())
	require.ErrorContains(failed.timelineState.err, "mismatched occurrence")
	assert.NotContains(stripANSI(failed.renderView()), "Archived planning")
}

func TestTimelineCalendarMovedOccurrenceRetainsOriginalRef(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	const ref = "evt:42@20270314T083000.123456789Z"
	m, backend := timelineCalendarModel(ref)
	backend.calendarDetail.Start = time.Date(2027, 3, 15, 10, 0, 0, 0, time.UTC)
	backend.calendarDetail.End = backend.calendarDetail.Start.Add(time.Hour)
	opened, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	loaded := sendMsg(t, opened, cmd())
	assert.Equal([]string{ref}, backend.calendarRequests)
	view := stripANSI(loaded.renderView())
	assert.Contains(view, ref)
	assert.Contains(view, "Start: 2027-03-15T10:00:00Z")
	assert.Contains(view, "End: 2027-03-15T11:00:00Z")
}

func TestTimelineCalendarAllDayAndDeletedStatus(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	m, backend := timelineCalendarModel("evt:42@20270314")
	backend.calendarDetail.AllDay = true
	backend.calendarDetail.StartDate, backend.calendarDetail.EndDate = "2027-03-14", "2027-03-15"
	backend.calendarDetail.DeletedFromSource = true
	backend.calendarDetail.Status = "cancelled"
	opened, cmd := sendKey(t, m, keyEnter())
	require.NotNil(cmd)
	loaded := sendMsg(t, opened, cmd())
	view := stripANSI(loaded.renderView())
	assert.Contains(view, "All day: 2027-03-14 to 2027-03-15 (end date exclusive)")
	assert.Contains(view, "Status: cancelled")
	assert.Contains(view, "Deleted from source; showing the archived occurrence")
}

func TestTimelineDayKeysUseViewedWindow(t *testing.T) {
	for _, offset := range []int{-4, 2} {
		for _, tc := range []struct {
			key  rune
			date string
		}{{'[', "2027-03-13"}, {']', "2027-03-15"}} {
			t.Run(fmt.Sprintf("%c/%d", tc.key, offset), func(t *testing.T) {
				m := timelineModeModel()
				entry := m.timelineState.result.Days[0].Bursts[0]
				entry.Time = entry.Time.AddDate(0, 0, offset)
				m.timelineState.result.Days = nil
				m.timelineState.result.Upcoming = []timeline.Entry{entry}
				m.rebuildTimelineRows()
				updated, cmd := sendKey(t, m, key(tc.key))
				require.NotNil(t, cmd)
				defer updated.cancelTimelineRequest()
				assert.Equal(t, tc.date, updated.timelineState.request.Date)
			})
		}
	}
}

func TestTimelineReaderKeepsLocalNavigationKeys(t *testing.T) {
	for _, reader := range []timelineReader{timelineReaderMessage, timelineReaderThread, timelineReaderCalendar, timelineReaderMeeting} {
		for _, k := range []rune{'a', 'w', '[', ']'} {
			t.Run(fmt.Sprintf("%d/%c", reader, k), func(t *testing.T) {
				m := timelineModeModel()
				m.timelineState.reader = reader
				updated, cmd := sendKey(t, m, key(k))
				defer updated.cancelTimelineRequest()
				assert.Nil(t, cmd)
				assert.Equal(t, reader, updated.timelineState.reader)
				assert.Equal(t, m.timelineState.request, updated.timelineState.request)
			})
		}
	}
}

func TestTimelineDayKeysUseViewSubject(t *testing.T) {
	for _, mode := range []timeline.Mode{timeline.ModeAnchor, timeline.ModeRelationship} {
		for _, tc := range []struct {
			key  rune
			date string
		}{{'[', "2027-03-13"}, {']', "2027-03-15"}} {
			t.Run(string(mode)+string(tc.key), func(t *testing.T) {
				m := timelineModeModel()
				m.timelineState.result.Mode = mode
				m.timelineState.result.Window = timeline.Window{From: "2027-02-11", To: "2027-04-14"}
				m.timelineState.result.Anchor = &timeline.Anchor{Time: time.Date(2027, 3, 14, 8, 30, 0, 0, time.UTC)}
				updated, cmd := sendKey(t, m, key(tc.key))
				require.NotNil(t, cmd)
				assert.Equal(t, tc.date, updated.timelineState.request.Date)
			})
		}
	}
	m := timelineModeModel()
	m.timelineState.result.Mode = timeline.ModeRelationship
	m.timelineState.result.Now = time.Date(2027, 3, 14, 8, 30, 0, 0, time.UTC)
	m.timelineState.result.Days = nil
	m.timelineState.result.Window.From = "2027-03-12"
	m.rebuildTimelineRows()
	updated, cmd := sendKey(t, m, key(']'))
	require.NotNil(t, cmd)
	assert.Equal(t, "2027-03-15", updated.timelineState.request.Date)
}

func TestTimelineRelationshipKeySkipsUnresolvedPeople(t *testing.T) {
	for _, person := range []timeline.Person{{PersonID: 9}, {ParticipantID: 10}} {
		m := timelineModeModel()
		m.timelineState.result.Days[0].Bursts[0].Burst.People = []timeline.Person{{Name: "Unresolved"}, person}
		m.rebuildTimelineRows()
		updated, cmd := sendKey(t, m, key('w'))
		require.NotNil(t, cmd)
		want := "person:9"
		if person.ParticipantID > 0 {
			want = "participant:10"
		}
		assert.Equal(t, want, updated.timelineState.request.With)
	}
}
