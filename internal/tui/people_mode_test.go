package tui

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
)

type fakePeopleBackend struct {
	peoplebrowser.Backend

	searchRequests     []peoplebrowser.SearchRequest
	searchPages        []*peoplebrowser.SearchPage
	searchErrs         []error
	completionRequests []peoplebrowser.CompletionRequest
	completionPages    []*peoplebrowser.CompletionPage
	completionErrs     []error
	contactRequests    []int64
	contactErrs        []error
	contacts           map[int64]*query.PersonSummary
}

func (b *fakePeopleBackend) Search(
	_ context.Context, request peoplebrowser.SearchRequest,
) (*peoplebrowser.SearchPage, error) {
	b.searchRequests = append(b.searchRequests, request)
	if len(b.searchErrs) > 0 {
		err := b.searchErrs[0]
		b.searchErrs = b.searchErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(b.searchPages) == 0 {
		return &peoplebrowser.SearchPage{}, nil
	}
	page := b.searchPages[0]
	b.searchPages = b.searchPages[1:]
	return page, nil
}

func (b *fakePeopleBackend) GetContact(
	_ context.Context, participantID int64,
) (*query.PersonSummary, error) {
	b.contactRequests = append(b.contactRequests, participantID)
	if len(b.contactErrs) > 0 {
		err := b.contactErrs[0]
		b.contactErrs = b.contactErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	contact := b.contacts[participantID]
	if contact == nil {
		return nil, fmt.Errorf("contact %d not found", participantID)
	}
	copied := *contact
	return &copied, nil
}

func TestPeopleContactNotFoundReturnsToRefreshedDirectoryAndRejectsStaleResults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := testPerson(2, "Merged Person")
	replacement := testPerson(3, "Replacement Person")
	backend := &fakePeopleBackend{
		contacts:    map[int64]*query.PersonSummary{2: &contact},
		contactErrs: []error{peoplebrowser.ErrContactNotFound},
		searchPages: []*peoplebrowser.SearchPage{{Rows: []query.PersonSummary{replacement}, TotalCount: 1}},
	}
	model := peopleModel(backend)
	model.mode = modePeople
	model.presentationGeneration = 7
	model.peopleState.requestID = 10
	model.peopleState.level = peopleLevelContact
	model.peopleState.tab = peopleTabOverview
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.err = errors.New("refresh requested after stale contact data")
	model.peopleState.searchQuery = "merged"
	model.peopleState.breadcrumbs = []peopleNavSnapshot{{level: peopleLevelDirectory}}

	model, refresh := sendKey(t, model, key('r'))
	missing := runPeopleCommandMessage[peopleContactLoadedMsg](t, refresh)
	missingRequestID := missing.requestID
	updated, directory := model.Update(missing)
	model = asModel(t, updated)

	assert.Equal(peopleLevelDirectory, model.peopleState.level)
	assert.Nil(model.peopleState.contact)
	assert.Zero(model.peopleState.participantID)
	assert.Empty(model.peopleState.breadcrumbs)
	assert.Equal("merged", model.peopleState.searchQuery)
	require.NotNil(directory)
	assert.Contains(model.peopleState.directoryNotice, "no longer available")

	model = sendMsg(t, model, peopleDirectoryLoadedMsg{
		page:      &peoplebrowser.SearchPage{Rows: []query.PersonSummary{contact}},
		requestID: missingRequestID, presentationGeneration: 7,
	})
	assert.Empty(model.peopleState.rows, "the old request cannot repopulate the recovered directory")

	model = sendMsg(t, model, peopleLoadedMessage(t, directory))
	require.Len(model.peopleState.rows, 1)
	assert.Equal(replacement.ID, model.peopleState.rows[0].ID)
	assert.Contains(stripANSI(model.renderPeopleView()), "no longer available")
	assert.Equal([]int64{contact.ID}, backend.contactRequests, "the missing contact is not reloaded in a loop")
}

func TestPeopleContactRetryKeepsCurrentWorkspace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := testPerson(2, "Retried Person")
	backend := &fakePeopleBackend{contacts: map[int64]*query.PersonSummary{2: &contact}}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.level = peopleLevelContact
	model.peopleState.participantID = 2
	model.peopleState.err = errors.New("temporary failure")
	model.peopleState.breadcrumbs = []peopleNavSnapshot{{
		level: peopleLevelDirectory, cursor: 3, scrollOffset: 2,
	}}

	model, cmd := sendKey(t, model, key('r'))

	assert.Equal(peopleLevelContact, model.peopleState.level)
	require.Len(model.peopleState.breadcrumbs, 1)
	require.NotNil(cmd)
	var loaded peopleContactLoadedMsg
	for _, msg := range runBatchCommand(t, cmd) {
		if candidate, ok := msg.(peopleContactLoadedMsg); ok {
			loaded = candidate
		}
	}
	assert.Equal(int64(2), loaded.participantID)
	assert.Equal([]int64{2}, backend.contactRequests)
}

