package tui

import (
	"errors"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
)

const peoplePageSize = 100

var (
	errPeopleDirectoryChanged    = errors.New("people directory changed; retry to reload")
	errPeopleInboxInterrupted    = errors.New("inbox load interrupted; retry from this view")
	errPeopleContentChanged      = errors.New("contact content changed; retry to reload")
	errPeopleRelationshipChanged = errors.New("relationship activity changed; retry to reload")
)

type peopleRelationshipCacheKey struct {
	canonicalID      int64
	year             int
	timezone         string
	cacheRevision    string
	identityRevision int64
}

type peopleLevel uint8

const (
	peopleLevelDirectory peopleLevel = iota
	peopleLevelContact
	peopleLevelInboxTypes
	peopleLevelInboxSources
	peopleLevelConversations
	peopleLevelConversation
	peopleLevelMessage
	peopleLevelMeetingDetail
	peopleLevelActivityMessage
)

type peopleTab uint8

const (
	peopleTabOverview peopleTab = iota
	peopleTabAttributes
	peopleTabInboxes
	peopleTabMeetings
	peopleTabFiles
	peopleTabActivity
	peopleTabCount
)

type peopleState struct {
	level       peopleLevel
	tab         peopleTab
	initialized bool

	rows          []query.PersonSummary
	totalCount    int64
	nextCursor    string
	cacheRevision string
	cursor        int
	scrollOffset  int

	requestID                    uint64
	directoryLoading             bool
	loadingMore                  bool
	paginationRestarted          bool
	directoryNotice              string
	searchActive                 bool
	searchInput                  textinput.Model
	searchQuery                  string
	searchRestoreQuery           string
	searchDebounceID             uint64
	completions                  []peoplebrowser.CompletionRow
	completionCursor             int
	completionLoading            bool
	completionErr                error
	completionCacheRevision      string
	participantID                int64
	contact                      *query.PersonSummary
	contactLoading               bool
	promoting                    bool
	attributes                   *peoplebrowser.Attributes
	attributesLoaded             bool
	attributesLoading            bool
	attributeCursor              int
	attributeScrollOffset        int
	attributesNotice             string
	attributesLoadErr            error
	attributesLoadErrTab         peopleTab
	relationshipCalendar         *query.RelationshipCalendarResponse
	relationshipCalendars        map[peopleRelationshipCacheKey]*query.RelationshipCalendarResponse
	relationshipYear             int
	relationshipFirstYear        int
	relationshipGeneration       uint64
	relationshipLoading          bool
	relationshipErr              error
	relationshipRestarted        bool
	relationshipCacheRevision    string
	relationshipIdentityRevision int64
	inboxTypes                   []peopleInboxType
	inboxesLoaded                bool
	inboxesLoading               bool
	selectedInboxType            string
	selectedInboxSource          *query.PersonInboxRow
	conversations                []query.ConversationRow
	conversationsLoading         bool
	conversationsNextOffset      int
	conversationsPendingOffset   int
	conversationsComplete        bool
	conversationsCacheRevision   string
	conversationsRestarted       bool
	selectedConversationID       int64
	selectedMessageID            int64
	conversationLoading          bool
	conversationNextOffset       int
	conversationPendingOffset    int
	conversationComplete         bool
	conversationCacheRevision    string
	conversationRestarted        bool
	messageLoading               bool
	meetings                     []query.MessageSummary
	meetingsLoaded               bool
	meetingsLoading              bool
	meetingsLoadingMore          bool
	meetingsNextCursor           string
	meetingsCacheRevision        string
	meetingsRestarted            bool
	meetingsErr                  error
	files                        []query.FileRow
	filesLoaded                  bool
	filesLoading                 bool
	filesLoadingMore             bool
	filesNextCursor              string
	filesCacheRevision           string
	filesRestarted               bool
	filesErr                     error
	fileOpening                  bool
	fileOpenFailed               bool
	selectedContentFile          int64
	selectedContentMessage       int64
	activity                     []query.EntryRow
	activityLoaded               bool
	activityLoading              bool
	activityLoadingMore          bool
	activityNextCursor           string
	activityCacheRevision        string
	activityRestarted            bool
	activityErr                  error
	location                     *time.Location
	parkedMeetingState           meetingState
	parkedTextState              textState
	inboxErr                     error
	form                         peopleFormState
	breadcrumbs                  []peopleNavSnapshot
	err                          error
}

