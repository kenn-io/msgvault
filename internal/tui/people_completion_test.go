package tui

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
)

func (b *fakePeopleBackend) Complete(
	_ context.Context, request peoplebrowser.CompletionRequest,
) (*peoplebrowser.CompletionPage, error) {
	b.completionRequests = append(b.completionRequests, request)
	if len(b.completionErrs) > 0 {
		err := b.completionErrs[0]
		b.completionErrs = b.completionErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(b.completionPages) == 0 {
		return &peoplebrowser.CompletionPage{}, nil
	}
	page := b.completionPages[0]
	b.completionPages = b.completionPages[1:]
	return page, nil
}

func completionRow(id int64, label string, kind query.PeopleCompletionKind, value, source string) peoplebrowser.CompletionRow {
	return peoplebrowser.CompletionRow{
		ParticipantID: id, DisplayLabel: label, Kind: kind, Value: value, Source: source,
	}
}

func completionLoadedMessage(t *testing.T, cmd tea.Cmd) peopleCompletionLoadedMsg {
	t.Helper()
	for _, msg := range runBatchCommand(t, cmd) {
		if loaded, ok := msg.(peopleCompletionLoadedMsg); ok {
			return loaded
		}
	}
	require.FailNow(t, "command did not produce a people completion result")
	return peopleCompletionLoadedMsg{}
}

func TestPeopleSearchDebounceLoadsBoundedTypedCompletions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakePeopleBackend{
		searchPages: []*peoplebrowser.SearchPage{{Rows: []query.PersonSummary{testPerson(7, "Alice")}}},
		completionPages: []*peoplebrowser.CompletionPage{{
			Rows: []peoplebrowser.CompletionRow{
				completionRow(7, "Alice", query.PeopleCompletionEmail, "alice@example.test", "profile"),
			},
			CacheRevision: "cache-7",
		}},
	}
	model := peopleModel(backend)
	model.mode = modePeople
	model.presentationGeneration = 4
	model.peopleState.searchActive = true
	model.peopleState.searchInput.Focus()
	model.peopleState.requestID = 9
	model.peopleState.searchDebounceID = 3

	updated, cmd := model.Update(peopleSearchDebounceMsg{
		query: "alice@example", debounceID: 3, requestID: 9, presentationGeneration: 4,
	})
	model = asModel(t, updated)
	loaded := completionLoadedMessage(t, cmd)
	require.Len(backend.completionRequests, 1)
	assert.Equal(peoplebrowser.CompletionRequest{Query: "alice@example", Limit: peopleCompletionLimit}, backend.completionRequests[0])
	assert.Equal(uint64(9), loaded.requestID)
	assert.Equal("alice@example", loaded.query)

	model = sendMsg(t, model, loaded)
	require.Len(model.peopleState.completions, 1)
	assert.Equal("alice@example.test", model.peopleState.completions[0].Value)
	assert.Equal("cache-7", model.peopleState.completionCacheRevision)
	assert.False(model.peopleState.completionLoading)
}

func TestPeopleCompletionRejectsStaleAndBlankQueriesDoNotLoad(t *testing.T) {
	assert := assert.New(t)
	backend := &fakePeopleBackend{}
	model := peopleModel(backend)
	model.mode = modePeople
	model.presentationGeneration = 8
	model.peopleState.searchActive = true
	model.peopleState.requestID = 12
	model.peopleState.searchDebounceID = 5
	model.peopleState.completions = []peoplebrowser.CompletionRow{
		completionRow(2, "Old", query.PeopleCompletionName, "Old", "archive"),
	}

	model = sendMsg(t, model, peopleCompletionLoadedMsg{
		page: &peoplebrowser.CompletionPage{Rows: []peoplebrowser.CompletionRow{
			completionRow(3, "Stale", query.PeopleCompletionName, "Stale", "archive"),
		}},
		query: "stale", requestID: 11, presentationGeneration: 8,
	})
	assert.Equal("Old", model.peopleState.completions[0].DisplayLabel)

	updated, cmd := model.Update(peopleSearchDebounceMsg{
		query: "", debounceID: 5, requestID: 12, presentationGeneration: 8,
	})
	model = asModel(t, updated)
	assert.NotNil(cmd, "directory search still refreshes for a blank query")
	assert.Empty(backend.completionRequests)
	assert.Empty(model.peopleState.completions)
	assert.False(model.peopleState.completionLoading)
}