func testPerson(id int64, label string) query.PersonSummary {
	return query.PersonSummary{
		ID:           id,
		DisplayLabel: label,
		Identifiers: []query.PersonIdentifier{{
			Type: emailMessageType, Value: fmt.Sprintf("person-%d@example.test", id),
			DisplayValue: fmt.Sprintf("person-%d@example.test", id), ParticipantID: id,
		}},
		ActivityCount: id * 10,
		FileCount:     id,
		FirstAt:       time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC),
		LastAt:        time.Date(2026, 8, int(id), 12, 30, 0, 0, time.UTC),
		CacheRevision: "cache-1",
	}
}

func peopleModel(backend peoplebrowser.Backend) Model {
	model := New(newMockEngine(MockConfig{}), Options{PeopleBackend: backend})
	model.width = 100
	model.height = 24
	model.pageSize = 19
	model.loading = false
	return model
}

func peopleLoadedMessage(t *testing.T, cmd tea.Cmd) peopleDirectoryLoadedMsg {
	t.Helper()
	for _, msg := range runBatchCommand(t, cmd) {
		if loaded, ok := msg.(peopleDirectoryLoadedMsg); ok {
			return loaded
		}
	}
	require.FailNow(t, "People directory command did not return a load message")
	return peopleDirectoryLoadedMsg{}
}

func leaveAndReenterPeople(t *testing.T, model Model) (Model, tea.Cmd) {
	t.Helper()
	updated, _, handled := model.handleGlobalKeys(key('m'))
	require.True(t, handled)
	model = updated
	require.Equal(t, modeEmail, model.mode)

	// Meetings immediately precedes People in the mode cycle. The leave and
	// re-entry paths are the behavior under test; intermediate modes are not.
	model.mode = modeMeetings
	updated, cmd, handled := model.handleGlobalKeys(key('m'))
	require.True(t, handled)
	model = updated
	require.Equal(t, modePeople, model.mode)
	return model, cmd
}

func TestNextModeIncludesPeople(t *testing.T) {
	assert := assert.New(t)
	assert.Equal(modeTexts, nextMode(modeEmail, true, true))
	assert.Equal(modeMeetings, nextMode(modeTexts, true, true))
	assert.Equal(modePeople, nextMode(modeMeetings, true, true))
	assert.Equal(modeEmail, nextMode(modePeople, true, true))
	assert.Equal(modeMeetings, nextMode(modeEmail, false, true))
	assert.Equal(modePeople, nextMode(modeMeetings, false, true))
	assert.Equal(modeEmail, nextMode(modeMeetings, false, false))
}

func TestEnteringPeopleLoadsFirstDirectoryPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakePeopleBackend{searchPages: []*peoplebrowser.SearchPage{{
		Rows:       []query.PersonSummary{testPerson(1, "Test Person")},
		TotalCount: 1, CacheRevision: "cache-1",
	}}}
	model := peopleModel(backend)
	model.mode = modeMeetings

	updated, cmd, handled := model.handleGlobalKeys(key('m'))

	require.True(handled)
	assert.Equal(modePeople, updated.mode)
	loaded := peopleLoadedMessage(t, cmd)
	require.Len(backend.searchRequests, 1)
	assert.Equal(peoplebrowser.SearchRequest{Limit: peoplePageSize}, backend.searchRequests[0])
	assert.Equal(updated.presentationGeneration, loaded.presentationGeneration)
	assert.Equal(updated.peopleState.requestID, loaded.requestID)

	updated = sendMsg(t, updated, loaded)
	require.Len(updated.peopleState.rows, 1)
	assert.Equal("Test Person", updated.peopleState.rows[0].DisplayLabel)
	assert.True(updated.peopleState.initialized)
	assert.False(updated.loading)
}

