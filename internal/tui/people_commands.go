package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

type peopleDirectoryLoadedMsg struct {
	page                   *peoplebrowser.SearchPage
	err                    error
	requestID              uint64
	append                 bool
	presentationGeneration uint64
}

type peopleContactLoadedMsg struct {
	contact                *query.PersonSummary
	err                    error
	requestID              uint64
	participantID          int64
	presentationGeneration uint64
}

type peopleSearchDebounceMsg struct {
	query                  string
	debounceID             uint64
	requestID              uint64
	presentationGeneration uint64
}

type peopleCompletionLoadedMsg struct {
	page                   *peoplebrowser.CompletionPage
	err                    error
	query                  string
	requestID              uint64
	presentationGeneration uint64
}

type peoplePromotedMsg struct {
	person                 *store.Person
	err                    error
	requestID              uint64
	participantID          int64
	tab                    peopleTab
	presentationGeneration uint64
}

type peopleAttributesLoadedMsg struct {
	attributes             *peoplebrowser.Attributes
	err                    error
	requestID              uint64
	participantID          int64
	personID               int64
	tab                    peopleTab
	presentationGeneration uint64
}

type peopleFieldCreatedMsg struct {
	definition             *store.AttributeDefinition
	err                    error
	requestID              uint64
	participantID          int64
	personID               int64
	tab                    peopleTab
	presentationGeneration uint64
}

type peopleAttributeSetMsg struct {
	write                  *store.PersonAttributeWrite
	err                    error
	requestID              uint64
	participantID          int64
	personID               int64
	tab                    peopleTab
	presentationGeneration uint64
}

type peopleInboxesLoadedMsg struct {
	inboxes                *query.PersonInboxResponse
	err                    error
	requestID              uint64
	participantID          int64
	presentationGeneration uint64
}

type peopleConversationsLoadedMsg struct {
	conversations          []query.ConversationRow
	nextOffset             int
	complete               bool
	cacheRevision          string
	offset                 int
	err                    error
	requestID              uint64
	participantID          int64
	sourceID               int64
	presentationGeneration uint64
}

type peopleConversationMessagesLoadedMsg struct {
	messages               []query.MessageSummary
	nextOffset             int
	complete               bool
	cacheRevision          string
	offset                 int
	err                    error
	requestID              uint64
	participantID          int64
	sourceID               int64
	conversationID         int64
	presentationGeneration uint64
}

type peopleMessageLoadedMsg struct {
	detail                 *query.MessageDetail
	err                    error
	requestID              uint64
	participantID          int64
	sourceID               int64
	conversationID         int64
	messageID              int64
	presentationGeneration uint64
}

type peopleMeetingsLoadedMsg struct {
	page                   *peoplebrowser.MessagePage
	err                    error
	requestID              uint64
	participantID          int64
	cursor                 string
	append                 bool
	presentationGeneration uint64
}

type peopleMeetingLoadedMsg struct {
	detail                 *query.MessageDetail
	err                    error
	requestID              uint64
	participantID          int64
	messageID              int64
	presentationGeneration uint64
}

type peopleFilesLoadedMsg struct {
	page                   *peoplebrowser.FilePage
	err                    error
	requestID              uint64
	participantID          int64
	cursor                 string
	append                 bool
	presentationGeneration uint64
}

type peopleFileMessageLoadedMsg struct {
	detail                 *query.MessageDetail
	err                    error
	requestID              uint64
	participantID          int64
	fileID                 int64
	messageID              int64
	presentationGeneration uint64
}

type peopleFileExportedMsg struct {
	result                 ExportResultMsg
	requestID              uint64
	participantID          int64
	tab                    peopleTab
	fileID                 int64
	messageID              int64
	presentationGeneration uint64
}

type peopleActivityLoadedMsg struct {
	page                   *peoplebrowser.ActivityPage
	err                    error
	requestID              uint64
	participantID          int64
	cursor                 string
	append                 bool
	presentationGeneration uint64
}

type peopleActivityMessageLoadedMsg struct {
	detail                 *query.MessageDetail
	err                    error
	requestID              uint64
	participantID          int64
	messageID              int64
	presentationGeneration uint64
}

