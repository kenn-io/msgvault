package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
)

func TestPeopleInboxDaemonRevisionDriftRestartsOnceThenPauses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var revision atomic.Value
	revision.Store("r2")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/text/conversations", r.URL.Path)
		offset := 0
		if value := r.URL.Query().Get("offset"); value != "" {
			var err error
			offset, err = strconv.Atoi(value)
			if !assert.NoError(err) {
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		currentRevision, ok := revision.Load().(string)
		if !assert.True(ok) {
			return
		}
		_, err := fmt.Fprintf(w, `{
			"cache_revision":%q,
			"conversations":[{"conversation_id":%d,"title":"Page","source_type":"whatsapp","message_count":1,"participant_count":1,"last_preview":"preview"}],
			"count":1,"has_more":true,"limit":100,"offset":%d
		}`, currentRevision, int64(offset+1), offset)
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	remote, err := daemonclient.NewEngine(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(remote.Close()) })

	contact := inboxTestContact()
	source := query.PersonInboxRow{SourceID: 21, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp}
	model := peopleModel(daemonclient.NewPeopleBrowser(remote))
	model.mode = modePeople
	model.peopleState.level = peopleLevelConversations
	model.peopleState.tab = peopleTabInboxes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.selectedInboxSource = &source
	model.peopleState.requestID = 5
	model.peopleState.conversations = []query.ConversationRow{{ConversationID: 1}}
	model.peopleState.conversationsCacheRevision = "r1"
	model.peopleState.conversationsPendingOffset = 100

	appendResult := runPeopleCommandMessage[peopleConversationsLoadedMsg](
		t, model.loadPeopleConversations(source, 100),
	)
	updated, restart := model.Update(appendResult)
	model = asModel(t, updated)
	require.NotNil(restart)
	assert.True(model.peopleState.conversationsRestarted)
	assert.Equal(uint64(6), model.peopleState.requestID)

	restarted := runPeopleCommandMessage[peopleConversationsLoadedMsg](t, restart)
	model = sendMsg(t, model, restarted)
	assert.Equal("r2", model.peopleState.conversationsCacheRevision)

	revision.Store("r3")
	model.peopleState.conversationsPendingOffset = 100
	model.peopleState.conversationsLoading = true
	secondAppend := runPeopleCommandMessage[peopleConversationsLoadedMsg](
		t, model.loadPeopleConversations(source, 100),
	)
	model = sendMsg(t, model, secondAppend)
	require.ErrorIs(model.peopleState.inboxErr, errPeopleContentChanged)
}

type peopleInboxConversationRequest struct {
	filter query.TextFilter
}

type peopleInboxMessageRequest struct {
	conversationID int64
	filter         query.TextFilter
}

type fakePeopleInboxBackend struct {
	*fakePeopleBackend

	inboxes              *query.PersonInboxResponse
	inboxRequests        []int64
	conversations        []query.ConversationRow
	conversationPages    map[int]*peoplebrowser.ConversationPage
	conversationErrs     []error
	conversationRequests []peopleInboxConversationRequest
	messages             []query.MessageSummary
	messagePages         map[int]*peoplebrowser.ConversationMessagePage
	messageRequests      []peopleInboxMessageRequest
	detail               *query.MessageDetail
	detailRequests       []int64
}

var errFakePeopleInboxMessageNotFound = errors.New("fake People inbox message not found")

func (b *fakePeopleInboxBackend) ListInboxes(
	_ context.Context, participantID int64,
) (*query.PersonInboxResponse, error) {
	b.inboxRequests = append(b.inboxRequests, participantID)
	return b.inboxes, nil
}

