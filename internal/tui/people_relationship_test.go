package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
)

type fakePeopleRelationshipBackend struct {
	*fakePeopleBackend

	calendarRequests  []peoplebrowser.CalendarRequest
	calendarResponses []*query.RelationshipCalendarResponse
	calendarErrs      []error
}

type fakePeopleRelationshipAttributesBackend struct {
	*fakePeopleRelationshipBackend

	attributes *peoplebrowser.Attributes
}

func (b *fakePeopleRelationshipAttributesBackend) ListAttributes(
	_ context.Context, _ int64,
) (*peoplebrowser.Attributes, error) {
	return b.attributes, nil
}

func (b *fakePeopleRelationshipBackend) RelationshipCalendar(
	_ context.Context, request peoplebrowser.CalendarRequest,
) (*query.RelationshipCalendarResponse, error) {
	b.calendarRequests = append(b.calendarRequests, request)
	if len(b.calendarErrs) > 0 {
		err := b.calendarErrs[0]
		b.calendarErrs = b.calendarErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(b.calendarResponses) == 0 {
		return &query.RelationshipCalendarResponse{}, nil
	}
	response := b.calendarResponses[0]
	b.calendarResponses = b.calendarResponses[1:]
	copied := *response
	copied.Days = append([]query.RelationshipCalendarDay(nil), response.Days...)
	return &copied, nil
}

func relationshipCalendarMessage(t *testing.T, cmd tea.Cmd) peopleRelationshipLoadedMsg {
	t.Helper()
	for _, msg := range runBatchCommand(t, cmd) {
		if loaded, ok := msg.(peopleRelationshipLoadedMsg); ok {
			return loaded
		}
	}
	require.FailNow(t, "People relationship command did not return a load message")
	return peopleRelationshipLoadedMsg{}
}

func TestPeopleOverviewLoadsRelationshipForObservedContact(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	location, err := time.LoadLocation("America/New_York")
	require.NoError(err)
	year := time.Now().In(location).Year()
	contact := testPerson(7, "Observed Person")
	contact.Profile = nil
	contact.FirstAt = time.Date(2018, 4, 2, 12, 0, 0, 0, time.UTC)
	contact.CacheRevision = "cache-7"
	response := &query.RelationshipCalendarResponse{
		CanonicalID: 7, Year: year, Timezone: location.String(),
		CacheRevision: "cache-7", IdentityRevision: 9,
	}
	backend := &fakePeopleRelationshipBackend{
		fakePeopleBackend: &fakePeopleBackend{}, calendarResponses: []*query.RelationshipCalendarResponse{response},
	}
	model := peopleModel(backend)
	model.mode = modePeople
	model.presentationGeneration = 3
	model.peopleState.location = location
	model.peopleState.level = peopleLevelContact
	model.peopleState.tab = peopleTabOverview
	model.peopleState.participantID = contact.ID
	model.peopleState.requestID = 4
	model.peopleState.contactLoading = true

	updated, cmd := model.Update(peopleContactLoadedMsg{
		contact: &contact, requestID: 4, participantID: contact.ID, presentationGeneration: 3,
	})
	model = asModel(t, updated)
	loaded := relationshipCalendarMessage(t, cmd)
	require.Len(backend.calendarRequests, 1)
	assert.Equal(peoplebrowser.CalendarRequest{
		ParticipantID: contact.ID, Year: year, Timezone: location.String(),
	}, backend.calendarRequests[0])
	assert.True(model.peopleState.relationshipLoading)
	assert.Equal(2018, model.peopleState.relationshipFirstYear)

	model = sendMsg(t, model, loaded)
	assert.False(model.peopleState.relationshipLoading)
	require.NotNil(model.peopleState.relationshipCalendar)
	assert.Equal(year, model.peopleState.relationshipCalendar.Year)
}

func TestPeopleRelationshipYearNavigationUsesBoundsAndCachedYears(t *testing.T) {
	assert := assert.New(t)
	currentYear := time.Now().UTC().Year()
	contact := testPerson(7, "Year Browser")
	contact.FirstAt = time.Date(currentYear-2, 1, 1, 0, 0, 0, 0, time.UTC)
	contact.CacheRevision = "cache-1"
	backend := &fakePeopleRelationshipBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		calendarResponses: []*query.RelationshipCalendarResponse{
			{CanonicalID: 7, Year: currentYear - 1, Timezone: "UTC", CacheRevision: "cache-1", IdentityRevision: 4},
		},
	}
	model := peopleContentModel(backend, &contact)
	model.peopleState.location = time.UTC
	model.peopleState.relationshipYear = currentYear
	model.peopleState.relationshipFirstYear = currentYear - 2
	model.peopleState.relationshipCacheRevision = "cache-1"
	model.peopleState.relationshipIdentityRevision = 4
	model.peopleState.relationshipCalendar = &query.RelationshipCalendarResponse{
		CanonicalID: 7, Year: currentYear, Timezone: "UTC", CacheRevision: "cache-1", IdentityRevision: 4,
	}
	model.peopleState.relationshipCalendars = map[peopleRelationshipCacheKey]*query.RelationshipCalendarResponse{
		{canonicalID: 7, year: currentYear, timezone: "UTC", cacheRevision: "cache-1", identityRevision: 4}: model.peopleState.relationshipCalendar,
	}

	model, cmd := sendKey(t, model, key('['))
	assert.Equal(currentYear-1, model.peopleState.relationshipYear)
	loaded := relationshipCalendarMessage(t, cmd)
	model = sendMsg(t, model, loaded)

	model, cmd = sendKey(t, model, key(']'))
	assert.Equal(currentYear, model.peopleState.relationshipYear)
	assert.Nil(cmd, "a previously loaded exact revision/year/timezone should be reused")

	model, cmd = sendKey(t, model, key(']'))
	assert.Equal(currentYear, model.peopleState.relationshipYear)
	assert.Nil(cmd, "future years are unavailable")
}

