package tui

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func newTextTimelineDetailModel(t *testing.T) (Model, *[]int64) {
	t.Helper()
	requested := &[]int64{}
	engine := &querytest.MockEngine{}
	engine.GetMessageFunc = func(_ context.Context, id int64) (*query.MessageDetail, error) {
		*requested = append(*requested, id)
		return &query.MessageDetail{
			ID:             id,
			ConversationID: 701,
			Subject:        "Full detail",
			MessageType:    sourceTypeWhatsApp,
			SentAt:         time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC),
			BodyText:       "distinct full body needle",
		}, nil
	}
	model := New(engine, Options{TextEngine: meetingModeTextEngine{}})
	model.width = 100
	model.height = 24
	model.pageSize = 10
	model.loading = false
	model.mode = modeTexts
	model.presentationGeneration = 9
	model.textRequestID = 3
	model.textState.level = textLevelTimeline
	model.textState.selectedConvID = 701
	model.textState.messages = []query.MessageSummary{
		{ID: 40, ConversationID: 701, Snippet: "older preview", MessageType: sourceTypeWhatsApp},
		{ID: 41, ConversationID: 701, Snippet: "another preview", MessageType: sourceTypeWhatsApp},
		{ID: 42, ConversationID: 701, Snippet: "short preview", MessageType: sourceTypeWhatsApp},
		{ID: 43, ConversationID: 701, Snippet: "newer preview", MessageType: sourceTypeWhatsApp},
	}
	model.textState.cursor = 2
	model.textState.scrollOffset = 1
	return model, requested
}

func TestTextTimelineOpensMessageDetail(t *testing.T) {
	assert := assert.New(t)
	model, requested := newTextTimelineDetailModel(t)

	model, cmd := sendKey(t, model, keyEnter())
	require.NotNil(t, cmd)
	model = sendMsg(t, model, cmd())
	assert.Equal([]int64{42}, *requested)
	assert.NotEqual(textLevelTimeline, model.textState.level)
	view := stripANSI(model.renderTextView())
	assert.Contains(view, "distinct full body needle")
	assert.NotContains(view, "short preview")

	model, _ = sendKey(t, model, key('/'))
	assert.True(model.detailSearchActive)
	model.detailSearchInput.SetValue("full body")
	model, _ = sendKey(t, model, keyEnter())
	assert.Equal("full body", model.detailSearchQuery)
	assert.NotEmpty(model.detailSearchMatches)

	model, _ = sendKey(t, model, keyEsc())
	assert.Empty(model.detailSearchQuery, "first Esc clears detail search")
	model, cmd = sendKey(t, model, keyEsc())
	assert.Nil(cmd)
	assert.Equal(textLevelTimeline, model.textState.level)
	assert.Equal(2, model.textState.cursor)
	assert.Equal(1, model.textState.scrollOffset)
	assert.Equal(int64(42), model.textState.messages[model.textState.cursor].ID)
}

func TestTextDetailRejectsStaleIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Model)
	}{
		{name: "mode", mutate: func(model *Model) { model.mode = modeEmail }},
		{name: "conversation", mutate: func(model *Model) { model.textState.selectedConvID = 999 }},
		{name: "request", mutate: func(model *Model) { model.textRequestID++ }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model, _ := newTextTimelineDetailModel(t)
			model, cmd := sendKey(t, model, keyEnter())
			require.NotNil(t, cmd)
			loaded := cmd()
			tc.mutate(&model)
			model = sendMsg(t, model, loaded)
			assert.Nil(t, model.messageDetail)
		})
	}
}

type timelineSearchTextEngine struct {
	meetingModeTextEngine
}

func (timelineSearchTextEngine) ListConversationMessages(
	_ context.Context, conversationID int64, filter query.TextFilter,
) ([]query.MessageSummary, error) {
	if conversationID != 701 || filter.SearchQuery != "hiddenneedle" {
		return nil, nil
	}
	return []query.MessageSummary{{
		ID:             42,
		ConversationID: 701,
		Snippet:        "short preview",
		MessageType:    sourceTypeWhatsApp,
	}}, nil
}

func TestTextTimelineSearchUsesConversationFullTextResults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	model, _ := newTextTimelineDetailModel(t)
	model.textEngine = timelineSearchTextEngine{}

	model, _ = sendKey(t, model, key('/'))
	model.searchInput.SetValue("hiddenneedle")
	model, cmd := sendKey(t, model, keyEnter())
	require.NotNil(cmd)
	model = sendMsg(t, model, cmd())

	require.Len(model.textState.messages, 1)
	assert.Equal(int64(42), model.textState.messages[0].ID)
	assert.Equal("short preview", model.textState.messages[0].Snippet)
	assert.Empty(model.textState.messages[0].BodyText)
}

type globalSearchRefineTextEngine struct {
	meetingModeTextEngine

	searches          []string
	conversationLoads int
}

func (e *globalSearchRefineTextEngine) TextSearch(
	_ context.Context, searchQuery string, _, _ int,
) ([]query.MessageSummary, error) {
	e.searches = append(e.searches, searchQuery)
	return []query.MessageSummary{{ID: 77, Subject: "Refined global result"}}, nil
}

func (e *globalSearchRefineTextEngine) ListConversationMessages(
	context.Context, int64, query.TextFilter,
) ([]query.MessageSummary, error) {
	e.conversationLoads++
	return []query.MessageSummary{{ID: 88, Subject: "Wrong conversation result"}}, nil
}

func TestGlobalTextSearchRefinementStaysGlobal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := &globalSearchRefineTextEngine{}
	model, _ := newTextTimelineDetailModel(t)
	model.textEngine = engine
	model.textState.globalSearchTimeline = true
	model.textState.selectedConvID = 701 // stale conversation identity must not select the backend
	model.textState.breadcrumbs = []textNavSnapshot{{level: textLevelConversations}}

	model, _ = sendKey(t, model, key('/'))
	model.searchInput.SetValue("refined")
	model, cmd := sendKey(t, model, keyEnter())
	require.NotNil(cmd)
	model = sendMsg(t, model, cmd())

	assert.Equal([]string{"refined"}, engine.searches)
	assert.Zero(engine.conversationLoads)
	require.Len(model.textState.messages, 1)
	assert.Equal(int64(77), model.textState.messages[0].ID)
	assert.True(model.textState.globalSearchTimeline)
	assert.Len(model.textState.breadcrumbs, 1, "refinement must preserve the original search breadcrumb")
}