func (b *fakePeopleInboxBackend) ListConversations(
	_ context.Context, filter query.TextFilter,
) (*peoplebrowser.ConversationPage, error) {
	b.conversationRequests = append(b.conversationRequests, peopleInboxConversationRequest{
		filter: clonePeopleInboxFilter(filter),
	})
	if len(b.conversationErrs) > 0 {
		err := b.conversationErrs[0]
		b.conversationErrs = b.conversationErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if page := b.conversationPages[filter.Pagination.Offset]; page != nil {
		copied := *page
		copied.Rows = slices.Clone(page.Rows)
		return &copied, nil
	}
	return conversationTestPage(b.conversations, filter.Pagination), nil
}

func (b *fakePeopleInboxBackend) ListConversationMessages(
	_ context.Context, conversationID int64, filter query.TextFilter,
) (*peoplebrowser.ConversationMessagePage, error) {
	b.messageRequests = append(b.messageRequests, peopleInboxMessageRequest{
		conversationID: conversationID,
		filter:         clonePeopleInboxFilter(filter),
	})
	if page := b.messagePages[filter.Pagination.Offset]; page != nil {
		copied := *page
		copied.Rows = slices.Clone(page.Rows)
		return &copied, nil
	}
	start := min(filter.Pagination.Offset, len(b.messages))
	end := min(start+filter.Pagination.Limit, len(b.messages))
	return &peoplebrowser.ConversationMessagePage{
		Rows: slices.Clone(b.messages[start:end]), NextOffset: end,
		Complete: end == len(b.messages),
	}, nil
}

func conversationTestPage(
	rows []query.ConversationRow, pagination query.Pagination,
) *peoplebrowser.ConversationPage {
	start := min(pagination.Offset, len(rows))
	end := min(start+pagination.Limit, len(rows))
	return &peoplebrowser.ConversationPage{
		Rows: slices.Clone(rows[start:end]), NextOffset: end, Complete: end == len(rows),
	}
}

func (b *fakePeopleInboxBackend) GetMessage(
	_ context.Context, messageID int64,
) (*query.MessageDetail, error) {
	b.detailRequests = append(b.detailRequests, messageID)
	if b.detail == nil {
		return nil, errFakePeopleInboxMessageNotFound
	}
	detail := *b.detail
	detail.Attachments = slices.Clone(b.detail.Attachments)
	return &detail, nil
}

func clonePeopleInboxFilter(filter query.TextFilter) query.TextFilter {
	cloned := filter
	cloned.ParticipantIDs = slices.Clone(filter.ParticipantIDs)
	if filter.SourceID != nil {
		sourceID := *filter.SourceID
		cloned.SourceID = &sourceID
	}
	return cloned
}

func inboxTestContact() query.PersonSummary {
	contact := testPerson(7, "Test Contact")
	contact.Cluster = &query.PersonCluster{
		CanonicalID: 7,
		MemberIDs:   []int64{7, 9, 11},
	}
	return contact
}

func TestPeopleInboxNavigatesBeeperWhatsAppWithExactContactScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	latest := time.Date(2026, 8, 20, 17, 45, 0, 0, time.UTC)
	contact := inboxTestContact()
	backend := &fakePeopleInboxBackend{
		fakePeopleBackend: &fakePeopleBackend{
			contacts: map[int64]*query.PersonSummary{contact.ID: &contact},
		},
		inboxes: &query.PersonInboxResponse{Rows: []query.PersonInboxRow{
			{SourceID: 21, SourceType: " Beeper ", SourceIdentifier: sourceTypeWhatsApp, LatestAt: latest},
			{SourceID: 22, SourceType: "beeper", SourceIdentifier: "signal", LatestAt: latest.Add(-time.Hour)},
			{SourceID: 23, SourceType: "BEEPER", SourceIdentifier: sourceTypeWhatsApp, LatestAt: latest.Add(-90 * time.Minute)},
			{SourceID: 31, SourceType: "SLACK", SourceIdentifier: "test-workspace", LatestAt: latest.Add(-2 * time.Hour)},
		}},
		conversations: []query.ConversationRow{{
			ConversationID: 101,
			Title:          "Test chat",
			SourceType:     sourceTypeWhatsApp,
			MessageCount:   1,
			LastMessageAt:  latest,
		}},
		messages: []query.MessageSummary{{
			ID:                501,
			ConversationID:    101,
			FromName:          "Test Sender",
			SentAt:            latest,
			MessageType:       sourceTypeWhatsApp,
			ConversationTitle: "Test chat",
			Snippet:           "Exact participant-scoped hello",
		}},
		detail: &query.MessageDetail{
			ID: 501, SourceID: 21, ConversationID: 101,
			Subject: "Test chat message", MessageType: sourceTypeWhatsApp,
			SentAt: latest, BodyText: "Existing detail renderer body",
			Attachments: []query.AttachmentInfo{{
				ID: 1, Filename: "test-note.txt", MimeType: "text/plain", Size: 42,
			}},
		},
	}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.level = peopleLevelContact
	model.peopleState.tab = peopleTabAttributes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact

	updated, cmd := model.activatePeopleTab(peopleTabInboxes)
	model = asModel(t, updated)
	loadedInboxes := runPeopleCommandMessage[peopleInboxesLoadedMsg](t, cmd)
	model = sendMsg(t, model, loadedInboxes)

	assert.Equal([]int64{contact.ID}, backend.inboxRequests)
	assert.Equal(peopleLevelInboxTypes, model.peopleState.level)
	typeView := model.renderPeopleView()
	assert.Contains(typeView, "Beeper")
	assert.Contains(typeView, "Slack")
	assert.Equal(1, strings.Count(typeView, "Beeper"))
	assert.NotContains(typeView, "WhatsApp")

	model, _ = sendKey(t, model, keyEnter())
	assert.Equal(peopleLevelInboxSources, model.peopleState.level)
	sourceView := model.renderPeopleView()
	assert.Contains(sourceView, "WhatsApp")
	assert.Contains(sourceView, "Signal")
	assert.Equal(2, strings.Count(sourceView, "WhatsApp"),
		"concrete source rows must not collapse when identifiers match")
	assert.NotContains(sourceView, "test-workspace")

	model, cmd = sendKey(t, model, keyEnter())
	loadedConversations := runPeopleCommandMessage[peopleConversationsLoadedMsg](t, cmd)
	model = sendMsg(t, model, loadedConversations)

	require.Len(backend.conversationRequests, 1)
	filter := backend.conversationRequests[0].filter
	require.NotNil(filter.SourceID)
	assert.Equal(int64(21), *filter.SourceID)
	assert.Equal([]int64{7, 9, 11}, filter.ParticipantIDs)
	assert.Equal(query.Pagination{Limit: peoplePageSize}, filter.Pagination)
	assert.Equal(query.TextSortByLastMessage, filter.SortField)
	assert.Equal(query.SortDesc, filter.SortDirection)
	assert.Equal(peopleLevelConversations, model.peopleState.level)
	assert.Contains(model.renderPeopleView(), "Test chat")

	model, cmd = sendKey(t, model, keyEnter())
	loadedMessages := runPeopleCommandMessage[peopleConversationMessagesLoadedMsg](t, cmd)
	model = sendMsg(t, model, loadedMessages)

	require.Len(backend.messageRequests, 1)
	assert.Equal(int64(101), backend.messageRequests[0].conversationID)
	assert.Equal(int64(21), *backend.messageRequests[0].filter.SourceID)
	assert.Equal([]int64{7, 9, 11}, backend.messageRequests[0].filter.ParticipantIDs)
	assert.Equal(peopleLevelConversation, model.peopleState.level)
	assert.Contains(model.renderPeopleView(), "Exact participant-scoped hello")

	model, cmd = sendKey(t, model, keyEnter())
	loadedDetail := runPeopleCommandMessage[peopleMessageLoadedMsg](t, cmd)
	model = sendMsg(t, model, loadedDetail)

	assert.Equal([]int64{501}, backend.detailRequests)
	assert.Equal(peopleLevelMessage, model.peopleState.level)
	detailView := model.renderPeopleView()
	assert.Contains(detailView, "Existing detail renderer body")
	assert.Contains(detailView, "test-note.txt")

	model, _ = sendKey(t, model, keyEsc())
	assert.Equal(peopleLevelConversation, model.peopleState.level)
	assert.Equal(0, model.textState.cursor)
}