func TestPeopleRelationshipYearNavigationDoesNotAbandonAttributesLoad(t *testing.T) {
	assert := assert.New(t)
	currentYear := time.Now().UTC().Year()
	contact := testPerson(7, "Concurrent Loader")
	contact.FirstAt = time.Date(currentYear-1, 1, 1, 0, 0, 0, 0, time.UTC)
	contact.Profile = &query.PersonProfile{ID: 51}
	backend := &fakePeopleRelationshipAttributesBackend{
		fakePeopleRelationshipBackend: &fakePeopleRelationshipBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			calendarResponses: []*query.RelationshipCalendarResponse{{
				CanonicalID: 7, Year: currentYear - 1, Timezone: "UTC",
			}},
		},
		attributes: &peoplebrowser.Attributes{PersonID: 51},
	}
	model := peopleContentModel(backend, &contact)
	model.peopleState.location = time.UTC
	model.peopleState.relationshipYear = currentYear
	model.peopleState.relationshipFirstYear = currentYear - 1
	model.peopleState.attributesLoading = true
	attributesRequestID := model.peopleState.requestID
	attributesCmd := model.loadPeopleAttributes(51, peopleTabOverview)

	model, relationshipCmd := sendKey(t, model, key('['))
	assert.NotNil(relationshipCmd)
	assert.Equal(attributesRequestID, model.peopleState.requestID,
		"calendar navigation must not invalidate unrelated Overview requests")
	assert.True(model.peopleState.attributesLoading)

	attributesLoaded := runPeopleCommandMessage[peopleAttributesLoadedMsg](t, attributesCmd)
	model = sendMsg(t, model, attributesLoaded)
	assert.False(model.peopleState.attributesLoading)
	assert.True(model.peopleState.attributesLoaded)
	assert.NotNil(model.peopleState.attributes)
}