func (p *peopleState) clearCompletions() {
	p.completions = nil
	p.completionCursor = 0
	p.completionLoading = false
	p.completionErr = nil
	p.completionCacheRevision = ""
}

func (m *Model) swapPeopleMeetingState() {
	m.meetingState, m.peopleState.parkedMeetingState =
		m.peopleState.parkedMeetingState, m.meetingState
}

func (m *Model) swapPeopleTextState() {
	m.textState, m.peopleState.parkedTextState =
		m.peopleState.parkedTextState, m.textState
}

func (m *Model) switchMessageReaderState(next tuiMode) {
	if m.mode == next || m.mode < modeEmail || m.mode >= modeCount ||
		next < modeEmail || next >= modeCount {
		return
	}
	m.parkedMessageReaders[m.mode] = m.messageReaderState
	m.messageReaderState = m.parkedMessageReaders[next]
}

type peopleNavSnapshot struct {
	level          peopleLevel
	tab            peopleTab
	cursor         int
	scrollOffset   int
	inboxType      string
	source         *query.PersonInboxRow
	conversationID int64
	messageID      int64
}

type peopleInboxType struct {
	key      string
	label    string
	sources  []query.PersonInboxRow
	latestAt time.Time
}

func (p *peopleState) pushBreadcrumb() {
	var source *query.PersonInboxRow
	if p.selectedInboxSource != nil {
		copied := *p.selectedInboxSource
		source = &copied
	}
	p.breadcrumbs = append(p.breadcrumbs, peopleNavSnapshot{
		level: p.level, tab: p.tab, cursor: p.cursor, scrollOffset: p.scrollOffset,
		inboxType: p.selectedInboxType, source: source,
		conversationID: p.selectedConversationID,
		messageID:      p.selectedMessageID,
	})
}

func (p *peopleState) popBreadcrumb() bool {
	if len(p.breadcrumbs) == 0 {
		return false
	}
	last := p.breadcrumbs[len(p.breadcrumbs)-1]
	p.breadcrumbs = p.breadcrumbs[:len(p.breadcrumbs)-1]
	p.level = last.level
	p.tab = last.tab
	p.cursor = last.cursor
	p.scrollOffset = last.scrollOffset
	p.selectedInboxType = last.inboxType
	p.selectedInboxSource = last.source
	p.selectedConversationID = last.conversationID
	p.selectedMessageID = last.messageID
	return true
}

func (m *Model) settlePeopleDirectoryLoad() {
	inboxLoading := m.peopleState.tab == peopleTabInboxes && (m.peopleState.inboxesLoading ||
		m.peopleState.conversationsLoading || m.peopleState.conversationLoading ||
		m.peopleState.messageLoading)
	m.peopleState.abandonContentPageLoad(m.peopleState.tab)
	m.peopleState.directoryLoading = false
	m.peopleState.loadingMore = false
	m.peopleState.completionLoading = false
	m.peopleState.attributesLoading = false
	m.peopleState.abandonRelationshipLoad()
	m.peopleState.promoting = false
	m.peopleState.inboxesLoading = false
	m.peopleState.conversationsLoading = false
	m.peopleState.conversationLoading = false
	m.peopleState.messageLoading = false
	m.meetingState.detailLoading = false
	m.peopleState.settleFileAction()
	if inboxLoading {
		m.peopleState.inboxErr = errPeopleInboxInterrupted
	}
	m.updatePeopleLoading()
}

func (p *peopleState) resetRelationshipContact() {
	p.abandonRelationshipLoad()
	p.relationshipCalendar = nil
	p.relationshipYear = 0
	p.relationshipFirstYear = 0
	p.relationshipErr = nil
	p.relationshipRestarted = false
	p.relationshipCacheRevision = ""
	p.relationshipIdentityRevision = 0
	if p.relationshipCalendars == nil {
		p.relationshipCalendars = make(map[peopleRelationshipCacheKey]*query.RelationshipCalendarResponse)
	}
}

func (p *peopleState) abandonRelationshipLoad() {
	p.relationshipGeneration++
	p.relationshipLoading = false
}