func TestPeopleInboxSourceRowsShowDirectionalCountsAndLatestReceived(t *testing.T) {
	assert := assert.New(t)
	latestReceived := time.Date(2026, 8, 20, 16, 30, 0, 0, time.UTC)
	model := peopleModel(&fakePeopleBackend{})
	contact := inboxTestContact()
	model.mode = modePeople
	model.peopleState.level = peopleLevelInboxSources
	model.peopleState.tab = peopleTabInboxes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.selectedInboxType = "beeper"
	model.peopleState.inboxTypes = []peopleInboxType{{
		key: "beeper", label: "Beeper", sources: []query.PersonInboxRow{{
			SourceID: 21, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp,
			ConversationCount: 3, ReceivedCount: 12, SentCount: 7,
			LatestReceivedAt: &latestReceived, LatestAt: latestReceived,
		}},
	}}

	view := model.renderPeopleView()

	assert.Contains(view, "WhatsApp")
	assert.Contains(view, "latest received 2026-08-20 16:30")
	assert.Contains(view, "sent 7")
	assert.Contains(view, "received 12")
	assert.Contains(view, "conversations 3")

	model.width = 52
	assert.Contains(model.renderPeopleView(), "WhatsApp")
}

func TestPeopleInboxEmptySourceHasExactHelpfulMessage(t *testing.T) {
	model := peopleModel(&fakePeopleBackend{})
	contact := inboxTestContact()
	model.mode = modePeople
	model.peopleState.level = peopleLevelConversations
	model.peopleState.tab = peopleTabInboxes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.selectedInboxSource = &query.PersonInboxRow{
		SourceID: 21, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp,
	}

	assert.Contains(
		t, model.renderPeopleView(),
		"No conversations found for this contact on this source",
	)
}

func TestPeopleInboxConversationRetryKeepsContactAndSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := inboxTestContact()
	source := query.PersonInboxRow{
		SourceID: 21, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp,
	}
	backend := &fakePeopleInboxBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		conversationErrs:  []error{errors.New("temporary source failure"), nil},
		conversations: []query.ConversationRow{{
			ConversationID: 101, Title: "Recovered chat",
		}},
	}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.level = peopleLevelInboxSources
	model.peopleState.tab = peopleTabInboxes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.selectedInboxType = "beeper"
	model.peopleState.inboxTypes = []peopleInboxType{{
		key: "beeper", label: "Beeper", sources: []query.PersonInboxRow{source},
	}}

	model, firstLoad := sendKey(t, model, keyEnter())
	firstFailure := runPeopleCommandMessage[peopleConversationsLoadedMsg](t, firstLoad)
	model = sendMsg(t, model, firstFailure)
	require.Error(model.peopleState.inboxErr)
	assert.Equal(peopleLevelConversations, model.peopleState.level)

	model, cmd := sendKey(t, model, key('r'))

	require.NotNil(cmd)
	assert.Equal(contact.ID, model.peopleState.participantID)
	require.NotNil(model.peopleState.selectedInboxSource)
	assert.Equal(source.SourceID, model.peopleState.selectedInboxSource.SourceID)
	loaded := runPeopleCommandMessage[peopleConversationsLoadedMsg](t, cmd)
	model = sendMsg(t, model, loaded)
	require.NoError(model.peopleState.inboxErr)
	assert.Equal(peopleLevelConversations, model.peopleState.level)
	assert.Contains(model.renderPeopleView(), "Recovered chat")
}

func TestPeopleInboxPaginatesAllSourceConversationsWithoutLosingSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := inboxTestContact()
	source := query.PersonInboxRow{SourceID: 21, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp}
	rows := make([]query.ConversationRow, 205)
	for i := range rows {
		rows[i] = query.ConversationRow{ConversationID: int64(i + 1), Title: fmt.Sprintf("Chat %03d", i+1)}
	}
	backend := &fakePeopleInboxBackend{
		fakePeopleBackend: &fakePeopleBackend{}, conversations: rows,
	}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.level = peopleLevelInboxSources
	model.peopleState.tab = peopleTabInboxes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.selectedInboxType = "beeper"
	model.peopleState.inboxTypes = []peopleInboxType{{key: "beeper", sources: []query.PersonInboxRow{source}}}

	model, cmd := sendKey(t, model, keyEnter())
	model = sendMsg(t, model, runPeopleCommandMessage[peopleConversationsLoadedMsg](t, cmd))
	require.Len(model.peopleState.conversations, peoplePageSize)

	model, cmd = sendKey(t, model, key('G'))
	require.NotNil(cmd, "reaching the end should request the next source page")
	assert.Equal(peoplePageSize-1, model.peopleState.cursor)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleConversationsLoadedMsg](t, cmd))
	require.Len(model.peopleState.conversations, 2*peoplePageSize)
	assert.Equal(peoplePageSize-1, model.peopleState.cursor)

	model, cmd = sendKey(t, model, key('G'))
	require.NotNil(cmd)
	model = sendMsg(t, model, runPeopleCommandMessage[peopleConversationsLoadedMsg](t, cmd))
	assert.Len(model.peopleState.conversations, 205)
	assert.True(model.peopleState.conversationsComplete)
	require.Len(backend.conversationRequests, 3)
	assert.Equal([]int{0, 100, 200}, []int{
		backend.conversationRequests[0].filter.Pagination.Offset,
		backend.conversationRequests[1].filter.Pagination.Offset,
		backend.conversationRequests[2].filter.Pagination.Offset,
	})
}