func TestPeopleSearchDebounceRejectsStaleResponses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakePeopleBackend{searchPages: []*peoplebrowser.SearchPage{{
		Rows:       []query.PersonSummary{testPerson(2, "Matching Person")},
		TotalCount: 1, CacheRevision: "cache-2",
	}}}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.initialized = true
	model.peopleState.rows = []query.PersonSummary{testPerson(1, "Original Person")}
	model.peopleState.requestID = 4
	model.presentationGeneration = 9

	model, _ = sendKey(t, model, key('/'))
	assert.True(model.peopleState.searchActive)
	assert.True(model.peopleState.searchInput.Focused())

	model, _ = sendKey(t, model, key('m'))
	requestID := model.peopleState.requestID
	debounceID := model.peopleState.searchDebounceID
	assert.Greater(requestID, uint64(4))

	model = sendMsg(t, model, peopleDirectoryLoadedMsg{
		page:      &peoplebrowser.SearchPage{Rows: []query.PersonSummary{testPerson(3, "Stale Request")}},
		requestID: requestID - 1, presentationGeneration: model.presentationGeneration,
	})
	model = sendMsg(t, model, peopleDirectoryLoadedMsg{
		page:      &peoplebrowser.SearchPage{Rows: []query.PersonSummary{testPerson(4, "Stale Presentation")}},
		requestID: requestID, presentationGeneration: model.presentationGeneration - 1,
	})
	require.Len(model.peopleState.rows, 1)
	assert.Equal("Original Person", model.peopleState.rows[0].DisplayLabel)

	updatedModel, cmd := model.Update(peopleSearchDebounceMsg{
		query: "m", debounceID: debounceID, requestID: requestID,
		presentationGeneration: model.presentationGeneration,
	})
	model = asModel(t, updatedModel)
	loaded := peopleLoadedMessage(t, cmd)
	assert.Equal("m", backend.searchRequests[0].Query)
	assert.Equal(requestID, loaded.requestID)

	model = sendMsg(t, model, loaded)
	require.Len(model.peopleState.rows, 1)
	assert.Equal("Matching Person", model.peopleState.rows[0].DisplayLabel)
	assert.Equal("m", model.peopleState.searchQuery)
}

func TestPeopleSearchEscapeRestartsCommittedQueryAfterDebounceLoad(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakePeopleBackend{searchPages: []*peoplebrowser.SearchPage{{
		Rows: []query.PersonSummary{testPerson(2, "Restored Result")},
	}}}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.initialized = true
	model.peopleState.searchQuery = "committed"
	model.peopleState.rows = []query.PersonSummary{testPerson(1, "Committed Result")}

	model, _ = sendKey(t, model, key('/'))
	model, _ = sendKey(t, model, key('x'))
	updated, previewLoad := model.Update(peopleSearchDebounceMsg{
		query:                  "committedx",
		debounceID:             model.peopleState.searchDebounceID,
		requestID:              model.peopleState.requestID,
		presentationGeneration: model.presentationGeneration,
	})
	model = asModel(t, updated)
	require.NotNil(previewLoad)
	assert.Empty(model.peopleState.rows)

	model, replacement := sendKey(t, model, keyEsc())
	assert.False(model.peopleState.searchActive)
	assert.Equal("committed", model.peopleState.searchQuery)
	assert.Equal("committed", model.peopleState.searchInput.Value())
	loaded := peopleLoadedMessage(t, replacement)
	require.Len(backend.searchRequests, 1)
	assert.Equal("committed", backend.searchRequests[0].Query)

	model = sendMsg(t, model, loaded)
	require.Len(model.peopleState.rows, 1)
	assert.Equal("Restored Result", model.peopleState.rows[0].DisplayLabel)
	assert.True(model.peopleState.initialized)
}