func TestPeopleAttributesReloadDoesNotAbandonRelationshipLoad(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	currentYear := time.Now().UTC().Year()
	contact := testPerson(7, "Independent Loaders")
	contact.Profile = &query.PersonProfile{ID: 51}
	contact.CacheRevision = "cache-1"
	backend := &fakePeopleRelationshipAttributesBackend{
		fakePeopleRelationshipBackend: &fakePeopleRelationshipBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			calendarResponses: []*query.RelationshipCalendarResponse{{
				CanonicalID: 7, Year: currentYear, Timezone: "UTC",
				CacheRevision: "cache-1", IdentityRevision: 4,
			}},
		},
		attributes: &peoplebrowser.Attributes{PersonID: 51},
	}
	model := peopleContentModel(backend, &contact)
	model.peopleState.location = time.UTC
	pending := model.beginPeopleRelationshipLoad()
	require.NotNil(pending)
	loaded := relationshipCalendarMessage(t, pending)
	sharedRequestID := model.peopleState.requestID

	model, _ = sendKey(t, model, key('r'))
	assert.Greater(model.peopleState.requestID, sharedRequestID)
	assert.True(model.peopleState.relationshipLoading)

	model = sendMsg(t, model, loaded)
	assert.False(model.peopleState.relationshipLoading)
	require.NotNil(model.peopleState.relationshipCalendar)
	assert.Equal(currentYear, model.peopleState.relationshipCalendar.Year)
}

func TestLeavingPeopleContactResetsRelationshipState(t *testing.T) {
	assert := assert.New(t)
	contact := testPerson(7, "Leaving Contact")
	model := peopleContentModel(
		&fakePeopleRelationshipBackend{fakePeopleBackend: &fakePeopleBackend{}},
		&contact,
	)
	model.peopleState.relationshipLoading = true
	model.peopleState.relationshipYear = 2025
	model.peopleState.relationshipFirstYear = 2018
	model.peopleState.relationshipCalendar = &query.RelationshipCalendarResponse{Year: 2025}
	model.peopleState.breadcrumbs = []peopleNavSnapshot{{
		level: peopleLevelDirectory, tab: peopleTabOverview,
	}}
	generation := model.peopleState.relationshipGeneration

	model, _ = sendKey(t, model, keyEsc())

	assert.Nil(model.peopleState.contact)
	assert.False(model.peopleState.relationshipLoading)
	assert.Nil(model.peopleState.relationshipCalendar)
	assert.Zero(model.peopleState.relationshipYear)
	assert.Zero(model.peopleState.relationshipFirstYear)
	assert.Greater(model.peopleState.relationshipGeneration, generation)
}

func TestPeopleRelationshipRejectsStaleResponseAndRestartsRevisionOnce(t *testing.T) {
	assert := assert.New(t)
	contact := testPerson(7, "Changing Person")
	contact.CacheRevision = "cache-new"
	backend := &fakePeopleRelationshipBackend{fakePeopleBackend: &fakePeopleBackend{}}
	model := peopleContentModel(backend, &contact)
	model.presentationGeneration = 8
	model.peopleState.requestID = 10
	model.peopleState.relationshipGeneration = 10
	model.peopleState.relationshipYear = 2025
	model.peopleState.relationshipLoading = true

	model = sendMsg(t, model, peopleRelationshipLoadedMsg{
		response:               &query.RelationshipCalendarResponse{Year: 2025},
		relationshipGeneration: 9, participantID: 7, year: 2025, timezone: "UTC", presentationGeneration: 8,
	})
	assert.True(model.peopleState.relationshipLoading, "stale results cannot settle the active request")

	model.peopleState.location = time.UTC
	model.peopleState.relationshipLoading = true
	updated, cmd := model.Update(peopleRelationshipLoadedMsg{
		response: &query.RelationshipCalendarResponse{
			CanonicalID: 7, Year: 2025, Timezone: "UTC", CacheRevision: "cache-old", IdentityRevision: 2,
		},
		relationshipGeneration: 10, participantID: 7, year: 2025, timezone: "UTC", presentationGeneration: 8,
	})
	model = asModel(t, updated)
	assert.True(model.peopleState.relationshipRestarted)
	assert.True(model.peopleState.contactLoading)
	require.NotNil(t, cmd, "first revision mismatch refreshes the contact before retrying")

	model.peopleState.contactLoading = false
	model.peopleState.relationshipLoading = true
	model.peopleState.requestID = 11
	model.peopleState.relationshipGeneration = 11
	model = sendMsg(t, model, peopleRelationshipLoadedMsg{
		response: &query.RelationshipCalendarResponse{
			CanonicalID: 7, Year: 2025, Timezone: "UTC", CacheRevision: "still-old", IdentityRevision: 3,
		},
		relationshipGeneration: 11, participantID: 7, year: 2025, timezone: "UTC", presentationGeneration: 8,
	})
	assert.False(model.peopleState.relationshipLoading)
	require.ErrorIs(t, model.peopleState.relationshipErr, errPeopleRelationshipChanged)
}