func TestPeopleInboxPaginatesAllConversationMessagesAndDeduplicatesBoundary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := inboxTestContact()
	source := query.PersonInboxRow{SourceID: 21, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp}
	first := make([]query.MessageSummary, peoplePageSize)
	second := make([]query.MessageSummary, peoplePageSize)
	for i := range first {
		first[i] = query.MessageSummary{ID: int64(i + 1), ConversationID: 101}
		second[i] = query.MessageSummary{ID: int64(i + 100), ConversationID: 101}
	}
	backend := &fakePeopleInboxBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		messagePages: map[int]*peoplebrowser.ConversationMessagePage{
			0:   {Rows: first, NextOffset: 100, CacheRevision: "r1"},
			100: {Rows: second, NextOffset: 200, Complete: true, CacheRevision: "r1"},
		},
	}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.level = peopleLevelConversations
	model.peopleState.tab = peopleTabInboxes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.selectedInboxSource = &source
	model.peopleState.conversations = []query.ConversationRow{{ConversationID: 101}}

	model, cmd := sendKey(t, model, keyEnter())
	model = sendMsg(t, model, runPeopleCommandMessage[peopleConversationMessagesLoadedMsg](t, cmd))
	require.Len(model.textState.messages, peoplePageSize)

	model, cmd = sendKey(t, model, key('G'))
	require.NotNil(cmd, "reaching the end should request the next message page")
	model = sendMsg(t, model, runPeopleCommandMessage[peopleConversationMessagesLoadedMsg](t, cmd))
	assert.Len(model.textState.messages, 199, "message 100 appears in both pages")
	assert.True(model.peopleState.conversationComplete)
	require.Len(backend.messageRequests, 2)
	assert.Equal(peoplePageSize, backend.messageRequests[1].filter.Pagination.Offset)
}

func TestPeopleInboxDrillSettlesAncestorAppend(t *testing.T) {
	contact := inboxTestContact()
	source := query.PersonInboxRow{SourceID: 21, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp}

	t.Run("conversation append while opening conversation", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		conversations := []query.ConversationRow{
			{ConversationID: 101, Title: "First"},
			{ConversationID: 102, Title: "Selected"},
		}
		backend := &fakePeopleInboxBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			conversationPages: map[int]*peoplebrowser.ConversationPage{
				100: {Rows: []query.ConversationRow{{ConversationID: 103, Title: "Stale append"}}, Complete: true, CacheRevision: "r1"},
			},
			messages: []query.MessageSummary{{ID: 501, ConversationID: 102}},
		}
		model := peopleModel(backend)
		model.mode = modePeople
		model.peopleState.level = peopleLevelConversations
		model.peopleState.tab = peopleTabInboxes
		model.peopleState.participantID = contact.ID
		model.peopleState.contact = &contact
		model.peopleState.selectedInboxSource = &source
		model.peopleState.conversations = slices.Clone(conversations)
		model.textState.conversations = slices.Clone(conversations)
		model.peopleState.cursor = 1
		model.peopleState.conversationsNextOffset = 100
		model.peopleState.conversationsCacheRevision = "r1"

		pendingAppend := model.maybeLoadMorePeopleInbox()
		require.NotNil(pendingAppend)
		require.True(model.peopleState.conversationsLoading)

		model, childLoad := sendKey(t, model, keyEnter())
		require.NotNil(childLoad)
		assert.Equal(peopleLevelConversation, model.peopleState.level)
		assert.Equal(conversations, model.peopleState.conversations)
		assert.False(model.peopleState.conversationsLoading)
		assert.True(model.peopleState.conversationLoading)
		assert.True(model.loading)

		stale := runPeopleCommandMessage[peopleConversationsLoadedMsg](t, pendingAppend)
		model = sendMsg(t, model, stale)
		assert.Equal(conversations, model.peopleState.conversations)
		assert.False(model.peopleState.conversationsLoading)

		model, _ = sendKey(t, model, keyEsc())
		assert.Equal(peopleLevelConversations, model.peopleState.level)
		assert.Equal(conversations, model.peopleState.conversations)
		assert.Equal(100, model.peopleState.conversationsNextOffset)
		assert.Equal("r1", model.peopleState.conversationsCacheRevision)
		resumed := model.maybeLoadMorePeopleInbox()
		assert.NotNil(resumed)
		assert.True(model.peopleState.conversationsLoading)
	})

	t.Run("message append while opening detail", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		messages := []query.MessageSummary{
			{ID: 501, ConversationID: 101, Snippet: "First"},
			{ID: 502, ConversationID: 101, Snippet: "Selected"},
		}
		backend := &fakePeopleInboxBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			messagePages: map[int]*peoplebrowser.ConversationMessagePage{
				100: {Rows: []query.MessageSummary{{ID: 503, ConversationID: 101}}, Complete: true, CacheRevision: "r1"},
			},
			detail: &query.MessageDetail{ID: 502, ConversationID: 101, BodyText: "Full detail"},
		}
		model := peopleModel(backend)
		model.mode = modePeople
		model.peopleState.level = peopleLevelConversation
		model.peopleState.tab = peopleTabInboxes
		model.peopleState.participantID = contact.ID
		model.peopleState.contact = &contact
		model.peopleState.selectedInboxSource = &source
		model.peopleState.selectedConversationID = 101
		model.textState.messages = slices.Clone(messages)
		model.textState.cursor = 1
		model.peopleState.conversationNextOffset = 100
		model.peopleState.conversationCacheRevision = "r1"

		pendingAppend := model.maybeLoadMorePeopleInbox()
		require.NotNil(pendingAppend)
		require.True(model.peopleState.conversationLoading)

		model, detailLoad := sendKey(t, model, keyEnter())
		require.NotNil(detailLoad)
		assert.Equal(peopleLevelMessage, model.peopleState.level)
		assert.Equal(messages, model.textState.messages)
		assert.False(model.peopleState.conversationLoading)
		assert.True(model.peopleState.messageLoading)
		assert.True(model.loading)

		stale := runPeopleCommandMessage[peopleConversationMessagesLoadedMsg](t, pendingAppend)
		model = sendMsg(t, model, stale)
		assert.Equal(messages, model.textState.messages)
		assert.False(model.peopleState.conversationLoading)

		model, _ = sendKey(t, model, keyEsc())
		assert.Equal(peopleLevelConversation, model.peopleState.level)
		assert.Equal(messages, model.textState.messages)
		assert.Equal(100, model.peopleState.conversationNextOffset)
		assert.Equal("r1", model.peopleState.conversationCacheRevision)
		resumed := model.maybeLoadMorePeopleInbox()
		assert.NotNil(resumed)
		assert.True(model.peopleState.conversationLoading)
	})
}