func TestPeopleSearchEscapeRestartsActiveDebounceLoadForSameQuery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakePeopleBackend{searchPages: []*peoplebrowser.SearchPage{{
		Rows: []query.PersonSummary{testPerson(2, "Restored Result")},
	}}}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.initialized = true
	model.peopleState.searchQuery = "committed"
	model.peopleState.rows = []query.PersonSummary{testPerson(1, "Committed Result")}

	model, _ = sendKey(t, model, key('/'))
	model, _ = sendKey(t, model, key(' '))
	updated, previewLoad := model.Update(peopleSearchDebounceMsg{
		query:                  "committed",
		debounceID:             model.peopleState.searchDebounceID,
		requestID:              model.peopleState.requestID,
		presentationGeneration: model.presentationGeneration,
	})
	model = asModel(t, updated)
	require.NotNil(previewLoad)
	assert.Empty(model.peopleState.rows)

	model, replacement := sendKey(t, model, keyEsc())
	loaded := peopleLoadedMessage(t, replacement)
	model = sendMsg(t, model, loaded)
	require.Len(model.peopleState.rows, 1)
	assert.Equal("Restored Result", model.peopleState.rows[0].DisplayLabel)
}

func TestPeopleSearchEscapeRestartsFreshLoadInvalidatedBeforeDebounce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakePeopleBackend{searchPages: []*peoplebrowser.SearchPage{{
		Rows: []query.PersonSummary{testPerson(2, "Restored Result")},
	}}}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.initialized = true
	model.peopleState.searchQuery = "committed"
	model.peopleState.rows = []query.PersonSummary{testPerson(1, "Committed Result")}
	model.peopleState.err = errors.New("refresh required")

	model, initialFreshLoad := sendKey(t, model, key('r'))
	require.NotNil(initialFreshLoad)
	assert.False(model.peopleState.initialized)
	assert.Empty(model.peopleState.rows)
	model, _ = sendKey(t, model, key('/'))
	model, _ = sendKey(t, model, key('x'))
	assert.False(model.peopleState.directoryLoading)
	assert.Equal("committed", model.peopleState.searchQuery)

	model, replacement := sendKey(t, model, keyEsc())
	loaded := peopleLoadedMessage(t, replacement)
	require.Len(backend.searchRequests, 1)
	assert.Equal("committed", backend.searchRequests[0].Query)
	model = sendMsg(t, model, loaded)
	require.Len(model.peopleState.rows, 1)
	assert.Equal("Restored Result", model.peopleState.rows[0].DisplayLabel)
	assert.True(model.peopleState.initialized)
}

func TestPeopleSearchFocusCancelPreservesPaginatedDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.peopleState.initialized = true
	for id := int64(1); id <= 10; id++ {
		model.peopleState.rows = append(model.peopleState.rows, testPerson(id, fmt.Sprintf("Person %d", id)))
	}
	model.peopleState.cursor = 4
	model.peopleState.scrollOffset = 2
	model.peopleState.nextCursor = "next-page"

	model, pagination := sendKey(t, model, keyDown())
	require.NotNil(pagination)
	assert.True(model.peopleState.loadingMore)
	model, _ = sendKey(t, model, key('/'))
	model, cancel := sendKey(t, model, keyEsc())

	assert.Nil(cancel)
	require.Len(model.peopleState.rows, 10)
	assert.Equal("Person 1", model.peopleState.rows[0].DisplayLabel)
	assert.Equal(5, model.peopleState.cursor)
	assert.Equal(2, model.peopleState.scrollOffset)
	assert.Equal("next-page", model.peopleState.nextCursor)
	assert.True(model.peopleState.initialized)
	assert.False(model.peopleState.directoryLoading)
	assert.False(model.peopleState.loadingMore)

	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyUp})
	_, resumed := sendKey(t, model, keyDown())
	assert.NotNil(resumed)
}

