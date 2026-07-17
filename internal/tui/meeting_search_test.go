package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/search"
)

func TestMeetingSearchScopesTranscriptAndSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	sourceID := int64(7)
	var captured *search.Query
	engine := &querytest.MockEngine{}
	engine.SearchFunc = func(_ context.Context, q *search.Query, _, _ int) ([]query.MessageSummary, error) {
		captured = q
		return []query.MessageSummary{{ID: 1, Subject: "Roadmap"}}, nil
	}
	model := New(engine, Options{})
	model.meetingState.sourceID = &sourceID

	msg := model.loadMeetingSearch("roadmap", 0, false)()

	loaded, ok := msg.(meetingSearchLoadedMsg)
	require.True(ok)
	require.NoError(loaded.err)
	require.NotNil(captured)
	assert.Equal([]string{meetingMessageType}, captured.MessageTypes)
	assert.Equal([]int64{sourceID}, captured.AccountIDs)
	assert.Equal([]string{"roadmap"}, captured.TextTerms)
}

func TestMeetingSearchCannotOverrideMeetingScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var captured *search.Query
	engine := &querytest.MockEngine{}
	engine.SearchFunc = func(_ context.Context, q *search.Query, _, _ int) ([]query.MessageSummary, error) {
		captured = q
		return nil, nil
	}
	model := New(engine, Options{})

	msg := model.loadMeetingSearch("message_type:email roadmap", 0, false)()

	loaded, ok := msg.(meetingSearchLoadedMsg)
	require.True(ok)
	require.NoError(loaded.err)
	require.NotNil(captured)
	assert.Equal([]string{meetingMessageType}, captured.MessageTypes)
	assert.NotNil(captured.AccountIDs, "conflicting scope must be match-nothing, not unscoped")
	assert.Empty(captured.AccountIDs)
}

func TestMeetingSlashStartsDeepSearch(t *testing.T) {
	model := NewBuilder().Build()
	model.mode = modeMeetings

	updated, cmd := sendKey(t, model, key('/'))

	assert.NotNil(t, cmd)
	assert.True(t, updated.meetingState.searchActive)
	assert.Equal(t, "search meetings and transcripts", updated.meetingState.searchInput.Placeholder)
}

func TestMeetingSearchEnterRunsQuery(t *testing.T) {
	assert := assert.New(t)
	model := NewBuilder().Build()
	model.mode = modeMeetings
	model.meetingState.searchActive = true
	model.meetingState.searchInput.SetValue("roadmap")

	updatedModel, cmd := model.handleMeetingKeyPress(tea.KeyPressMsg{Code: tea.KeyEnter})
	updated, ok := updatedModel.(Model)
	require.True(t, ok)

	assert.False(updated.meetingState.searchActive)
	assert.Equal("roadmap", updated.meetingState.searchQuery)
	assert.NotNil(cmd)
}

func TestMeetingEscapeClearsSearchAndRestoresList(t *testing.T) {
	assert := assert.New(t)
	model := NewBuilder().Build()
	model.mode = modeMeetings
	model.meetingState.searchQuery = "roadmap"
	model.meetingState.messages = []query.MessageSummary{{ID: 2, Subject: "Search result"}}
	model.meetingState.preSearch = []query.MessageSummary{{ID: 1, Subject: "Original meeting"}}

	updated, cmd := sendKey(t, model, keyEsc())

	assert.Nil(cmd)
	assert.Empty(updated.meetingState.searchQuery)
	require.Len(t, updated.meetingState.messages, 1)
	assert.Equal("Original meeting", updated.meetingState.messages[0].Subject)
}
