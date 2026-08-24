package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
)

type peopleRelationshipLoadedMsg struct {
	response               *query.RelationshipCalendarResponse
	err                    error
	relationshipGeneration uint64
	participantID          int64
	year                   int
	timezone               string
	presentationGeneration uint64
}

func (m Model) peopleRelationshipTimezone() string {
	location := m.peopleState.location
	if location == nil {
		location = time.Local
	}
	return location.String()
}

func (m *Model) initializePeopleRelationshipYears(contact *query.PersonSummary) {
	location := m.peopleState.location
	if location == nil {
		location = time.Local
		m.peopleState.location = location
	}
	currentYear := time.Now().In(location).Year()
	firstYear := currentYear
	if contact != nil && !contact.FirstAt.IsZero() {
		firstYear = contact.FirstAt.In(location).Year()
	}
	firstYear = max(1970, min(firstYear, currentYear))
	m.peopleState.relationshipFirstYear = firstYear
	if m.peopleState.relationshipYear < firstYear || m.peopleState.relationshipYear > currentYear {
		m.peopleState.relationshipYear = currentYear
	}
}

func (m Model) cachedPeopleRelationshipCalendar() *query.RelationshipCalendarResponse {
	contact := m.peopleState.contact
	if contact == nil || m.peopleState.relationshipCacheRevision == "" {
		return nil
	}
	key := peopleRelationshipCacheKey{
		canonicalID:      contact.ID,
		year:             m.peopleState.relationshipYear,
		timezone:         m.peopleRelationshipTimezone(),
		cacheRevision:    m.peopleState.relationshipCacheRevision,
		identityRevision: m.peopleState.relationshipIdentityRevision,
	}
	return m.peopleState.relationshipCalendars[key]
}

// beginPeopleRelationshipLoad starts a request under an identity independent
// from other People-mode work, so attribute and content loads cannot make a
// valid calendar completion stale.
func (m *Model) beginPeopleRelationshipLoad() tea.Cmd {
	if m.peopleState.contact == nil || m.peopleState.tab != peopleTabOverview ||
		m.peopleState.level != peopleLevelContact {
		return nil
	}
	m.peopleState.relationshipGeneration++
	m.initializePeopleRelationshipYears(m.peopleState.contact)
	if cached := m.cachedPeopleRelationshipCalendar(); cached != nil {
		m.peopleState.relationshipCalendar = cached
		m.peopleState.relationshipLoading = false
		m.peopleState.relationshipErr = nil
		return nil
	}
	m.peopleState.relationshipCalendar = nil
	m.peopleState.relationshipLoading = true
	m.peopleState.relationshipErr = nil
	return m.loadPeopleRelationshipCalendar(
		m.peopleState.participantID,
		m.peopleState.relationshipYear,
		m.peopleRelationshipTimezone(),
	)
}

func (m Model) loadPeopleRelationshipCalendar(
	participantID int64, year int, timezone string,
) tea.Cmd {
	backend := m.peopleBackend
	relationshipGeneration := m.peopleState.relationshipGeneration
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			response, err := backend.RelationshipCalendar(context.Background(), peoplebrowser.CalendarRequest{
				ParticipantID: participantID,
				Year:          year,
				Timezone:      timezone,
			})
			return peopleRelationshipLoadedMsg{
				response: response, err: err, relationshipGeneration: relationshipGeneration,
				participantID: participantID, year: year, timezone: timezone,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleRelationshipLoadedMsg{
				err:                    fmt.Errorf("people relationship calendar panic: %v", r),
				relationshipGeneration: relationshipGeneration,
				participantID:          participantID, year: year, timezone: timezone,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) handlePeopleRelationshipLoaded(msg peopleRelationshipLoadedMsg) (tea.Model, tea.Cmd) {
	if m.mode != modePeople || msg.presentationGeneration != m.presentationGeneration ||
		msg.relationshipGeneration != m.peopleState.relationshipGeneration ||
		m.peopleState.level != peopleLevelContact || m.peopleState.tab != peopleTabOverview ||
		msg.participantID != m.peopleState.participantID ||
		msg.year != m.peopleState.relationshipYear ||
		msg.timezone != m.peopleRelationshipTimezone() {
		return m, nil
	}
	m.peopleState.relationshipLoading = false
	if msg.err != nil {
		m.peopleState.relationshipErr = fmt.Errorf("load relationship: %w", msg.err)
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.response == nil {
		m.peopleState.relationshipErr = errors.New("load relationship: empty response")
		m.updatePeopleLoading()
		return m, nil
	}
	contact := m.peopleState.contact
	if contact == nil {
		m.updatePeopleLoading()
		return m, nil
	}
	revisionChanged := (contact.CacheRevision != "" && msg.response.CacheRevision != contact.CacheRevision) ||
		(m.peopleState.relationshipCacheRevision != "" &&
			(msg.response.CacheRevision != m.peopleState.relationshipCacheRevision ||
				msg.response.IdentityRevision != m.peopleState.relationshipIdentityRevision))
	if revisionChanged {
		m.peopleState.relationshipCalendar = nil
		clear(m.peopleState.relationshipCalendars)
		m.peopleState.relationshipCacheRevision = ""
		m.peopleState.relationshipIdentityRevision = 0
		if !m.peopleState.relationshipRestarted {
			m.peopleState.relationshipRestarted = true
			m.peopleState.requestID++
			m.peopleState.contactLoading = true
			m.loading = true
			return m, tea.Batch(m.startSpinner(), m.loadPeopleContact(m.peopleState.participantID))
		}
		m.peopleState.relationshipErr = errPeopleRelationshipChanged
		m.updatePeopleLoading()
		return m, nil
	}

	response := *msg.response
	response.Days = append([]query.RelationshipCalendarDay(nil), msg.response.Days...)
	m.peopleState.relationshipCacheRevision = response.CacheRevision
	m.peopleState.relationshipIdentityRevision = response.IdentityRevision
	key := peopleRelationshipCacheKey{
		canonicalID:      response.CanonicalID,
		year:             response.Year,
		timezone:         response.Timezone,
		cacheRevision:    response.CacheRevision,
		identityRevision: response.IdentityRevision,
	}
	if m.peopleState.relationshipCalendars == nil {
		m.peopleState.relationshipCalendars = make(map[peopleRelationshipCacheKey]*query.RelationshipCalendarResponse)
	}
	m.peopleState.relationshipCalendars[key] = &response
	m.peopleState.relationshipCalendar = &response
	m.peopleState.relationshipErr = nil
	m.peopleState.relationshipRestarted = false
	m.updatePeopleLoading()
	return m, nil
}

func (m Model) changePeopleRelationshipYear(delta int) (Model, tea.Cmd) {
	if delta == 0 || m.peopleState.contact == nil {
		return m, nil
	}
	m.initializePeopleRelationshipYears(m.peopleState.contact)
	location := m.peopleState.location
	if location == nil {
		location = time.Local
	}
	currentYear := time.Now().In(location).Year()
	nextYear := m.peopleState.relationshipYear + delta
	if nextYear < m.peopleState.relationshipFirstYear || nextYear > currentYear {
		return m, nil
	}
	m.peopleState.relationshipYear = nextYear
	m.peopleState.scrollOffset = 0
	m.peopleState.relationshipLoading = false
	m.peopleState.relationshipErr = nil
	m.peopleState.relationshipRestarted = false
	cmd := m.beginPeopleRelationshipLoad()
	m.updatePeopleLoading()
	if cmd == nil {
		return m, nil
	}
	m.loading = true
	return m, tea.Batch(m.startSpinner(), cmd)
}