func TestPeopleContactBackRestoresDirectoryPosition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	second := testPerson(2, "Second Person")
	backend := &fakePeopleBackend{contacts: map[int64]*query.PersonSummary{2: &second}}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.initialized = true
	model.peopleState.rows = []query.PersonSummary{
		testPerson(1, "First Person"), second, testPerson(3, "Third Person"),
	}
	model.peopleState.cursor = 1
	model.peopleState.scrollOffset = 1

	model, cmd := sendKey(t, model, keyEnter())
	assert.Equal(peopleLevelContact, model.peopleState.level)
	assert.Equal(int64(2), model.peopleState.participantID)
	require.Len(model.peopleState.breadcrumbs, 1)

	loaded := cmd()
	contactLoaded, ok := loaded.(peopleContactLoadedMsg)
	require.True(ok)
	assert.Equal(model.peopleState.requestID, contactLoaded.requestID)
	assert.Equal(model.presentationGeneration, contactLoaded.presentationGeneration)
	assert.Equal(int64(2), contactLoaded.participantID)
	model = sendMsg(t, model, contactLoaded)
	require.NotNil(model.peopleState.contact)
	assert.Equal("Second Person", model.peopleState.contact.DisplayLabel)

	model, _ = sendKey(t, model, keyEsc())
	assert.Equal(peopleLevelDirectory, model.peopleState.level)
	assert.Equal(1, model.peopleState.cursor)
	assert.Equal(1, model.peopleState.scrollOffset)
	assert.Empty(model.peopleState.breadcrumbs)
	assert.Nil(model.peopleState.contact)
}

func TestReenteringPeopleReloadsAbandonedContactRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := testPerson(2, "Reloaded Person")
	backend := &fakePeopleBackend{contacts: map[int64]*query.PersonSummary{2: &contact}}
	model := peopleModel(backend)
	model.mode = modeMeetings
	model.peopleState.level = peopleLevelContact
	model.peopleState.initialized = true
	model.peopleState.participantID = 2
	model.peopleState.contact = nil
	model.peopleState.requestID = 5

	updated, cmd, handled := model.handleGlobalKeys(key('m'))

	require.True(handled)
	assert.Equal(modePeople, updated.mode)
	require.NotNil(cmd)
	var loaded peopleContactLoadedMsg
	found := false
	for _, msg := range runBatchCommand(t, cmd) {
		if candidate, ok := msg.(peopleContactLoadedMsg); ok {
			loaded = candidate
			found = true
		}
	}
	require.True(found)
	assert.Equal(uint64(6), loaded.requestID)
	assert.Equal(updated.presentationGeneration, loaded.presentationGeneration)
	assert.Equal(int64(2), loaded.participantID)
}

func TestPeopleDirectoryRefreshReentryRestartsAbandonedLoad(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakePeopleBackend{searchPages: []*peoplebrowser.SearchPage{{
		Rows: []query.PersonSummary{testPerson(2, "Refreshed Person")},
	}}}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.initialized = true
	model.peopleState.rows = []query.PersonSummary{testPerson(1, "Cached Person")}
	model.peopleState.err = errors.New("refresh required")

	model, abandoned := sendKey(t, model, key('r'))
	require.NotNil(abandoned)
	assert.Empty(model.peopleState.rows)

	model, reloaded := leaveAndReenterPeople(t, model)
	loaded := peopleLoadedMessage(t, reloaded)
	require.Len(backend.searchRequests, 1)
	assert.Empty(backend.searchRequests[0].Cursor)
	model = sendMsg(t, model, loaded)
	require.Len(model.peopleState.rows, 1)
	assert.Equal("Refreshed Person", model.peopleState.rows[0].DisplayLabel)
}

func TestPeopleSearchDebounceLoadReentryRestartsAbandonedLoad(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakePeopleBackend{searchPages: []*peoplebrowser.SearchPage{{
		Rows: []query.PersonSummary{testPerson(2, "Search Result")},
	}}}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.initialized = true
	model.peopleState.rows = []query.PersonSummary{testPerson(1, "Cached Person")}

	model, _ = sendKey(t, model, key('/'))
	model, _ = sendKey(t, model, key('n'))
	updated, abandoned := model.Update(peopleSearchDebounceMsg{
		query:                  "n",
		debounceID:             model.peopleState.searchDebounceID,
		requestID:              model.peopleState.requestID,
		presentationGeneration: model.presentationGeneration,
	})
	model = asModel(t, updated)
	require.NotNil(abandoned)
	assert.Empty(model.peopleState.rows)
	model, _ = sendKey(t, model, keyEsc())

	model, reloaded := leaveAndReenterPeople(t, model)
	loaded := peopleLoadedMessage(t, reloaded)
	require.Len(backend.searchRequests, 1)
	assert.Empty(backend.searchRequests[0].Query)
	model = sendMsg(t, model, loaded)
	require.Len(model.peopleState.rows, 1)
	assert.Equal("Search Result", model.peopleState.rows[0].DisplayLabel)
}