func TestPeopleInboxPageRevisionDriftRestartsOnceThenPauses(t *testing.T) {
	contact := inboxTestContact()
	source := query.PersonInboxRow{SourceID: 21, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp}

	t.Run("source conversations", func(t *testing.T) {
		assert := assert.New(t)
		model := peopleModel(&fakePeopleInboxBackend{fakePeopleBackend: &fakePeopleBackend{}})
		model.mode = modePeople
		model.peopleState.level = peopleLevelConversations
		model.peopleState.tab = peopleTabInboxes
		model.peopleState.participantID = contact.ID
		model.peopleState.contact = &contact
		model.peopleState.selectedInboxSource = &source
		model.peopleState.requestID = 5
		model.peopleState.conversations = []query.ConversationRow{{ConversationID: 1}}
		model.peopleState.conversationsCacheRevision = "r1"
		model.peopleState.conversationsPendingOffset = 100

		updated, restart := model.Update(peopleConversationsLoadedMsg{
			conversations: []query.ConversationRow{{ConversationID: 2}}, offset: 100,
			nextOffset: 200, cacheRevision: "r2", requestID: 5,
			participantID: contact.ID, sourceID: source.SourceID,
			presentationGeneration: model.presentationGeneration,
		})
		model = asModel(t, updated)
		require.NotNil(t, restart)
		assert.True(model.peopleState.conversationsRestarted)
		assert.Equal(uint64(6), model.peopleState.requestID)

		model = sendMsg(t, model, peopleConversationsLoadedMsg{
			conversations: []query.ConversationRow{{ConversationID: 3}}, offset: 0,
			nextOffset: 100, cacheRevision: "r2", requestID: 6,
			participantID: contact.ID, sourceID: source.SourceID,
			presentationGeneration: model.presentationGeneration,
		})
		model.peopleState.conversationsPendingOffset = 100
		model = sendMsg(t, model, peopleConversationsLoadedMsg{
			conversations: []query.ConversationRow{{ConversationID: 4}}, offset: 100,
			nextOffset: 200, cacheRevision: "r3", requestID: 6,
			participantID: contact.ID, sourceID: source.SourceID,
			presentationGeneration: model.presentationGeneration,
		})
		require.ErrorIs(t, model.peopleState.inboxErr, errPeopleContentChanged)
		assert.Equal(int64(3), model.peopleState.conversations[0].ConversationID)
	})

	t.Run("conversation messages", func(t *testing.T) {
		assert := assert.New(t)
		model := peopleModel(&fakePeopleInboxBackend{fakePeopleBackend: &fakePeopleBackend{}})
		model.mode = modePeople
		model.peopleState.level = peopleLevelConversation
		model.peopleState.tab = peopleTabInboxes
		model.peopleState.participantID = contact.ID
		model.peopleState.contact = &contact
		model.peopleState.selectedInboxSource = &source
		model.peopleState.selectedConversationID = 101
		model.peopleState.requestID = 8
		model.textState.messages = []query.MessageSummary{{ID: 1}}
		model.peopleState.conversationCacheRevision = "r1"
		model.peopleState.conversationPendingOffset = 100

		updated, restart := model.Update(peopleConversationMessagesLoadedMsg{
			messages: []query.MessageSummary{{ID: 2}}, offset: 100,
			nextOffset: 200, cacheRevision: "r2", requestID: 8,
			participantID: contact.ID, sourceID: source.SourceID, conversationID: 101,
			presentationGeneration: model.presentationGeneration,
		})
		model = asModel(t, updated)
		require.NotNil(t, restart)
		assert.True(model.peopleState.conversationRestarted)
		model = sendMsg(t, model, peopleConversationMessagesLoadedMsg{
			messages: []query.MessageSummary{{ID: 3}}, offset: 0,
			nextOffset: 100, cacheRevision: "r2", requestID: 9,
			participantID: contact.ID, sourceID: source.SourceID, conversationID: 101,
			presentationGeneration: model.presentationGeneration,
		})
		model.peopleState.conversationPendingOffset = 100
		model = sendMsg(t, model, peopleConversationMessagesLoadedMsg{
			messages: []query.MessageSummary{{ID: 4}}, offset: 100,
			nextOffset: 200, cacheRevision: "r3", requestID: 9,
			participantID: contact.ID, sourceID: source.SourceID, conversationID: 101,
			presentationGeneration: model.presentationGeneration,
		})
		require.ErrorIs(t, model.peopleState.inboxErr, errPeopleContentChanged)
		assert.Equal(int64(3), model.textState.messages[0].ID)
	})
}