func (m Model) loadPeopleDirectory(cursor string, appendResults bool) tea.Cmd {
	backend := m.peopleBackend
	request := peoplebrowser.SearchRequest{
		Query: m.peopleState.searchQuery, Cursor: cursor, Limit: peoplePageSize,
	}
	requestID := m.peopleState.requestID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			page, err := backend.Search(context.Background(), request)
			return peopleDirectoryLoadedMsg{
				page: page, err: err, requestID: requestID, append: appendResults,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleDirectoryLoadedMsg{
				err: fmt.Errorf("people directory panic: %v", r), requestID: requestID,
				append: appendResults, presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) loadPeopleCompletions(queryText string) tea.Cmd {
	backend := m.peopleBackend
	request := peoplebrowser.CompletionRequest{Query: queryText, Limit: peopleCompletionLimit}
	requestID := m.peopleState.requestID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			page, err := backend.Complete(context.Background(), request)
			return peopleCompletionLoadedMsg{
				page: page, err: err, query: queryText, requestID: requestID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleCompletionLoadedMsg{
				err: fmt.Errorf("people completion panic: %v", r), query: queryText,
				requestID: requestID, presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) handlePeopleCompletionLoaded(msg peopleCompletionLoadedMsg) (tea.Model, tea.Cmd) {
	if m.mode != modePeople || !m.peopleState.searchActive ||
		msg.presentationGeneration != m.presentationGeneration ||
		msg.requestID != m.peopleState.requestID || msg.query != m.peopleState.searchQuery {
		return m, nil
	}
	m.peopleState.completionLoading = false
	if msg.err != nil {
		m.peopleState.completions = nil
		m.peopleState.completionCursor = 0
		m.peopleState.completionCacheRevision = ""
		m.peopleState.completionErr = fmt.Errorf("complete people search: %w", msg.err)
		return m, nil
	}
	if msg.page == nil {
		m.peopleState.completions = nil
		m.peopleState.completionCursor = 0
		m.peopleState.completionCacheRevision = ""
		m.peopleState.completionErr = errors.New("complete people search: empty response")
		return m, nil
	}
	m.peopleState.completions = append([]peoplebrowser.CompletionRow(nil), msg.page.Rows...)
	m.peopleState.completionCursor = 0
	m.peopleState.completionCacheRevision = msg.page.CacheRevision
	m.peopleState.completionErr = nil
	return m, nil
}

func (m Model) handlePeopleDirectoryLoaded(msg peopleDirectoryLoadedMsg) (tea.Model, tea.Cmd) {
	if m.mode != modePeople || msg.presentationGeneration != m.presentationGeneration ||
		msg.requestID != m.peopleState.requestID {
		return m, nil
	}
	m.peopleState.directoryLoading = false
	m.peopleState.loadingMore = false

	if msg.err != nil {
		if isPeopleRevisionChange(msg.err) {
			return m.handlePeopleDirectoryDrift()
		}
		m.peopleState.err = fmt.Errorf("load people directory: %w", msg.err)
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.page == nil {
		m.peopleState.err = errors.New("load people directory: empty response")
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.append && m.peopleState.cacheRevision != "" &&
		msg.page.CacheRevision != m.peopleState.cacheRevision {
		return m.handlePeopleDirectoryDrift()
	}

	m.peopleState.err = nil
	m.peopleState.initialized = true
	if msg.append {
		m.peopleState.rows = append(m.peopleState.rows, msg.page.Rows...)
	} else {
		m.peopleState.rows = append([]query.PersonSummary(nil), msg.page.Rows...)
		m.peopleState.cursor = 0
		m.peopleState.scrollOffset = 0
	}
	m.peopleState.totalCount = msg.page.TotalCount
	m.peopleState.nextCursor = msg.page.NextCursor
	m.peopleState.cacheRevision = msg.page.CacheRevision
	m.updatePeopleLoading()
	return m, nil
}

func (m Model) handlePeopleDirectoryDrift() (tea.Model, tea.Cmd) {
	if m.peopleState.paginationRestarted {
		m.peopleState.err = errPeopleDirectoryChanged
		m.updatePeopleLoading()
		return m, nil
	}
	m.peopleState.paginationRestarted = true
	m.peopleState.initialized = false
	m.peopleState.rows = nil
	m.peopleState.totalCount = 0
	m.peopleState.nextCursor = ""
	m.peopleState.cacheRevision = ""
	m.peopleState.cursor = 0
	m.peopleState.scrollOffset = 0
	m.peopleState.err = nil
	m.peopleState.requestID++
	m.peopleState.directoryLoading = true
	m.peopleState.loadingMore = false
	m.loading = true
	return m, m.loadPeopleDirectory("", false)
}

func isPeopleRevisionChange(err error) bool {
	var coded interface{ APIErrorCode() string }
	if !errors.As(err, &coded) {
		return false
	}
	switch coded.APIErrorCode() {
	case "archive_revision_changed", "search_revision_changed", "identity_revision_changed":
		return true
	default:
		return false
	}
}

func (m Model) loadPeopleContact(participantID int64) tea.Cmd {
	backend := m.peopleBackend
	requestID := m.peopleState.requestID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			contact, err := backend.GetContact(context.Background(), participantID)
			return peopleContactLoadedMsg{
				contact: contact, err: err, requestID: requestID, participantID: participantID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleContactLoadedMsg{
				err: fmt.Errorf("people contact panic: %v", r), requestID: requestID,
				participantID: participantID, presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) handlePeopleContactLoaded(msg peopleContactLoadedMsg) (tea.Model, tea.Cmd) {
	if m.mode != modePeople || msg.presentationGeneration != m.presentationGeneration ||
		msg.requestID != m.peopleState.requestID ||
		msg.participantID != m.peopleState.participantID {
		return m, nil
	}
	m.peopleState.contactLoading = false
	if msg.err != nil {
		if errors.Is(msg.err, peoplebrowser.ErrContactNotFound) {
			m.peopleState.level = peopleLevelDirectory
			m.peopleState.tab = peopleTabOverview
			m.peopleState.contact = nil
			m.peopleState.participantID = 0
			m.peopleState.breadcrumbs = nil
			m.peopleState.resetAttributes()
			m.peopleState.resetRelationshipContact()
			m.peopleState.resetInboxes()
			m.peopleState.resetContent()
			m.messageDetail = nil
			m.textState.messages = nil
			m.peopleState.requestID++
			m, cmd := m.startPeopleDirectoryLoad()
			m.peopleState.directoryNotice = "Contact is no longer available; the directory was refreshed."
			return m, cmd
		}
		m.peopleState.err = fmt.Errorf("load contact: %w", msg.err)
	} else if msg.contact == nil {
		m.peopleState.err = errors.New("load contact: empty response")
	} else {
		contact := *msg.contact
		m.peopleState.contact = &contact
		m.peopleState.err = nil
		m.initializePeopleRelationshipYears(&contact)
		if m.peopleState.tab == peopleTabOverview || m.peopleState.tab == peopleTabAttributes {
			var commands []tea.Cmd
			m.peopleState.requestID++
			if m.peopleState.tab == peopleTabOverview {
				if cmd := m.beginPeopleRelationshipLoad(); cmd != nil {
					commands = append(commands, cmd)
				}
			}
			if contact.Profile == nil {
				if m.peopleState.tab == peopleTabAttributes {
					m.peopleState.attributesNotice = peoplePromotionInstruction
				}
			} else {
				m.peopleState.attributesLoading = true
				m.peopleState.attributesLoadErr = nil
				commands = append(commands, m.loadPeopleAttributes(contact.Profile.ID, m.peopleState.tab))
			}
			if len(commands) > 0 {
				m.loading = true
				return m, tea.Batch(append([]tea.Cmd{m.startSpinner()}, commands...)...)
			}
		}
	}
	m.updatePeopleLoading()
	return m, nil
}

func (m Model) promotePeopleContact(participantID int64, tab peopleTab) tea.Cmd {
	backend := m.peopleBackend
	requestID := m.peopleState.requestID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			person, err := backend.Promote(context.Background(), participantID)
			return peoplePromotedMsg{
				person: person, err: err, requestID: requestID, participantID: participantID,
				tab: tab, presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peoplePromotedMsg{
				err: fmt.Errorf("people promotion panic: %v", r), requestID: requestID,
				participantID: participantID, tab: tab,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) loadPeopleAttributes(personID int64, tab peopleTab) tea.Cmd {
	backend := m.peopleBackend
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			attributes, err := backend.ListAttributes(context.Background(), personID)
			return peopleAttributesLoadedMsg{
				attributes: attributes, err: err, requestID: requestID,
				participantID: participantID, personID: personID, tab: tab,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleAttributesLoadedMsg{
				err: fmt.Errorf("people attributes panic: %v", r), requestID: requestID,
				participantID: participantID, personID: personID, tab: tab,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) createPeopleField(field peoplebrowser.NewField) tea.Cmd {
	backend := m.peopleBackend
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	personID := m.peopleState.contact.Profile.ID
	tab := m.peopleState.tab
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			definition, err := backend.CreateField(context.Background(), field)
			return peopleFieldCreatedMsg{
				definition: definition, err: err, requestID: requestID,
				participantID: participantID, personID: personID, tab: tab,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleFieldCreatedMsg{
				err: fmt.Errorf("people field creation panic: %v", r), requestID: requestID,
				participantID: participantID, personID: personID, tab: tab,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) setPeopleAttribute(request peoplebrowser.SetAttributeRequest) tea.Cmd {
	backend := m.peopleBackend
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	tab := m.peopleState.tab
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			write, err := backend.SetAttribute(context.Background(), request)
			return peopleAttributeSetMsg{
				write: write, err: err, requestID: requestID,
				participantID: participantID, personID: request.PersonID, tab: tab,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleAttributeSetMsg{
				err: fmt.Errorf("people attribute write panic: %v", r), requestID: requestID,
				participantID: participantID, personID: request.PersonID, tab: tab,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) loadPeopleInboxes(participantID int64) tea.Cmd {
	backend := m.peopleBackend
	requestID := m.peopleState.requestID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			inboxes, err := backend.ListInboxes(context.Background(), participantID)
			return peopleInboxesLoadedMsg{
				inboxes: inboxes, err: err, requestID: requestID,
				participantID: participantID, presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleInboxesLoadedMsg{
				err: fmt.Errorf("people inboxes panic: %v", r), requestID: requestID,
				participantID: participantID, presentationGeneration: presentationGeneration,
			}
		},
	)
}

func peopleConversationFilter(
	contact *query.PersonSummary, selected query.PersonInboxRow, offset int,
) query.TextFilter {
	return query.TextFilter{
		SourceID:       &selected.SourceID,
		ParticipantIDs: slices.Clone(contactMemberIDs(contact)),
		Pagination:     query.Pagination{Limit: peoplePageSize, Offset: offset},
		SortField:      query.TextSortByLastMessage,
		SortDirection:  query.SortDesc,
	}
}

func (m Model) loadPeopleConversations(selected query.PersonInboxRow, offset int) tea.Cmd {
	backend := m.peopleBackend
	filter := peopleConversationFilter(m.peopleState.contact, selected, offset)
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			page, err := backend.ListConversations(context.Background(), filter)
			if page == nil {
				page = &peoplebrowser.ConversationPage{}
			}
			return peopleConversationsLoadedMsg{
				conversations: page.Rows, nextOffset: page.NextOffset,
				complete: page.Complete, cacheRevision: page.CacheRevision,
				offset: offset, err: err, requestID: requestID,
				participantID: participantID, sourceID: selected.SourceID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleConversationsLoadedMsg{
				err: fmt.Errorf("people conversations panic: %v", r), offset: offset,
				requestID:     requestID,
				participantID: participantID, sourceID: selected.SourceID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) loadPeopleConversationMessages(
	selected query.PersonInboxRow, conversationID int64, offset int,
) tea.Cmd {
	backend := m.peopleBackend
	filter := peopleConversationFilter(m.peopleState.contact, selected, offset)
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			page, err := backend.ListConversationMessages(
				context.Background(), conversationID, filter,
			)
			if page == nil {
				page = &peoplebrowser.ConversationMessagePage{}
			}
			return peopleConversationMessagesLoadedMsg{
				messages: page.Rows, nextOffset: page.NextOffset,
				complete: page.Complete, cacheRevision: page.CacheRevision,
				offset: offset, err: err, requestID: requestID,
				participantID: participantID, sourceID: selected.SourceID,
				conversationID:         conversationID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleConversationMessagesLoadedMsg{
				err: fmt.Errorf("people conversation messages panic: %v", r), offset: offset,
				requestID:     requestID,
				participantID: participantID, sourceID: selected.SourceID,
				conversationID:         conversationID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) loadPeopleMessage(
	selected query.PersonInboxRow, conversationID, messageID int64,
) tea.Cmd {
	backend := m.peopleBackend
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			detail, err := backend.GetMessage(context.Background(), messageID)
			return peopleMessageLoadedMsg{
				detail: detail, err: err, requestID: requestID,
				participantID: participantID, sourceID: selected.SourceID,
				conversationID: conversationID, messageID: messageID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleMessageLoadedMsg{
				err: fmt.Errorf("people message panic: %v", r), requestID: requestID,
				participantID: participantID, sourceID: selected.SourceID,
				conversationID: conversationID, messageID: messageID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) loadPeopleMeetings(cursor string, appendResults bool) tea.Cmd {
	backend := m.peopleBackend
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	presentationGeneration := m.presentationGeneration
	request := peoplebrowser.ContactPageRequest{
		ParticipantID: participantID,
		Cursor:        cursor,
		Limit:         peoplePageSize,
	}
	return safeCmdWithPanic(
		func() tea.Msg {
			page, err := backend.ListMeetings(context.Background(), request)
			return peopleMeetingsLoadedMsg{
				page: page, err: err, requestID: requestID, participantID: participantID,
				cursor: cursor, append: appendResults,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleMeetingsLoadedMsg{
				err: fmt.Errorf("people meetings panic: %v", r), requestID: requestID,
				participantID: participantID, cursor: cursor, append: appendResults,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) loadPeopleMeeting(messageID int64) tea.Cmd {
	backend := m.peopleBackend
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			detail, err := backend.GetMessage(context.Background(), messageID)
			return peopleMeetingLoadedMsg{
				detail: detail, err: err, requestID: requestID,
				participantID: participantID, messageID: messageID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleMeetingLoadedMsg{
				err: fmt.Errorf("people meeting panic: %v", r), requestID: requestID,
				participantID: participantID, messageID: messageID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) loadPeopleFiles(cursor string, appendResults bool) tea.Cmd {
	backend := m.peopleBackend
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	presentationGeneration := m.presentationGeneration
	request := peoplebrowser.ContactPageRequest{
		ParticipantID: participantID,
		Cursor:        cursor,
		Limit:         peoplePageSize,
	}
	return safeCmdWithPanic(
		func() tea.Msg {
			page, err := backend.ListFiles(context.Background(), request)
			return peopleFilesLoadedMsg{
				page: page, err: err, requestID: requestID, participantID: participantID,
				cursor: cursor, append: appendResults,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleFilesLoadedMsg{
				err: fmt.Errorf("people files panic: %v", r), requestID: requestID,
				participantID: participantID, cursor: cursor, append: appendResults,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) loadPeopleFileMessage(fileID, messageID int64) tea.Cmd {
	backend := m.peopleBackend
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			detail, err := backend.GetMessage(context.Background(), messageID)
			return peopleFileMessageLoadedMsg{
				detail: detail, err: err, requestID: requestID,
				participantID: participantID, fileID: fileID, messageID: messageID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleFileMessageLoadedMsg{
				err: fmt.Errorf("people file message panic: %v", r), requestID: requestID,
				participantID: participantID, fileID: fileID, messageID: messageID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) loadPeopleActivity(cursor string, appendResults bool) tea.Cmd {
	backend := m.peopleBackend
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	presentationGeneration := m.presentationGeneration
	request := peoplebrowser.ActivityPageRequest{
		ParticipantID: participantID,
		Cursor:        cursor,
		Limit:         peoplePageSize,
	}
	return safeCmdWithPanic(
		func() tea.Msg {
			page, err := backend.ListActivity(context.Background(), request)
			return peopleActivityLoadedMsg{
				page: page, err: err, requestID: requestID, participantID: participantID,
				cursor: cursor, append: appendResults,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleActivityLoadedMsg{
				err: fmt.Errorf("people activity panic: %v", r), requestID: requestID,
				participantID: participantID, cursor: cursor, append: appendResults,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) loadPeopleActivityMessage(messageID int64) tea.Cmd {
	backend := m.peopleBackend
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			detail, err := backend.GetMessage(context.Background(), messageID)
			return peopleActivityMessageLoadedMsg{
				detail: detail, err: err, requestID: requestID,
				participantID: participantID, messageID: messageID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleActivityMessageLoadedMsg{
				err: fmt.Errorf("people activity message panic: %v", r), requestID: requestID,
				participantID: participantID, messageID: messageID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) peopleContentContextMatches(
	presentationGeneration, requestID uint64, participantID int64, tab peopleTab,
) bool {
	return m.mode == modePeople && m.presentationGeneration == presentationGeneration &&
		m.peopleState.requestID == requestID &&
		m.peopleState.participantID == participantID &&
		m.peopleState.contact != nil && m.peopleState.contact.ID == participantID &&
		m.peopleState.tab == tab
}

func (m Model) handlePeopleMeetingsLoaded(msg peopleMeetingsLoadedMsg) (tea.Model, tea.Cmd) {
	if !m.peopleContentContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID, peopleTabMeetings,
	) || m.peopleState.level != peopleLevelContact {
		return m, nil
	}
	if (msg.append && msg.cursor != m.peopleState.meetingsNextCursor) ||
		(!msg.append && msg.cursor != "") {
		return m, nil
	}
	m.peopleState.meetingsLoading = false
	m.peopleState.meetingsLoadingMore = false
	if msg.err != nil {
		if isPeopleRevisionChange(msg.err) {
			return m.handlePeopleMeetingsDrift()
		}
		m.peopleState.meetingsErr = fmt.Errorf("load contact meetings: %w", msg.err)
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.page == nil {
		m.peopleState.meetingsErr = errors.New("load contact meetings: empty response")
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.append && m.peopleState.meetingsCacheRevision != "" &&
		msg.page.CacheRevision != m.peopleState.meetingsCacheRevision {
		return m.handlePeopleMeetingsDrift()
	}
	if msg.append {
		m.peopleState.meetings = append(m.peopleState.meetings, msg.page.Rows...)
	} else {
		m.peopleState.meetings = slices.Clone(msg.page.Rows)
		m.peopleState.cursor = 0
		m.peopleState.scrollOffset = 0
	}
	sort.SliceStable(m.peopleState.meetings, func(i, j int) bool {
		return m.peopleState.meetings[i].SentAt.After(m.peopleState.meetings[j].SentAt)
	})
	m.peopleState.meetingsLoaded = true
	m.peopleState.meetingsNextCursor = msg.page.NextCursor
	m.peopleState.meetingsCacheRevision = msg.page.CacheRevision
	m.peopleState.meetingsErr = nil
	m.updatePeopleLoading()
	return m, nil
}

func (m Model) handlePeopleMeetingsDrift() (tea.Model, tea.Cmd) {
	if m.peopleState.meetingsRestarted {
		m.peopleState.meetingsErr = errPeopleContentChanged
		m.updatePeopleLoading()
		return m, nil
	}
	m.peopleState.meetingsRestarted = true
	m.peopleState.meetings = nil
	m.peopleState.meetingsLoaded = false
	m.peopleState.meetingsNextCursor = ""
	m.peopleState.meetingsCacheRevision = ""
	m.peopleState.cursor = 0
	m.peopleState.scrollOffset = 0
	m.peopleState.meetingsErr = nil
	m.peopleState.requestID++
	m.peopleState.meetingsLoading = true
	m.loading = true
	return m, m.loadPeopleMeetings("", false)
}

func (m Model) handlePeopleMeetingLoaded(msg peopleMeetingLoadedMsg) (tea.Model, tea.Cmd) {
	if !m.peopleContentContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID, peopleTabMeetings,
	) || m.peopleState.level != peopleLevelMeetingDetail ||
		m.peopleState.selectedContentMessage != msg.messageID {
		return m, nil
	}
	m.meetingState.detailLoading = false
	if msg.err != nil {
		m.peopleState.meetingsErr = fmt.Errorf("load meeting detail: %w", msg.err)
	} else if msg.detail == nil || msg.detail.ID != msg.messageID {
		m.peopleState.meetingsErr = errors.New("load meeting detail: empty or mismatched response")
	} else {
		m.meetingState.detail = msg.detail
		m.peopleState.meetingsErr = nil
		if m.meetingState.detailSearchQuery != "" {
			m.findMeetingDetailMatches()
		}
	}
	m.updatePeopleLoading()
	return m, nil
}

func (m Model) handlePeopleFilesLoaded(msg peopleFilesLoadedMsg) (tea.Model, tea.Cmd) {
	if !m.peopleContentContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID, peopleTabFiles,
	) || m.peopleState.level != peopleLevelContact {
		return m, nil
	}
	if (msg.append && msg.cursor != m.peopleState.filesNextCursor) ||
		(!msg.append && msg.cursor != "") {
		return m, nil
	}
	m.peopleState.filesLoading = false
	m.peopleState.filesLoadingMore = false
	m.peopleState.fileOpenFailed = false
	if msg.err != nil {
		if isPeopleRevisionChange(msg.err) {
			return m.handlePeopleFilesDrift()
		}
		m.peopleState.filesErr = fmt.Errorf("load received files: %w", msg.err)
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.page == nil {
		m.peopleState.filesErr = errors.New("load received files: empty response")
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.append && m.peopleState.filesCacheRevision != "" &&
		msg.page.CacheRevision != m.peopleState.filesCacheRevision {
		return m.handlePeopleFilesDrift()
	}
	rows := msg.page.Rows
	if msg.append {
		rows = append(slices.Clone(m.peopleState.files), rows...)
	}
	m.peopleState.files = deduplicatePeopleFiles(rows)
	sort.SliceStable(m.peopleState.files, func(i, j int) bool {
		return m.peopleState.files[i].OccurredAt.After(m.peopleState.files[j].OccurredAt)
	})
	if !msg.append {
		m.peopleState.cursor = 0
		m.peopleState.scrollOffset = 0
	}
	m.peopleState.filesLoaded = true
	m.peopleState.filesNextCursor = msg.page.NextCursor
	m.peopleState.filesCacheRevision = msg.page.CacheRevision
	m.peopleState.filesErr = nil
	m.updatePeopleLoading()
	return m, nil
}

func (m Model) handlePeopleFilesDrift() (tea.Model, tea.Cmd) {
	if m.peopleState.filesRestarted {
		m.peopleState.filesErr = errPeopleContentChanged
		m.updatePeopleLoading()
		return m, nil
	}
	m.peopleState.filesRestarted = true
	m.peopleState.files = nil
	m.peopleState.filesLoaded = false
	m.peopleState.filesNextCursor = ""
	m.peopleState.filesCacheRevision = ""
	m.peopleState.cursor = 0
	m.peopleState.scrollOffset = 0
	m.peopleState.filesErr = nil
	m.peopleState.requestID++
	m.peopleState.filesLoading = true
	m.loading = true
	return m, m.loadPeopleFiles("", false)
}

func deduplicatePeopleFiles(rows []query.FileRow) []query.FileRow {
	deduped := make([]query.FileRow, 0, len(rows))
	seen := make(map[int64]struct{}, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.ID]; ok {
			continue
		}
		seen[row.ID] = struct{}{}
		deduped = append(deduped, row)
	}
	return deduped
}

func (m Model) handlePeopleFileMessageLoaded(
	msg peopleFileMessageLoadedMsg,
) (tea.Model, tea.Cmd) {
	if !m.peopleContentContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID, peopleTabFiles,
	) || m.peopleState.level != peopleLevelContact ||
		m.peopleState.selectedContentFile != msg.fileID ||
		m.peopleState.selectedContentMessage != msg.messageID {
		return m, nil
	}
	m.peopleState.fileOpening = false
	if msg.err != nil {
		m.peopleState.filesErr = fmt.Errorf("open received file: %w", msg.err)
		m.peopleState.fileOpenFailed = true
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.detail == nil || msg.detail.ID != msg.messageID {
		m.peopleState.filesErr = errors.New("open received file: empty or mismatched message")
		m.peopleState.fileOpenFailed = true
		m.updatePeopleLoading()
		return m, nil
	}
	selection := make(map[int]bool)
	for index, attachment := range msg.detail.Attachments {
		if attachment.ID == msg.fileID && strings.TrimSpace(attachment.ContentHash) != "" {
			selection[index] = true
			break
		}
	}
	if len(selection) == 0 {
		m.peopleState.filesErr = errors.New("open received file: attachment content is unavailable")
		m.peopleState.fileOpenFailed = true
		m.updatePeopleLoading()
		return m, nil
	}
	cmd := m.actions.ExportAttachments(msg.detail, selection)
	if cmd == nil {
		m.peopleState.filesErr = errors.New("open received file: attachment content is unavailable")
		m.peopleState.fileOpenFailed = true
		m.updatePeopleLoading()
		return m, nil
	}
	m.peopleState.filesErr = nil
	m.peopleState.fileOpenFailed = false
	m.peopleState.fileOpening = true
	m.updatePeopleLoading()
	return m, tagPeopleFileExport(cmd, peopleFileExportedMsg{
		requestID: msg.requestID, participantID: msg.participantID,
		tab: peopleTabFiles, fileID: msg.fileID, messageID: msg.messageID,
		presentationGeneration: msg.presentationGeneration,
	})
}

func tagPeopleFileExport(cmd tea.Cmd, context peopleFileExportedMsg) tea.Cmd {
	return func() tea.Msg {
		result, ok := cmd().(ExportResultMsg)
		if !ok {
			result = ExportResultMsg{Err: errors.New("export failed: unexpected response")}
		}
		context.result = result
		return context
	}
}

func (m Model) handlePeopleFileExported(msg peopleFileExportedMsg) (tea.Model, tea.Cmd) {
	if msg.tab != peopleTabFiles || !m.peopleContentContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID, peopleTabFiles,
	) || m.peopleState.level != peopleLevelContact ||
		m.peopleState.selectedContentFile != msg.fileID ||
		m.peopleState.selectedContentMessage != msg.messageID ||
		m.peopleState.cursor < 0 || m.peopleState.cursor >= len(m.peopleState.files) ||
		m.peopleState.files[m.peopleState.cursor].ID != msg.fileID ||
		m.peopleState.files[m.peopleState.cursor].MessageID != msg.messageID {
		return m, nil
	}
	m.peopleState.fileOpening = false
	m.updatePeopleLoading()
	return m.handleExportResult(msg.result)
}

func (m Model) handlePeopleActivityLoaded(
	msg peopleActivityLoadedMsg,
) (tea.Model, tea.Cmd) {
	if !m.peopleContentContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID, peopleTabActivity,
	) || m.peopleState.level != peopleLevelContact {
		return m, nil
	}
	if (msg.append && msg.cursor != m.peopleState.activityNextCursor) ||
		(!msg.append && msg.cursor != "") {
		return m, nil
	}
	m.peopleState.activityLoading = false
	m.peopleState.activityLoadingMore = false
	if msg.err != nil {
		if isPeopleRevisionChange(msg.err) {
			return m.handlePeopleActivityDrift()
		}
		m.peopleState.activityErr = fmt.Errorf("load contact activity: %w", msg.err)
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.page == nil {
		m.peopleState.activityErr = errors.New("load contact activity: empty response")
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.append && m.peopleState.activityCacheRevision != "" &&
		msg.page.CacheRevision != m.peopleState.activityCacheRevision {
		return m.handlePeopleActivityDrift()
	}
	rows := msg.page.Rows
	if msg.append {
		rows = append(slices.Clone(m.peopleState.activity), rows...)
	}
	m.peopleState.activity = deduplicatePeopleActivity(rows)
	sort.SliceStable(m.peopleState.activity, func(i, j int) bool {
		return m.peopleState.activity[i].OccurredAt.After(m.peopleState.activity[j].OccurredAt)
	})
	if !msg.append {
		m.peopleState.cursor = 0
		m.peopleState.scrollOffset = 0
	}
	m.peopleState.activityLoaded = true
	m.peopleState.activityNextCursor = msg.page.NextCursor
	m.peopleState.activityCacheRevision = msg.page.CacheRevision
	m.peopleState.activityErr = nil
	m.updatePeopleLoading()
	return m, nil
}

func (m Model) handlePeopleActivityDrift() (tea.Model, tea.Cmd) {
	if m.peopleState.activityRestarted {
		m.peopleState.activityErr = errPeopleContentChanged
		m.updatePeopleLoading()
		return m, nil
	}
	m.peopleState.activityRestarted = true
	m.peopleState.activity = nil
	m.peopleState.activityLoaded = false
	m.peopleState.activityNextCursor = ""
	m.peopleState.activityCacheRevision = ""
	m.peopleState.cursor = 0
	m.peopleState.scrollOffset = 0
	m.peopleState.activityErr = nil
	m.peopleState.requestID++
	m.peopleState.activityLoading = true
	m.loading = true
	return m, m.loadPeopleActivity("", false)
}

func deduplicatePeopleActivity(rows []query.EntryRow) []query.EntryRow {
	deduped := make([]query.EntryRow, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.Key]; ok {
			continue
		}
		seen[row.Key] = struct{}{}
		deduped = append(deduped, row)
	}
	return deduped
}

func (m Model) handlePeopleActivityMessageLoaded(
	msg peopleActivityMessageLoadedMsg,
) (tea.Model, tea.Cmd) {
	if !m.peopleContentContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID, peopleTabActivity,
	) || m.peopleState.level != peopleLevelActivityMessage ||
		m.peopleState.selectedContentMessage != msg.messageID {
		return m, nil
	}
	m.peopleState.messageLoading = false
	if msg.err != nil {
		m.peopleState.activityErr = fmt.Errorf("load activity detail: %w", msg.err)
	} else if msg.detail == nil || msg.detail.ID != msg.messageID {
		m.peopleState.activityErr = errors.New("load activity detail: empty or mismatched response")
	} else {
		m.messageDetail = msg.detail
		m.detailScroll = 0
		m.detailSearchActive = false
		m.detailSearchQuery = ""
		m.detailSearchMatches = nil
		m.detailSearchMatchIndex = 0
		m.peopleState.activityErr = nil
		m.updateDetailLineCount()
	}
	m.updatePeopleLoading()
	return m, nil
}

func (m Model) peopleInboxContextMatches(
	presentationGeneration, requestID uint64, participantID int64,
) bool {
	return m.mode == modePeople && m.presentationGeneration == presentationGeneration &&
		m.peopleState.requestID == requestID &&
		m.peopleState.participantID == participantID &&
		m.peopleState.contact != nil && m.peopleState.contact.ID == participantID &&
		m.peopleState.tab == peopleTabInboxes
}

func (m Model) handlePeopleInboxesLoaded(msg peopleInboxesLoadedMsg) (tea.Model, tea.Cmd) {
	if !m.peopleInboxContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID,
	) || m.peopleState.level != peopleLevelInboxTypes {
		return m, nil
	}
	m.peopleState.inboxesLoading = false
	if msg.err != nil {
		m.peopleState.inboxErr = fmt.Errorf("load contact inboxes: %w", msg.err)
	} else if msg.inboxes == nil {
		m.peopleState.inboxErr = errors.New("load contact inboxes: empty response")
	} else {
		m.peopleState.inboxTypes = groupPeopleInboxes(msg.inboxes.Rows)
		m.peopleState.inboxesLoaded = true
		m.peopleState.inboxErr = nil
		m.peopleState.cursor = min(m.peopleState.cursor, max(len(m.peopleState.inboxTypes)-1, 0))
	}
	m.updatePeopleLoading()
	return m, nil
}

func (m Model) handlePeopleConversationsLoaded(
	msg peopleConversationsLoadedMsg,
) (tea.Model, tea.Cmd) {
	if !m.peopleInboxContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID,
	) || m.peopleState.level != peopleLevelConversations ||
		m.peopleState.selectedInboxSource == nil ||
		m.peopleState.selectedInboxSource.SourceID != msg.sourceID ||
		m.peopleState.conversationsPendingOffset != msg.offset {
		return m, nil
	}
	m.peopleState.conversationsLoading = false
	if msg.err != nil {
		m.peopleState.inboxErr = fmt.Errorf("load source conversations: %w", msg.err)
	} else if msg.offset > 0 && peoplePageRevisionChanged(
		m.peopleState.conversationsCacheRevision, msg.cacheRevision,
	) {
		if m.peopleState.conversationsRestarted {
			m.peopleState.inboxErr = errPeopleContentChanged
		} else {
			m.peopleState.conversationsRestarted = true
			m.peopleState.requestID++
			m.peopleState.conversationsPendingOffset = 0
			m.peopleState.conversationsLoading = true
			m.loading = true
			return m, tea.Batch(
				m.startSpinner(), m.loadPeopleConversations(*m.peopleState.selectedInboxSource, 0),
			)
		}
	} else {
		if msg.offset == 0 {
			m.peopleState.conversations = slices.Clone(msg.conversations)
		} else {
			m.peopleState.conversations = appendUniqueConversations(
				m.peopleState.conversations, msg.conversations,
			)
		}
		m.textState.conversations = slices.Clone(m.peopleState.conversations)
		m.peopleState.conversationsNextOffset = msg.nextOffset
		m.peopleState.conversationsComplete = msg.complete
		m.peopleState.conversationsCacheRevision = msg.cacheRevision
		m.peopleState.inboxErr = nil
		m.peopleState.cursor = min(m.peopleState.cursor, max(len(m.peopleState.conversations)-1, 0))
		m.peopleState.scrollOffset = min(m.peopleState.scrollOffset, m.peopleState.cursor)
	}
	m.updatePeopleLoading()
	return m, nil
}

func (m Model) handlePeopleConversationMessagesLoaded(
	msg peopleConversationMessagesLoadedMsg,
) (tea.Model, tea.Cmd) {
	if !m.peopleInboxContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID,
	) || m.peopleState.level != peopleLevelConversation ||
		m.peopleState.selectedInboxSource == nil ||
		m.peopleState.selectedInboxSource.SourceID != msg.sourceID ||
		m.peopleState.selectedConversationID != msg.conversationID ||
		m.peopleState.conversationPendingOffset != msg.offset {
		return m, nil
	}
	m.peopleState.conversationLoading = false
	if msg.err != nil {
		m.peopleState.inboxErr = fmt.Errorf("load conversation messages: %w", msg.err)
	} else if msg.offset > 0 && peoplePageRevisionChanged(
		m.peopleState.conversationCacheRevision, msg.cacheRevision,
	) {
		if m.peopleState.conversationRestarted {
			m.peopleState.inboxErr = errPeopleContentChanged
		} else {
			m.peopleState.conversationRestarted = true
			m.peopleState.requestID++
			m.peopleState.conversationPendingOffset = 0
			m.peopleState.conversationLoading = true
			m.loading = true
			return m, tea.Batch(
				m.startSpinner(), m.loadPeopleConversationMessages(
					*m.peopleState.selectedInboxSource, m.peopleState.selectedConversationID, 0,
				),
			)
		}
	} else {
		if msg.offset == 0 {
			m.textState.messages = slices.Clone(msg.messages)
			m.textState.cursor = 0
			m.textState.scrollOffset = 0
		} else {
			m.textState.messages = appendUniqueMessages(m.textState.messages, msg.messages)
		}
		m.peopleState.conversationNextOffset = msg.nextOffset
		m.peopleState.conversationComplete = msg.complete
		m.peopleState.conversationCacheRevision = msg.cacheRevision
		m.textState.unfilteredMessages = nil
		m.peopleState.inboxErr = nil
	}
	m.updatePeopleLoading()
	return m, nil
}

func peoplePageRevisionChanged(previous, next string) bool {
	return previous != "" && next != "" && previous != next
}

func appendUniqueConversations(
	current, next []query.ConversationRow,
) []query.ConversationRow {
	seen := make(map[int64]struct{}, len(current)+len(next))
	result := slices.Clone(current)
	for _, row := range current {
		seen[row.ConversationID] = struct{}{}
	}
	for _, row := range next {
		if _, ok := seen[row.ConversationID]; ok {
			continue
		}
		seen[row.ConversationID] = struct{}{}
		result = append(result, row)
	}
	return result
}

func appendUniqueMessages(current, next []query.MessageSummary) []query.MessageSummary {
	seen := make(map[int64]struct{}, len(current)+len(next))
	result := slices.Clone(current)
	for _, row := range current {
		seen[row.ID] = struct{}{}
	}
	for _, row := range next {
		if _, ok := seen[row.ID]; ok {
			continue
		}
		seen[row.ID] = struct{}{}
		result = append(result, row)
	}
	return result
}

func (m Model) handlePeopleMessageLoaded(msg peopleMessageLoadedMsg) (tea.Model, tea.Cmd) {
	if !m.peopleInboxContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID,
	) || m.peopleState.level != peopleLevelMessage ||
		m.peopleState.selectedInboxSource == nil ||
		m.peopleState.selectedInboxSource.SourceID != msg.sourceID ||
		m.peopleState.selectedConversationID != msg.conversationID ||
		m.peopleState.selectedMessageID != msg.messageID {
		return m, nil
	}
	m.peopleState.messageLoading = false
	if msg.err != nil {
		m.peopleState.inboxErr = fmt.Errorf("load message detail: %w", msg.err)
	} else if msg.detail == nil || msg.detail.ID != msg.messageID {
		m.peopleState.inboxErr = errors.New("load message detail: empty or mismatched response")
	} else {
		m.messageDetail = msg.detail
		m.detailScroll = 0
		m.detailSearchActive = false
		m.detailSearchQuery = ""
		m.detailSearchMatches = nil
		m.detailSearchMatchIndex = 0
		m.peopleState.inboxErr = nil
		m.updateDetailLineCount()
	}
	m.updatePeopleLoading()
	return m, nil
}

func groupPeopleInboxes(rows []query.PersonInboxRow) []peopleInboxType {
	groups := make([]peopleInboxType, 0)
	byKey := make(map[string]int)
	for _, row := range rows {
		key := normalizePeopleSourceType(row.SourceType)
		if key == "" {
			key = "unknown"
		}
		index, ok := byKey[key]
		if !ok {
			label := key
			if len(label) > 0 {
				label = strings.ToUpper(label[:1]) + label[1:]
			}
			index = len(groups)
			byKey[key] = index
			groups = append(groups, peopleInboxType{key: key, label: label})
		}
		groups[index].sources = append(groups[index].sources, row)
		if row.LatestAt.After(groups[index].latestAt) {
			groups[index].latestAt = row.LatestAt
		}
	}
	for i := range groups {
		sort.SliceStable(groups[i].sources, func(a, b int) bool {
			left, right := groups[i].sources[a], groups[i].sources[b]
			if left.LatestAt.Equal(right.LatestAt) {
				return left.SourceID < right.SourceID
			}
			return left.LatestAt.After(right.LatestAt)
		})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].latestAt.Equal(groups[j].latestAt) {
			return groups[i].label < groups[j].label
		}
		return groups[i].latestAt.After(groups[j].latestAt)
	})
	return groups
}

func (m Model) peopleAsyncContextMatches(
	presentationGeneration, requestID uint64, participantID, personID int64, tab peopleTab,
) bool {
	if m.mode != modePeople || m.presentationGeneration != presentationGeneration ||
		m.peopleState.requestID != requestID ||
		m.peopleState.level != peopleLevelContact ||
		m.peopleState.participantID != participantID || m.peopleState.tab != tab {
		return false
	}
	return personID <= 0 || (m.peopleState.contact != nil &&
		m.peopleState.contact.Profile != nil && m.peopleState.contact.Profile.ID == personID)
}

func (m Model) handlePeoplePromoted(msg peoplePromotedMsg) (tea.Model, tea.Cmd) {
	if !m.peopleAsyncContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID, 0, msg.tab,
	) {
		return m, nil
	}
	m.peopleState.promoting = false
	if msg.err != nil {
		m.peopleState.attributesNotice = fmt.Sprintf("Promotion failed: %v", msg.err)
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.person == nil {
		m.peopleState.attributesNotice = "Promotion failed: empty response"
		m.updatePeopleLoading()
		return m, nil
	}
	if !slices.Contains(msg.person.ParticipantIDs, msg.participantID) {
		m.peopleState.attributesNotice = "Promotion failed: response did not match the active contact"
		m.updatePeopleLoading()
		return m, nil
	}
	profile := &query.PersonProfile{ID: msg.person.ID, Revision: msg.person.Revision}
	if msg.person.DisplayName != nil {
		displayName := *msg.person.DisplayName
		profile.DisplayName = &displayName
	}
	m.peopleState.contact.Profile = profile
	m.peopleState.attributes = nil
	m.peopleState.attributesLoaded = false
	m.peopleState.attributesLoadErr = nil
	m.peopleState.attributesNotice = "Contact promoted. Loading attributes..."
	m.peopleState.requestID++
	m.peopleState.attributesLoading = true
	m.loading = true
	return m, m.loadPeopleAttributes(msg.person.ID, msg.tab)
}

func (m Model) handlePeopleAttributesLoaded(
	msg peopleAttributesLoadedMsg,
) (tea.Model, tea.Cmd) {
	if !m.peopleAsyncContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID, msg.personID, msg.tab,
	) {
		return m, nil
	}
	m.peopleState.attributesLoading = false
	if msg.err != nil {
		m.peopleState.attributesLoadErr = fmt.Errorf("load attributes failed: %w", msg.err)
		m.peopleState.attributesLoadErrTab = msg.tab
		m.peopleState.attributesNotice = ""
		if m.peopleState.form.staleReloadPending {
			m.peopleState.form.serverValue = ""
			m.peopleState.form.serverValuePresent = false
			m.peopleState.form.notice = "Server reload failed. Draft preserved; press Enter to retry reload or Esc to cancel."
		}
		m.updatePeopleLoading()
		return m, nil
	}
	if msg.attributes == nil || msg.attributes.PersonID != msg.personID {
		m.peopleState.attributesLoadErr = errors.New("load attributes failed: empty or mismatched response")
		m.peopleState.attributesLoadErrTab = msg.tab
		m.peopleState.attributesNotice = ""
		if m.peopleState.form.staleReloadPending {
			m.peopleState.form.serverValue = ""
			m.peopleState.form.serverValuePresent = false
			m.peopleState.form.notice = "Server reload failed. Draft preserved; press Enter to retry reload or Esc to cancel."
		}
		m.updatePeopleLoading()
		return m, nil
	}
	m.peopleState.attributes = msg.attributes
	m.peopleState.attributesLoaded = true
	m.peopleState.attributesLoadErr = nil
	m.peopleState.attributesLoadErrTab = peopleTabOverview
	m.peopleState.attributesNotice = ""
	rows := peopleAttributeSelections(msg.attributes)
	m.peopleState.attributeCursor = min(m.peopleState.attributeCursor, max(len(rows)-1, 0))
	m.peopleState.attributeScrollOffset = calculateScrollOffset(
		m.peopleState.attributeCursor, m.peopleState.attributeScrollOffset,
		m.peopleAttributesDataRows(),
	)
	m.refreshStalePeopleForm(msg.attributes)
	m.updatePeopleLoading()
	return m, nil
}

func (m Model) handlePeopleFieldCreated(msg peopleFieldCreatedMsg) (tea.Model, tea.Cmd) {
	if !m.peopleAsyncContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID, msg.personID, msg.tab,
	) || m.peopleState.form.overlay != peopleOverlayNewField {
		return m, nil
	}
	m.peopleState.form.submitting = false
	if msg.err != nil {
		m.peopleState.form.notice = fmt.Sprintf("Create field failed: %v", msg.err)
		return m, nil
	}
	if msg.definition == nil {
		m.peopleState.form.notice = "Create field failed: empty response"
		return m, nil
	}
	m.peopleState.form = newPeopleValueForm(*msg.definition, nil, 0)
	m.peopleState.requestID++
	m.peopleState.attributesLoading = true
	m.loading = true
	return m, m.loadPeopleAttributes(msg.personID, msg.tab)
}

func (m Model) handlePeopleAttributeSet(msg peopleAttributeSetMsg) (tea.Model, tea.Cmd) {
	if !m.peopleAsyncContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID, msg.personID, msg.tab,
	) || m.peopleState.form.overlay != peopleOverlayAttributeValue {
		return m, nil
	}
	m.peopleState.form.submitting = false
	if msg.err != nil {
		if stale, ok := stalePeopleValue(msg.err); ok {
			m.peopleState.form.expectedValueID = stale.CurrentValueID
			m.peopleState.form.serverValue = ""
			m.peopleState.form.serverValuePresent = false
			m.peopleState.form.staleConflict = true
			m.peopleState.form.staleReloadPending = true
			m.peopleState.form.notice = "Value changed on the server. Reloading the current server value; draft preserved."
			m.peopleState.requestID++
			m.peopleState.attributesLoading = true
			m.loading = true
			return m, m.loadPeopleAttributes(msg.personID, msg.tab)
		}
		m.peopleState.form.notice = fmt.Sprintf("Save failed: %v", msg.err)
		return m, nil
	}
	// The write response is intentionally not copied into loaded values. The
	// following server reload is the only source of displayed current values.
	_ = msg.write
	m.peopleState.form.close()
	m.peopleState.attributesNotice = "Value saved. Reloading attributes..."
	m.peopleState.requestID++
	m.peopleState.attributesLoading = true
	m.loading = true
	return m, m.loadPeopleAttributes(msg.personID, msg.tab)
}

func (m *Model) updatePeopleLoading() {
	if m.mode != modePeople {
		return
	}
	m.loading = m.peopleState.directoryLoading || m.peopleState.contactLoading ||
		m.peopleState.attributesLoading || m.peopleState.relationshipLoading ||
		m.peopleState.promoting ||
		m.peopleState.inboxesLoading || m.peopleState.conversationsLoading ||
		m.peopleState.conversationLoading || m.peopleState.messageLoading ||
		m.peopleState.meetingsLoading || m.meetingState.detailLoading ||
		m.peopleState.filesLoading || m.peopleState.fileOpening ||
		m.peopleState.activityLoading
	if !m.loading {
		m.transitionBuffer = ""
	}
}