func TestPeopleContactTabsCycleBothDirections(t *testing.T) {
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.peopleState.level = peopleLevelContact
	model.peopleState.contact = new(query.PersonSummary)

	want := []peopleTab{
		peopleTabAttributes,
		peopleTabInboxes,
		peopleTabMeetings,
		peopleTabFiles,
		peopleTabActivity,
		peopleTabOverview,
	}
	for _, tab := range want {
		model, _ = sendKey(t, model, keyTab())
		assert.Equal(t, tab, model.peopleState.tab)
	}

	model, _ = sendKey(t, model, keyShiftTab())
	assert.Equal(t, peopleTabActivity, model.peopleState.tab)
}

func TestPeoplePaginationLoadsWithinLastFiveRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	secondPage := []query.PersonSummary{testPerson(11, "Next Page")}
	backend := &fakePeopleBackend{searchPages: []*peoplebrowser.SearchPage{{
		Rows: secondPage, TotalCount: 11, CacheRevision: "cache-1",
	}}}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.initialized = true
	for id := int64(1); id <= 10; id++ {
		model.peopleState.rows = append(model.peopleState.rows, testPerson(id, fmt.Sprintf("Person %d", id)))
	}
	model.peopleState.cursor = 4
	model.peopleState.nextCursor = "next-page"
	model.peopleState.cacheRevision = "cache-1"
	model.peopleState.requestID = 6

	model, cmd := sendKey(t, model, keyDown())

	assert.Equal(5, model.peopleState.cursor)
	require.NotNil(cmd)
	loaded := cmd()
	page, ok := loaded.(peopleDirectoryLoadedMsg)
	require.True(ok)
	assert.True(page.append)
	require.Len(backend.searchRequests, 1)
	assert.Equal("next-page", backend.searchRequests[0].Cursor)

	model = sendMsg(t, model, page)
	require.Len(model.peopleState.rows, 11)
	assert.Equal("Next Page", model.peopleState.rows[10].DisplayLabel)
}

func TestPeoplePaginationModeReentrySettlesAbandonedLoad(t *testing.T) {
	assert := assert.New(t)
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.peopleState.initialized = true
	for id := int64(1); id <= 10; id++ {
		model.peopleState.rows = append(model.peopleState.rows, testPerson(id, fmt.Sprintf("Person %d", id)))
	}
	model.peopleState.cursor = 4
	model.peopleState.nextCursor = "next-page"

	model, abandoned := sendKey(t, model, keyDown())
	require.NotNil(t, abandoned)
	assert.True(model.peopleState.loadingMore)

	model, reloaded := leaveAndReenterPeople(t, model)
	assert.Nil(reloaded)
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyUp})
	_, resumed := sendKey(t, model, keyDown())
	assert.NotNil(resumed)
}

func TestPeopleSearchCancellationSettlesAbandonedPagination(t *testing.T) {
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.peopleState.initialized = true
	for id := int64(1); id <= 10; id++ {
		model.peopleState.rows = append(model.peopleState.rows, testPerson(id, fmt.Sprintf("Person %d", id)))
	}
	model.peopleState.cursor = 4
	model.peopleState.nextCursor = "next-page"

	model, abandoned := sendKey(t, model, keyDown())
	require.NotNil(t, abandoned)
	model, _ = sendKey(t, model, key('/'))
	model, _ = sendKey(t, model, key('x'))
	model, _ = sendKey(t, model, keyEsc())
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyUp})
	_, resumed := sendKey(t, model, keyDown())
	assert.NotNil(t, resumed)
}

func TestPeopleOpenContactSettlesAbandonedPagination(t *testing.T) {
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.peopleState.initialized = true
	for id := int64(1); id <= 10; id++ {
		model.peopleState.rows = append(model.peopleState.rows, testPerson(id, fmt.Sprintf("Person %d", id)))
	}
	model.peopleState.cursor = 4
	model.peopleState.nextCursor = "next-page"

	model, abandoned := sendKey(t, model, keyDown())
	require.NotNil(t, abandoned)
	model, contactLoad := sendKey(t, model, keyEnter())
	require.NotNil(t, contactLoad)
	model, _ = sendKey(t, model, keyEsc())
	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyUp})
	_, resumed := sendKey(t, model, keyDown())
	assert.NotNil(t, resumed)
}