func TestPeopleInboxAsyncResultsRequireFullNavigationContext(t *testing.T) {
	contact := inboxTestContact()
	source := query.PersonInboxRow{
		SourceID: 21, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp,
	}
	newModel := func(level peopleLevel) Model {
		model := peopleModel(&fakePeopleBackend{})
		model.mode = modePeople
		model.presentationGeneration = 9
		model.peopleState.requestID = 5
		model.peopleState.level = level
		model.peopleState.tab = peopleTabInboxes
		model.peopleState.participantID = contact.ID
		model.peopleState.contact = &contact
		model.peopleState.selectedInboxSource = &source
		model.peopleState.selectedConversationID = 101
		model.peopleState.selectedMessageID = 501
		return model
	}

	t.Run("inboxes reject a different contact", func(t *testing.T) {
		model := newModel(peopleLevelInboxTypes)
		model.peopleState.inboxTypes = []peopleInboxType{{key: "old", label: "Old"}}
		model = sendMsg(t, model, peopleInboxesLoadedMsg{
			inboxes: &query.PersonInboxResponse{Rows: []query.PersonInboxRow{{
				SourceID: 99, SourceType: "stale", SourceIdentifier: "stale",
			}}},
			requestID: 5, participantID: contact.ID + 1, presentationGeneration: 9,
		})
		require.Len(t, model.peopleState.inboxTypes, 1)
		assert.Equal(t, "old", model.peopleState.inboxTypes[0].key)
	})

	t.Run("inboxes reject stale selected contact data", func(t *testing.T) {
		model := newModel(peopleLevelInboxTypes)
		staleContact := contact
		staleContact.ID++
		model.peopleState.contact = &staleContact
		model.peopleState.inboxTypes = []peopleInboxType{{key: "old", label: "Old"}}
		model = sendMsg(t, model, peopleInboxesLoadedMsg{
			inboxes: &query.PersonInboxResponse{Rows: []query.PersonInboxRow{{
				SourceID: 99, SourceType: "stale", SourceIdentifier: "stale",
			}}},
			requestID: 5, participantID: contact.ID, presentationGeneration: 9,
		})
		require.Len(t, model.peopleState.inboxTypes, 1)
		assert.Equal(t, "old", model.peopleState.inboxTypes[0].key)
	})

	t.Run("conversations reject a different source", func(t *testing.T) {
		model := newModel(peopleLevelConversations)
		model.peopleState.conversations = []query.ConversationRow{{ConversationID: 1, Title: "Old"}}
		model = sendMsg(t, model, peopleConversationsLoadedMsg{
			conversations: []query.ConversationRow{{ConversationID: 2, Title: "Stale"}},
			requestID:     5, participantID: contact.ID, sourceID: source.SourceID + 1,
			presentationGeneration: 9,
		})
		require.Len(t, model.peopleState.conversations, 1)
		assert.Equal(t, "Old", model.peopleState.conversations[0].Title)
	})

	t.Run("messages reject a different conversation", func(t *testing.T) {
		model := newModel(peopleLevelConversation)
		model.textState.messages = []query.MessageSummary{{ID: 1, Snippet: "Old"}}
		model = sendMsg(t, model, peopleConversationMessagesLoadedMsg{
			messages:  []query.MessageSummary{{ID: 2, Snippet: "Stale"}},
			requestID: 5, participantID: contact.ID, sourceID: source.SourceID,
			conversationID: 102, presentationGeneration: 9,
		})
		require.Len(t, model.textState.messages, 1)
		assert.Equal(t, "Old", model.textState.messages[0].Snippet)
	})

	t.Run("detail rejects a different message and presentation", func(t *testing.T) {
		model := newModel(peopleLevelMessage)
		model.messageDetail = &query.MessageDetail{ID: 501, BodyText: "Old"}
		model = sendMsg(t, model, peopleMessageLoadedMsg{
			detail:    &query.MessageDetail{ID: 502, BodyText: "Stale"},
			requestID: 5, participantID: contact.ID, sourceID: source.SourceID,
			conversationID: 101, messageID: 502, presentationGeneration: 8,
		})
		require.NotNil(t, model.messageDetail)
		assert.Equal(t, "Old", model.messageDetail.BodyText)
	})
}

func TestPeopleInboxMessageDetailFindKeepsGlobalKeysInInput(t *testing.T) {
	contact := inboxTestContact()
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.peopleState.level = peopleLevelMessage
	model.peopleState.tab = peopleTabInboxes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.messageDetail = &query.MessageDetail{ID: 501, BodyText: "message body"}

	model, _ = sendKey(t, model, key('/'))
	require.True(t, model.detailSearchActive)
	model, _ = sendKey(t, model, key('m'))

	assert.Equal(t, modePeople, model.mode)
	assert.Equal(t, "m", model.detailSearchInput.Value())
}