func TestPeopleSearchCompletionNavigationAcceptsAndOpensPerson(t *testing.T) {
	assert := assert.New(t)
	contact := testPerson(9, "Alice Example")
	backend := &fakePeopleBackend{contacts: map[int64]*query.PersonSummary{9: &contact}}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.initialized = true
	model.peopleState.searchActive = true
	model.peopleState.searchInput.Focus()
	model.peopleState.searchInput.SetValue("ali")
	model.peopleState.completions = []peoplebrowser.CompletionRow{
		completionRow(9, "Alice Example", query.PeopleCompletionName, "Alice Example", "profile"),
		completionRow(9, "Alice Example", query.PeopleCompletionPhone, "+1 415 555 0100", sourceTypeWhatsApp),
	}

	model, _ = sendKey(t, model, keyDown())
	assert.Equal(1, model.peopleState.completionCursor)
	model, cmd := sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal("+1 415 555 0100", model.peopleState.searchInput.Value())
	assert.NotNil(cmd)

	model.peopleState.completions = []peoplebrowser.CompletionRow{
		completionRow(9, "Alice Example", query.PeopleCompletionPhone, "+1 415 555 0100", sourceTypeWhatsApp),
	}
	model.peopleState.completionCursor = 0
	model, open := sendKey(t, model, keyEnter())
	assert.False(model.peopleState.searchActive)
	assert.Equal(peopleLevelContact, model.peopleState.level)
	assert.Equal(int64(9), model.peopleState.participantID)
	require.NotNil(t, open)
	loaded := runPeopleCommandMessage[peopleContactLoadedMsg](t, open)
	assert.Equal(int64(9), loaded.participantID)
}

func TestPeopleCompletionPanelRendersTypeSourceAndErrorsWithoutHidingDirectory(t *testing.T) {
	assert := assert.New(t)
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.width = 70
	model.height = 18
	model.pageSize = 13
	model.peopleState.initialized = true
	model.peopleState.searchActive = true
	model.peopleState.rows = []query.PersonSummary{testPerson(1, "Directory Person")}
	model.peopleState.completions = []peoplebrowser.CompletionRow{
		completionRow(2, "Alice Example", query.PeopleCompletionOrganization, "Example Labs", "profile"),
	}
	model.peopleState.completionErr = errors.New("completion unavailable")

	view := stripANSI(model.renderPeopleView())
	assert.Contains(view, "Directory Person")
	assert.Contains(view, "Alice Example")
	assert.Contains(view, "organization")
	assert.Contains(view, "Example Labs")
	assert.Contains(view, "profile")
	assert.Contains(view, "completion unavailable")
	assert.Contains(view, "Tab accept")
}

func TestPeopleCompletionPanelKeepsLastSelectionVisibleAndJKRemainText(t *testing.T) {
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.width = 70
	model.height = 18
	model.pageSize = 13
	model.peopleState.searchActive = true
	model.peopleState.searchInput.Focus()
	for id := int64(1); id <= 8; id++ {
		model.peopleState.completions = append(model.peopleState.completions,
			completionRow(id, "Person", query.PeopleCompletionUsername,
				"handle-"+string(rune('0'+id)), "signal"))
	}
	model.peopleState.completionCursor = 7

	view := stripANSI(model.renderPeopleView())
	assert.Contains(t, view, "handle-8")
	assert.NotContains(t, view, "handle-1")

	model.peopleState.clearCompletions()
	model, _ = sendKey(t, model, key('j'))
	model, _ = sendKey(t, model, key('k'))
	assert.Equal(t, "jk", model.peopleState.searchInput.Value())
}