type peopleRevisionError struct{ code string }

func (e peopleRevisionError) Error() string        { return e.code }
func (e peopleRevisionError) APIErrorCode() string { return e.code }

func TestPeoplePaginationRestartsOnceAfterCacheDrift(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	restartPage := &peoplebrowser.SearchPage{
		Rows:       []query.PersonSummary{testPerson(9, "Restarted Person")},
		TotalCount: 1, CacheRevision: "cache-2",
	}
	backend := &fakePeopleBackend{searchPages: []*peoplebrowser.SearchPage{restartPage}}
	model := peopleModel(backend)
	model.mode = modePeople
	model.presentationGeneration = 4
	model.peopleState.initialized = true
	model.peopleState.rows = []query.PersonSummary{testPerson(1, "Old Page")}
	model.peopleState.cursor = 1
	model.peopleState.scrollOffset = 1
	model.peopleState.cacheRevision = "cache-1"
	model.peopleState.requestID = 7

	updated, cmd := model.Update(peopleDirectoryLoadedMsg{
		err: peopleRevisionError{code: "archive_revision_changed"}, append: true,
		requestID: 7, presentationGeneration: 4,
	})
	model = asModel(t, updated)
	assert.Empty(model.peopleState.rows)
	assert.Zero(model.peopleState.cursor)
	assert.Zero(model.peopleState.scrollOffset)
	assert.True(model.peopleState.paginationRestarted)
	require.NoError(model.peopleState.err)

	restarted := cmd()
	loaded, ok := restarted.(peopleDirectoryLoadedMsg)
	require.True(ok)
	require.Len(backend.searchRequests, 1)
	assert.Empty(backend.searchRequests[0].Cursor)
	model = sendMsg(t, model, loaded)
	require.Len(model.peopleState.rows, 1)
	assert.Equal("Restarted Person", model.peopleState.rows[0].DisplayLabel)

	updated, cmd = model.Update(peopleDirectoryLoadedMsg{
		err: peopleRevisionError{code: "search_revision_changed"}, append: true,
		requestID:              model.peopleState.requestID,
		presentationGeneration: model.presentationGeneration,
	})
	model = asModel(t, updated)
	assert.Nil(cmd)
	require.Error(model.peopleState.err)
	require.ErrorIs(model.peopleState.err, errPeopleDirectoryChanged)
	assert.Contains(model.peopleState.err.Error(), "retry")
}

func TestPeopleSecondPaginationDriftPausesUntilExplicitRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	reloadPage := &peoplebrowser.SearchPage{
		Rows: []query.PersonSummary{testPerson(3, "Reloaded Person")},
	}
	backend := &fakePeopleBackend{searchPages: []*peoplebrowser.SearchPage{reloadPage}}
	model := peopleModel(backend)
	model.mode = modePeople
	model.presentationGeneration = 5
	model.peopleState.initialized = true
	model.peopleState.rows = []query.PersonSummary{
		testPerson(1, "Partial Person"), testPerson(2, "Second Person"),
	}
	model.peopleState.nextCursor = "stale-page"
	model.peopleState.cacheRevision = "cache-2"
	model.peopleState.paginationRestarted = true
	model.peopleState.directoryLoading = true
	model.peopleState.loadingMore = true
	model.peopleState.requestID = 9

	model = sendMsg(t, model, peopleDirectoryLoadedMsg{
		err: peopleRevisionError{code: "search_revision_changed"}, append: true,
		requestID: 9, presentationGeneration: 5,
	})
	require.Error(model.peopleState.err)
	view := stripANSI(model.renderView())
	assert.Contains(view, "Partial Person")
	assert.Contains(view, "people directory changed")
	assert.Contains(view, "r retry")

	model, automatic := sendKey(t, model, keyDown())
	assert.Nil(automatic)
	assert.Empty(backend.searchRequests)

	model, retry := sendKey(t, model, key('r'))
	loaded := peopleLoadedMessage(t, retry)
	require.Len(backend.searchRequests, 1)
	assert.Empty(backend.searchRequests[0].Cursor)
	model = sendMsg(t, model, loaded)
	require.Len(model.peopleState.rows, 1)
	assert.Equal("Reloaded Person", model.peopleState.rows[0].DisplayLabel)
	require.NoError(model.peopleState.err)
}