func (p *peopleState) resetAttributes() {
	p.attributes = nil
	p.attributesLoaded = false
	p.attributesLoading = false
	p.attributeCursor = 0
	p.attributeScrollOffset = 0
	p.attributesNotice = ""
	p.attributesLoadErr = nil
	p.attributesLoadErrTab = peopleTabOverview
	p.promoting = false
	p.form.close()
}

func (p *peopleState) resetInboxes() {
	p.inboxTypes = nil
	p.inboxesLoaded = false
	p.inboxesLoading = false
	p.selectedInboxType = ""
	p.selectedInboxSource = nil
	p.conversations = nil
	p.conversationsLoading = false
	p.conversationsNextOffset = 0
	p.conversationsPendingOffset = 0
	p.conversationsComplete = false
	p.conversationsCacheRevision = ""
	p.conversationsRestarted = false
	p.selectedConversationID = 0
	p.selectedMessageID = 0
	p.conversationLoading = false
	p.conversationNextOffset = 0
	p.conversationPendingOffset = 0
	p.conversationComplete = false
	p.conversationCacheRevision = ""
	p.conversationRestarted = false
	p.messageLoading = false
	p.inboxErr = nil
}

func (p *peopleState) resetContent() {
	p.meetings = nil
	p.meetingsLoaded = false
	p.meetingsLoading = false
	p.meetingsLoadingMore = false
	p.meetingsNextCursor = ""
	p.meetingsCacheRevision = ""
	p.meetingsRestarted = false
	p.meetingsErr = nil
	p.files = nil
	p.filesLoaded = false
	p.filesLoading = false
	p.filesLoadingMore = false
	p.filesNextCursor = ""
	p.filesCacheRevision = ""
	p.filesRestarted = false
	p.filesErr = nil
	p.fileOpening = false
	p.fileOpenFailed = false
	p.selectedContentFile = 0
	p.selectedContentMessage = 0
	p.activity = nil
	p.activityLoaded = false
	p.activityLoading = false
	p.activityLoadingMore = false
	p.activityNextCursor = ""
	p.activityCacheRevision = ""
	p.activityRestarted = false
	p.activityErr = nil
}

func (p *peopleState) settleFileAction() bool {
	if !p.fileOpening && !p.fileOpenFailed && p.selectedContentFile <= 0 {
		return false
	}
	wasOpening := p.fileOpening
	if p.fileOpenFailed {
		p.filesErr = nil
	}
	p.fileOpening = false
	p.fileOpenFailed = false
	p.selectedContentFile = 0
	p.selectedContentMessage = 0
	return wasOpening
}

func (p *peopleState) abandonContentPageLoad(tab peopleTab) {
	switch tab {
	case peopleTabMeetings:
		if !p.meetingsLoading {
			return
		}
		p.meetings = nil
		p.meetingsLoaded = false
		p.meetingsLoading = false
		p.meetingsLoadingMore = false
		p.meetingsNextCursor = ""
		p.meetingsCacheRevision = ""
		p.meetingsRestarted = false
		p.meetingsErr = nil
	case peopleTabFiles:
		if !p.filesLoading {
			return
		}
		p.files = nil
		p.filesLoaded = false
		p.filesLoading = false
		p.filesLoadingMore = false
		p.filesNextCursor = ""
		p.filesCacheRevision = ""
		p.filesRestarted = false
		p.filesErr = nil
		p.fileOpenFailed = false
	case peopleTabActivity:
		if !p.activityLoading {
			return
		}
		p.activity = nil
		p.activityLoaded = false
		p.activityLoading = false
		p.activityLoadingMore = false
		p.activityNextCursor = ""
		p.activityCacheRevision = ""
		p.activityRestarted = false
		p.activityErr = nil
	case peopleTabOverview, peopleTabAttributes, peopleTabInboxes, peopleTabCount:
		return
	}
}

func contactMemberIDs(contact *query.PersonSummary) []int64 {
	if contact == nil {
		return nil
	}
	if contact.Cluster != nil && len(contact.Cluster.MemberIDs) > 0 {
		return slices.Clone(contact.Cluster.MemberIDs)
	}
	if contact.ID > 0 {
		return []int64{contact.ID}
	}
	return nil
}

func normalizePeopleSourceType(sourceType string) string {
	return strings.ToLower(strings.TrimSpace(sourceType))
}