func TestPeopleInboxLeavingDuringLoadReturnsToRetryableSameSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := inboxTestContact()
	source := query.PersonInboxRow{
		SourceID: 21, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp,
	}
	backend := &fakePeopleInboxBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		conversations: []query.ConversationRow{{
			ConversationID: 101, Title: "Resumed chat",
		}},
	}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.level = peopleLevelConversations
	model.peopleState.tab = peopleTabInboxes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.selectedInboxType = "beeper"
	model.peopleState.selectedInboxSource = &source
	model.peopleState.conversationsLoading = true
	model.loading = true

	model, _ = sendKey(t, model, key('m'))
	assert.Equal(modeEmail, model.mode)
	assert.False(model.peopleState.conversationsLoading)
	require.Error(model.peopleState.inboxErr)

	model.mode = modeMeetings
	model, _ = sendKey(t, model, key('m'))
	assert.Equal(modePeople, model.mode)
	assert.Equal(contact.ID, model.peopleState.participantID)
	require.NotNil(model.peopleState.selectedInboxSource)
	assert.Equal(source.SourceID, model.peopleState.selectedInboxSource.SourceID)

	model, cmd := sendKey(t, model, key('r'))
	require.NotNil(cmd)
	loaded := runPeopleCommandMessage[peopleConversationsLoadedMsg](t, cmd)
	model = sendMsg(t, model, loaded)
	require.NoError(model.peopleState.inboxErr)
	assert.Contains(model.renderPeopleView(), "Resumed chat")
}

func TestPeopleInboxBackRestoresEveryListPosition(t *testing.T) {
	assert := assert.New(t)
	contact := inboxTestContact()
	latest := time.Date(2026, 8, 20, 17, 45, 0, 0, time.UTC)
	backend := &fakePeopleInboxBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		conversations: []query.ConversationRow{
			{ConversationID: 101, Title: "First chat", LastMessageAt: latest},
			{ConversationID: 102, Title: "Second chat", LastMessageAt: latest.Add(-time.Hour)},
		},
		messages: []query.MessageSummary{
			{ID: 501, ConversationID: 102, Snippet: "First message", SentAt: latest},
			{ID: 502, ConversationID: 102, Snippet: "Second message", SentAt: latest.Add(-time.Minute)},
		},
		detail: &query.MessageDetail{
			ID: 502, SourceID: 22, ConversationID: 102, BodyText: "Second detail",
		},
	}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.level = peopleLevelInboxSources
	model.peopleState.tab = peopleTabInboxes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.selectedInboxType = "beeper"
	model.peopleState.inboxTypes = []peopleInboxType{{
		key: "beeper", label: "Beeper", sources: []query.PersonInboxRow{
			{SourceID: 21, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp, LatestAt: latest},
			{SourceID: 22, SourceType: "beeper", SourceIdentifier: "signal", LatestAt: latest.Add(-time.Hour)},
		},
	}}
	model.peopleState.cursor = 1
	model.peopleState.scrollOffset = 1

	model, cmd := sendKey(t, model, keyEnter())
	model = sendMsg(t, model, runPeopleCommandMessage[peopleConversationsLoadedMsg](t, cmd))
	model, _ = sendKey(t, model, key('j'))
	assert.Equal(1, model.peopleState.cursor)

	model, cmd = sendKey(t, model, keyEnter())
	model = sendMsg(t, model, runPeopleCommandMessage[peopleConversationMessagesLoadedMsg](t, cmd))
	model, _ = sendKey(t, model, key('j'))
	assert.Equal(1, model.textState.cursor)

	model, cmd = sendKey(t, model, keyEnter())
	model = sendMsg(t, model, runPeopleCommandMessage[peopleMessageLoadedMsg](t, cmd))
	model, _ = sendKey(t, model, keyEsc())
	assert.Equal(peopleLevelConversation, model.peopleState.level)
	assert.Equal(1, model.textState.cursor)

	model, _ = sendKey(t, model, keyEsc())
	assert.Equal(peopleLevelConversations, model.peopleState.level)
	assert.Equal(1, model.peopleState.cursor)

	model, _ = sendKey(t, model, keyEsc())
	assert.Equal(peopleLevelInboxSources, model.peopleState.level)
	assert.Equal(1, model.peopleState.cursor)
	assert.Equal(1, model.peopleState.scrollOffset)
}

func TestPeopleInboxListsKeepDeepSelectionVisibleAtConstrainedHeight(t *testing.T) {
	contact := inboxTestContact()
	tests := []struct {
		name      string
		level     peopleLevel
		configure func(*Model)
		want      string
	}{
		{
			name:  "source types",
			level: peopleLevelInboxTypes,
			configure: func(model *Model) {
				model.peopleState.inboxTypes = []peopleInboxType{
					{key: "type-0", label: "Type 0"},
					{key: "type-1", label: "Type 1"},
					{key: "type-2", label: "Type 2"},
					{key: "type-3", label: "Type 3"},
					{key: "type-4", label: "Type 4"},
					{key: "type-5", label: "Type 5"},
				}
			},
			want: "▶ Type 5",
		},
		{
			name:  "concrete sources",
			level: peopleLevelInboxSources,
			configure: func(model *Model) {
				model.peopleState.selectedInboxType = "beeper"
				model.peopleState.inboxTypes = []peopleInboxType{{
					key: "beeper", label: "Beeper", sources: []query.PersonInboxRow{
						{SourceID: 20, SourceIdentifier: "network-0"},
						{SourceID: 21, SourceIdentifier: "network-1"},
						{SourceID: 22, SourceIdentifier: "network-2"},
						{SourceID: 23, SourceIdentifier: "network-3"},
						{SourceID: 24, SourceIdentifier: "network-4"},
						{SourceID: 25, SourceIdentifier: "network-5"},
					},
				}}
			},
			want: "▶ Network 5",
		},
		{
			name:  "conversations",
			level: peopleLevelConversations,
			configure: func(model *Model) {
				model.peopleState.conversations = []query.ConversationRow{
					{ConversationID: 100, Title: "Chat 0"},
					{ConversationID: 101, Title: "Chat 1"},
					{ConversationID: 102, Title: "Chat 2"},
					{ConversationID: 103, Title: "Chat 3"},
					{ConversationID: 104, Title: "Chat 4"},
					{ConversationID: 105, Title: "Chat 5"},
				}
			},
			want: "▶ Chat 5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := peopleModel(&fakePeopleBackend{})
			model.mode = modePeople
			model.pageSize = 5
			model.width = 52
			model.peopleState.level = tt.level
			model.peopleState.tab = peopleTabInboxes
			model.peopleState.participantID = contact.ID
			model.peopleState.contact = &contact
			tt.configure(&model)

			model, _ = sendKey(t, model, key('G'))

			assert.Equal(t, 5, model.peopleState.cursor)
			assert.Equal(t, 4, model.peopleState.scrollOffset)
			assert.Contains(t, stripANSI(model.renderPeopleView()), tt.want)
		})
	}
}

