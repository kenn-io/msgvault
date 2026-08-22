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
			MessageType:    "whatsapp",
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
		{ID: 40, ConversationID: 701, Snippet: "older preview", MessageType: "whatsapp"},
		{ID: 41, ConversationID: 701, Snippet: "another preview", MessageType: "whatsapp"},
		{ID: 42, ConversationID: 701, Snippet: "short preview", MessageType: "whatsapp"},
		{ID: 43, ConversationID: 701, Snippet: "newer preview", MessageType: "whatsapp"},
	}
	model.textState.cursor = 2
	model.textState.scrollOffset = 1
	return model, requested
}

func TestTextTimelineOpensMessageDetail(t *testing.T) {
	model, requested := newTextTimelineDetailModel(t)

	model, cmd := sendKey(t, model, keyEnter())
	require.NotNil(t, cmd)
	model = sendMsg(t, model, cmd())
	assert.Equal(t, []int64{42}, *requested)
	assert.NotEqual(t, textLevelTimeline, model.textState.level)
	view := stripANSI(model.renderTextView())
	assert.Contains(t, view, "distinct full body needle")
	assert.NotContains(t, view, "short preview")

	model, _ = sendKey(t, model, key('/'))
	assert.True(t, model.detailSearchActive)
	model.detailSearchInput.SetValue("full body")
	model, _ = sendKey(t, model, keyEnter())
	assert.Equal(t, "full body", model.detailSearchQuery)
	assert.NotEmpty(t, model.detailSearchMatches)

	model, _ = sendKey(t, model, keyEsc())
	assert.Empty(t, model.detailSearchQuery, "first Esc clears detail search")
	model, cmd = sendKey(t, model, keyEsc())
	assert.Nil(t, cmd)
	assert.Equal(t, textLevelTimeline, model.textState.level)
	assert.Equal(t, 2, model.textState.cursor)
	assert.Equal(t, 1, model.textState.scrollOffset)
	assert.Equal(t, int64(42), model.textState.messages[model.textState.cursor].ID)
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