func TestPeopleRelationshipErrorRetriesAndTabLeaveSettlesPendingLoad(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := testPerson(7, "Retry Person")
	contact.Profile = nil
	contact.CacheRevision = "cache-1"
	backend := &fakePeopleRelationshipBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		calendarErrs:      []error{errors.New("calendar unavailable")},
	}
	model := peopleContentModel(backend, &contact)
	model.peopleState.location = time.UTC
	model.peopleState.relationshipYear = time.Now().UTC().Year()
	model.peopleState.relationshipFirstYear = model.peopleState.relationshipYear - 1
	model.peopleState.relationshipErr = errors.New("calendar unavailable")

	model, retry := sendKey(t, model, key('r'))
	assert.True(model.peopleState.relationshipLoading)
	require.NotNil(retry)
	stale := relationshipCalendarMessage(t, retry)

	model, _ = sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(peopleTabAttributes, model.peopleState.tab)
	assert.False(model.peopleState.relationshipLoading)
	assert.False(model.loading)
	model = sendMsg(t, model, stale)
	require.NoError(model.peopleState.relationshipErr, "the abandoned response is rejected")

	model, fresh := sendKey(t, model, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	assert.Equal(peopleTabOverview, model.peopleState.tab)
	assert.True(model.peopleState.relationshipLoading)
	require.NotNil(fresh, "re-entering Overview resumes an abandoned uncached calendar")
}

func TestPeopleModeReentryRestartsAbandonedRelationshipLoad(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := testPerson(7, "Mode Switch Person")
	contact.CacheRevision = "cache-1"
	backend := &fakePeopleRelationshipBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		calendarResponses: []*query.RelationshipCalendarResponse{{
			CanonicalID: 7, Year: time.Now().UTC().Year(), Timezone: "UTC",
			CacheRevision: "cache-1", IdentityRevision: 2,
		}},
	}
	model := peopleContentModel(backend, &contact)
	model.peopleState.location = time.UTC
	abandoned := model.beginPeopleRelationshipLoad()
	require.NotNil(abandoned)
	require.True(model.peopleState.relationshipLoading)

	updated, _, handled := model.handleGlobalKeys(key('m'))
	require.True(handled)
	model = updated
	assert.Equal(modeEmail, model.mode)
	assert.False(model.peopleState.relationshipLoading)

	model.mode = modeMeetings
	updated, restarted, handled := model.handleGlobalKeys(key('m'))
	require.True(handled)
	model = updated
	assert.Equal(modePeople, model.mode)
	assert.True(model.peopleState.relationshipLoading)
	require.NotNil(restarted, "re-entering Overview restarts the abandoned calendar request")
	assert.Contains(stripANSI(model.renderView()), "Loading relationship activity")
	assert.NotContains(stripANSI(model.renderView()), "Press r to retry")

	loaded := relationshipCalendarMessage(t, restarted)
	require.Len(backend.calendarRequests, 1)
	model = sendMsg(t, model, loaded)
	require.NotNil(model.peopleState.relationshipCalendar)
	assert.False(model.peopleState.relationshipLoading)
}