func TestPeopleInboxBackRestoresVisibleSourceAtConstrainedHeight(t *testing.T) {
	assert := assert.New(t)
	contact := inboxTestContact()
	model := peopleModel(&fakePeopleInboxBackend{fakePeopleBackend: &fakePeopleBackend{}})
	model.mode = modePeople
	model.pageSize = 5
	model.width = 52
	model.peopleState.level = peopleLevelInboxSources
	model.peopleState.tab = peopleTabInboxes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.selectedInboxType = "beeper"
	model.peopleState.inboxTypes = []peopleInboxType{{
		key: "beeper", label: "Beeper", sources: []query.PersonInboxRow{
			{SourceID: 20, SourceIdentifier: "network-0"},
			{SourceID: 21, SourceIdentifier: "network-1"},
			{SourceID: 22, SourceIdentifier: "network-2"},
			{SourceID: 23, SourceIdentifier: "network-3"},
			{SourceID: 24, SourceIdentifier: "network-4"},
			{SourceID: 25, SourceIdentifier: "network-5"},
		},
	}}

	model, _ = sendKey(t, model, key('G'))
	model, _ = sendKey(t, model, keyEnter())
	model, _ = sendKey(t, model, keyEsc())

	assert.Equal(peopleLevelInboxSources, model.peopleState.level)
	assert.Equal(5, model.peopleState.cursor)
	assert.Equal(4, model.peopleState.scrollOffset)
	assert.Contains(stripANSI(model.renderPeopleView()), "▶ Network 5")
}

func TestPeopleInboxTabLeaveSettlesPendingLoadAndReentryStartsFresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := inboxTestContact()
	backend := &fakePeopleInboxBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		inboxes: &query.PersonInboxResponse{Rows: []query.PersonInboxRow{{
			SourceID: 21, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp,
		}}},
	}
	model := peopleModel(backend)
	model.mode = modePeople
	model.peopleState.level = peopleLevelContact
	model.peopleState.tab = peopleTabAttributes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact

	updated, pending := model.activatePeopleTab(peopleTabInboxes)
	model = asModel(t, updated)
	require.NotNil(pending)
	require.True(model.peopleState.inboxesLoading)

	model, _ = sendKey(t, model, keyTab())

	assert.Equal(peopleTabMeetings, model.peopleState.tab)
	assert.Equal(peopleLevelContact, model.peopleState.level)
	assert.False(model.peopleState.inboxesLoading)
	assert.False(model.peopleState.conversationsLoading)
	assert.False(model.peopleState.conversationLoading)
	assert.False(model.peopleState.messageLoading)
	assert.False(model.loading)

	stale := runPeopleCommandMessage[peopleInboxesLoadedMsg](t, pending)
	model = sendMsg(t, model, stale)
	assert.False(model.peopleState.inboxesLoaded)
	assert.Empty(model.peopleState.inboxTypes)

	model, fresh := sendKey(t, model, keyShiftTab())

	require.NotNil(fresh)
	assert.Equal(peopleTabInboxes, model.peopleState.tab)
	assert.True(model.peopleState.inboxesLoading)
	loaded := runPeopleCommandMessage[peopleInboxesLoadedMsg](t, fresh)
	model = sendMsg(t, model, loaded)
	require.Len(backend.inboxRequests, 2)
	assert.Equal([]int64{contact.ID, contact.ID}, backend.inboxRequests)
	assert.True(model.peopleState.inboxesLoaded)
	require.Len(model.peopleState.inboxTypes, 1)
	assert.Equal("beeper", model.peopleState.inboxTypes[0].key)
}

func TestPeopleInboxTabLeaveSettlesEverySubloadWithoutDiscardingCache(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := inboxTestContact()
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.loading = true
	model.peopleState.level = peopleLevelInboxTypes
	model.peopleState.tab = peopleTabInboxes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.inboxesLoaded = true
	model.peopleState.inboxTypes = []peopleInboxType{{key: "beeper", label: "Beeper"}}
	model.peopleState.conversations = []query.ConversationRow{{
		ConversationID: 101, Title: "Cached chat",
	}}
	model.peopleState.inboxesLoading = true
	model.peopleState.conversationsLoading = true
	model.peopleState.conversationLoading = true
	model.peopleState.messageLoading = true

	updated, _ := model.activatePeopleTab(peopleTabMeetings)
	model = asModel(t, updated)

	assert.False(model.peopleState.inboxesLoading)
	assert.False(model.peopleState.conversationsLoading)
	assert.False(model.peopleState.conversationLoading)
	assert.False(model.peopleState.messageLoading)
	assert.False(model.loading)
	assert.True(model.peopleState.inboxesLoaded)
	require.Len(model.peopleState.inboxTypes, 1)
	assert.Equal("beeper", model.peopleState.inboxTypes[0].key)
	require.Len(model.peopleState.conversations, 1)
	assert.Equal("Cached chat", model.peopleState.conversations[0].Title)
}
