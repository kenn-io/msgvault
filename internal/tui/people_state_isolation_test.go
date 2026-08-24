package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

func cycleToMode(t *testing.T, model Model, target tuiMode) Model {
	t.Helper()
	for range 4 {
		if model.mode == target {
			return model
		}
		updated, _, handled := model.handleGlobalKeys(key('m'))
		require.True(t, handled)
		model = updated
	}
	require.Equal(t, target, model.mode)
	return model
}

func TestPeopleInboxKeepsTextWorkspaceIndependentAcrossModeSwitches(t *testing.T) {
	assert := assert.New(t)
	textSourceID := int64(71)
	model := peopleModel(&fakePeopleBackend{})
	model.textEngine = meetingModeTextEngine{}
	model.mode = modeTexts
	model.accountFilter = &textSourceID
	model.textState = textState{
		viewType: query.TextViewContacts,
		level:    textLevelDetail,
		conversations: []query.ConversationRow{
			{ConversationID: 701, Title: "Independent text conversation"},
		},
		aggregateRows: []query.AggregateRow{{Key: "Independent text contact", Count: 3}},
		messages: []query.MessageSummary{
			{ID: 711, ConversationID: 701, Snippet: "first independent row"},
			{ID: 712, ConversationID: 701, Snippet: "second independent row"},
			{ID: 713, ConversationID: 701, Snippet: "third independent row"},
		},
		cursor:            2,
		scrollOffset:      1,
		selectedConvID:    701,
		selectedMessageID: 713,
		filter: query.TextFilter{
			SourceID:     &textSourceID,
			ContactPhone: "+15550000071",
			SortField:    query.TextSortByName,
		},
	}

	model = cycleToMode(t, model, modePeople)
	contact := inboxTestContact()
	peopleSource := query.PersonInboxRow{
		SourceID: 21, SourceType: "beeper", SourceIdentifier: sourceTypeWhatsApp,
	}
	model.peopleState.level = peopleLevelConversation
	model.peopleState.tab = peopleTabInboxes
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.selectedInboxSource = &peopleSource
	model.peopleState.selectedConversationID = 101
	model.peopleState.requestID = 8
	model.peopleState.conversationPendingOffset = 0
	model.peopleState.conversationLoading = true
	model.textState.level = textLevelTimeline
	model.textState.selectedConvID = 101
	model.textState.filter = peopleConversationFilter(&contact, peopleSource, 0)
	model = sendMsg(t, model, peopleConversationMessagesLoadedMsg{
		messages: []query.MessageSummary{
			{ID: 501, ConversationID: 101, Snippet: "first contact row"},
			{ID: 502, ConversationID: 101, Snippet: "second contact row"},
			{ID: 503, ConversationID: 101, Snippet: "third contact row"},
		},
		sourceID: 21, conversationID: 101, participantID: contact.ID,
		requestID: model.peopleState.requestID, presentationGeneration: model.presentationGeneration,
	})
	model.textState.cursor = 2
	model.textState.scrollOffset = 1
	peopleText := model.textState

	stalePeoplePage := peopleConversationMessagesLoadedMsg{
		messages: []query.MessageSummary{{ID: 999, ConversationID: 101, Snippet: "stale contact row"}},
		sourceID: 21, conversationID: 101, participantID: contact.ID,
		requestID: model.peopleState.requestID, presentationGeneration: model.presentationGeneration,
	}
	model = cycleToMode(t, model, modeTexts)
	model = sendMsg(t, model, stalePeoplePage)

	assert.Equal(textLevelDetail, model.textState.level)
	assert.Equal(query.TextViewContacts, model.textState.viewType)
	assert.Equal(int64(701), model.textState.selectedConvID)
	assert.Equal(int64(713), model.textState.selectedMessageID)
	assert.Equal(2, model.textState.cursor)
	assert.Equal(1, model.textState.scrollOffset)
	require.NotNil(t, model.textState.filter.SourceID)
	assert.Equal(textSourceID, *model.textState.filter.SourceID)
	assert.Equal("+15550000071", model.textState.filter.ContactPhone)
	assert.Empty(model.textState.filter.ParticipantIDs)
	assert.Equal([]int64{711, 712, 713}, messageSummaryIDs(model.textState.messages))
	assert.Equal([]int64{701}, conversationRowIDs(model.textState.conversations))

	model = cycleToMode(t, model, modePeople)
	assert.Equal(peopleText.level, model.textState.level)
	assert.Equal(peopleText.selectedConvID, model.textState.selectedConvID)
	assert.Equal(peopleText.cursor, model.textState.cursor)
	assert.Equal(peopleText.scrollOffset, model.textState.scrollOffset)
	assert.Equal(peopleText.filter.ParticipantIDs, model.textState.filter.ParticipantIDs)
	assert.Equal([]int64{501, 502, 503}, messageSummaryIDs(model.textState.messages))
	assert.NotContains(messageSummaryIDs(model.textState.messages), int64(999))

	model = cycleToMode(t, model, modeTexts)
	assert.Equal([]int64{711, 712, 713}, messageSummaryIDs(model.textState.messages))
	assert.Empty(model.textState.filter.ParticipantIDs)
}

func TestStaleTextCompletionsCannotReplacePeopleInboxState(t *testing.T) {
	want := textState{
		level:          textLevelTimeline,
		selectedConvID: 101,
		messages: []query.MessageSummary{
			{ID: 501, ConversationID: 101, Snippet: "people inbox row"},
		},
		conversations: []query.ConversationRow{
			{ConversationID: 101, Title: "People inbox"},
		},
		aggregateRows: []query.AggregateRow{{Key: "people", Count: 1}},
		stats:         &query.TotalStats{MessageCount: 1},
	}

	completions := []struct {
		name string
		msg  tea.Msg
	}{
		{
			name: "conversations",
			msg: textConversationsLoadedMsg{
				conversations: []query.ConversationRow{{ConversationID: 999}},
				stats:         &query.TotalStats{MessageCount: 999}, requestID: 7,
				presentationGeneration: 4,
			},
		},
		{
			name: "aggregates",
			msg: textAggregateLoadedMsg{
				rows:  []query.AggregateRow{{Key: "stale", Count: 999}},
				stats: &query.TotalStats{MessageCount: 999}, requestID: 7,
				presentationGeneration: 4,
			},
		},
		{
			name: "messages",
			msg: textMessagesLoadedMsg{
				messages: []query.MessageSummary{{ID: 999}}, requestID: 7,
				presentationGeneration: 4,
			},
		},
		{
			name: "search",
			msg: textSearchResultMsg{
				messages: []query.MessageSummary{{ID: 999}}, requestID: 7,
				presentationGeneration: 4,
			},
		},
	}

	for _, tc := range completions {
		t.Run(tc.name, func(t *testing.T) {
			model := NewBuilder().Build()
			model.mode = modePeople
			model.presentationGeneration = 5
			model.textRequestID = 7
			model.textState = want

			updated := sendMsg(t, model, tc.msg)

			assert.Equal(t, want, updated.textState)
		})
	}
}

func messageSummaryIDs(rows []query.MessageSummary) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func conversationRowIDs(rows []query.ConversationRow) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ConversationID)
	}
	return ids
}

func TestPeopleMessageReaderIsIndependentFromTextReader(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := contentTestContact()
	model := peopleContentModel(
		&fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}, &contact,
	)
	model.textEngine = meetingModeTextEngine{}
	peopleDetail := &query.MessageDetail{
		ID: 81, MessageType: sourceTypeWhatsApp, Subject: "Contact activity",
		BodyText: "people needle " + strings.Repeat("contact context ", 20),
	}
	model.peopleState.tab = peopleTabActivity
	model.peopleState.level = peopleLevelActivityMessage
	model.peopleState.selectedContentMessage = peopleDetail.ID
	model.peopleState.requestID = 5
	model = sendMsg(t, model, peopleActivityMessageLoadedMsg{
		detail: peopleDetail, messageID: peopleDetail.ID, participantID: contact.ID,
		requestID: model.peopleState.requestID, presentationGeneration: model.presentationGeneration,
	})
	model.detailSearchQuery = "people needle"
	model.detailSearchInput.SetValue("people needle")
	model.findDetailMatches()
	model.detailScroll = 2
	peopleMatches := slices.Clone(model.detailSearchMatches)

	model = cycleToMode(t, model, modeTexts)
	textDetail := &query.MessageDetail{
		ID: 91, ConversationID: 901, MessageType: "sms", Subject: "Independent text",
		BodyText: "text needle " + strings.Repeat("text context ", 20),
	}
	model.textState.level = textLevelDetail
	model.textState.selectedConvID = textDetail.ConversationID
	model.textState.selectedMessageID = textDetail.ID
	model.textRequestID = 12
	model = sendMsg(t, model, textMessageLoadedMsg{
		detail: textDetail, requestID: model.textRequestID,
		conversationID: textDetail.ConversationID, messageID: textDetail.ID,
		presentationGeneration: model.presentationGeneration,
	})
	model.detailSearchQuery = "text needle"
	model.detailSearchInput.SetValue("text needle")
	model.findDetailMatches()
	model.detailScroll = 3
	textMatches := slices.Clone(model.detailSearchMatches)

	model = cycleToMode(t, model, modePeople)
	require.NotNil(model.messageDetail)
	assert.Equal(peopleDetail.ID, model.messageDetail.ID)
	assert.Equal("people needle", model.detailSearchQuery)
	assert.Equal("people needle", model.detailSearchInput.Value())
	assert.Equal(peopleMatches, model.detailSearchMatches)
	assert.Equal(2, model.detailScroll)

	model = cycleToMode(t, model, modeTexts)
	require.NotNil(model.messageDetail)
	assert.Equal(textDetail.ID, model.messageDetail.ID)
	assert.Equal("text needle", model.detailSearchQuery)
	assert.Equal("text needle", model.detailSearchInput.Value())
	assert.Equal(textMatches, model.detailSearchMatches)
	assert.Equal(3, model.detailScroll)
}

func TestEmailDetailCompletionWhilePeopleActiveCannotReplacePeopleReader(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := contentTestContact()
	model := peopleContentModel(
		&fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}, &contact,
	)
	peopleDetail := &query.MessageDetail{
		ID: 81, MessageType: sourceTypeWhatsApp, Subject: "Contact message",
		BodyText: "people needle " + strings.Repeat("contact context ", 20),
	}
	model.peopleState.tab = peopleTabInboxes
	model.peopleState.level = peopleLevelMessage
	model.peopleState.selectedInboxSource = &query.PersonInboxRow{SourceID: 21}
	model.peopleState.selectedConversationID = 101
	model.peopleState.selectedMessageID = peopleDetail.ID
	model.peopleState.requestID = 5
	model = sendMsg(t, model, peopleMessageLoadedMsg{
		detail: peopleDetail, messageID: peopleDetail.ID, conversationID: 101,
		sourceID: 21, participantID: contact.ID, requestID: model.peopleState.requestID,
		presentationGeneration: model.presentationGeneration,
	})
	model.detailSearchQuery = "people needle"
	model.detailSearchInput.SetValue("people needle")
	model.findDetailMatches()
	model.detailScroll = 2
	peopleMatches := slices.Clone(model.detailSearchMatches)

	model = cycleToMode(t, model, modeEmail)
	model.detailRequestID = 14
	emailDetail := &query.MessageDetail{
		ID: 92, MessageType: emailMessageType, Subject: "Independent email", BodyText: "email body",
	}
	pendingEmail := messageDetailLoadedMsg{
		detail: emailDetail, requestID: model.detailRequestID,
		presentationGeneration: model.presentationGeneration,
	}
	model = cycleToMode(t, model, modePeople)
	model = sendMsg(t, model, pendingEmail)

	require.NotNil(model.messageDetail)
	assert.Equal(peopleDetail.ID, model.messageDetail.ID)
	assert.Equal("people needle", model.detailSearchQuery)
	assert.Equal("people needle", model.detailSearchInput.Value())
	assert.Equal(peopleMatches, model.detailSearchMatches)
	assert.Equal(2, model.detailScroll)

	model = cycleToMode(t, model, modeEmail)
	require.NotNil(model.messageDetail)
	assert.Equal(emailDetail.ID, model.messageDetail.ID)
	assert.Empty(model.detailSearchQuery)
}

func TestResizeReflowsAllSharedMessageDetailOwners(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Model)
		render    func(Model) string
	}{
		{
			name: "texts",
			configure: func(model *Model) {
				model.mode = modeTexts
				model.textState.level = textLevelDetail
			},
			render: func(model Model) string { return model.renderTextView() },
		},
		{
			name: "people inbox message",
			configure: func(model *Model) {
				model.mode = modePeople
				model.peopleState.level = peopleLevelMessage
				model.peopleState.tab = peopleTabInboxes
			},
			render: func(model Model) string { return model.renderPeopleView() },
		},
		{
			name: "people activity message",
			configure: func(model *Model) {
				model.mode = modePeople
				model.peopleState.level = peopleLevelActivityMessage
				model.peopleState.tab = peopleTabActivity
			},
			render: func(model Model) string { return model.renderPeopleView() },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			model := peopleModel(&fakePeopleBackend{})
			model.width = 28
			model.height = 12
			model.pageSize = 7
			test.configure(&model)
			model.messageDetail = &query.MessageDetail{
				ID: 601, MessageType: sourceTypeWhatsApp, Subject: "Responsive detail",
				BodyText: strings.Repeat("context before the search result ", 8) +
					"needle appears after text that wraps differently",
			}
			model.detailSearchQuery = "needle"
			model.updateDetailLineCount()
			model.findDetailMatches()
			narrowLineCount := model.detailLineCount
			require.NotEmpty(t, model.detailSearchMatches)
			model.detailScroll = 1000

			model = resizeModel(t, model, 100, 12)

			assert.Less(model.detailLineCount, narrowLineCount)
			assert.NotEmpty(model.detailSearchMatches)
			assert.LessOrEqual(model.detailScroll,
				max(model.detailLineCount-model.detailPageSize(), 0))
			assert.Contains(stripANSI(test.render(model)), "needle")
		})
	}
}

func TestResizeReflowsPeopleMeetingDetail(t *testing.T) {
	assert := assert.New(t)
	contact := contentTestContact()
	model := peopleContentModel(
		&fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}, &contact,
	)
	model.width = 28
	model.height = 12
	model.pageSize = 7
	model.peopleState.level = peopleLevelMeetingDetail
	model.peopleState.tab = peopleTabMeetings
	model.meetingState.level = meetingLevelDetail
	model.meetingState.detail = &query.MessageDetail{
		ID: 701, MessageType: meetingMessageType, Subject: "Responsive meeting",
		BodyText: strings.Repeat("context before the search result ", 8) +
			"needle appears after text that wraps differently",
	}
	model.meetingState.detailSearchQuery = "needle"
	model.findMeetingDetailMatches()
	narrowLines := len(model.meetingDetailLines())
	require.NotEmpty(t, model.meetingState.detailSearchMatches)
	model.meetingState.detailScroll = 1000

	model = resizeModel(t, model, 100, 12)

	assert.Less(len(model.meetingDetailLines()), narrowLines)
	assert.NotEmpty(model.meetingState.detailSearchMatches)
	assert.LessOrEqual(model.meetingState.detailScroll,
		max(len(model.meetingDetailLines())-model.detailPageSize(), 0))
	assert.Contains(stripANSI(model.renderPeopleView()), "needle")
}

func TestResizeReflowsParkedPeopleReadersBeforeReentry(t *testing.T) {
	contact := contentTestContact()

	t.Run("activity message", func(t *testing.T) {
		assert := assert.New(t)
		model := peopleContentModel(
			&fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}, &contact,
		)
		model.width = 28
		model.height = 12
		model.pageSize = 7
		model.peopleState.level = peopleLevelActivityMessage
		model.peopleState.tab = peopleTabActivity
		model.peopleState.activityLoaded = true
		model.messageDetail = &query.MessageDetail{
			ID: 801, MessageType: sourceTypeWhatsApp, Subject: "Parked activity",
			BodyText: strings.Repeat("context before the search result ", 8) +
				"needle appears after text that wraps differently",
		}
		model.detailSearchQuery = "needle"
		model.updateDetailLineCount()
		model.findDetailMatches()
		narrowLineCount := model.detailLineCount
		model.detailScroll = 1000

		model = cycleToMode(t, model, modeEmail)
		model = resizeModel(t, model, 100, 12)
		model = cycleToMode(t, model, modePeople)

		assert.Less(model.detailLineCount, narrowLineCount)
		assert.NotEmpty(model.detailSearchMatches)
		assert.LessOrEqual(model.detailScroll,
			max(model.detailLineCount-model.detailPageSize(), 0))
		assert.Contains(stripANSI(model.renderPeopleView()), "needle")
	})

	t.Run("meeting", func(t *testing.T) {
		assert := assert.New(t)
		model := peopleContentModel(
			&fakePeopleContentBackend{fakePeopleBackend: &fakePeopleBackend{}}, &contact,
		)
		model.width = 28
		model.height = 12
		model.pageSize = 7
		model.peopleState.level = peopleLevelMeetingDetail
		model.peopleState.tab = peopleTabMeetings
		model.peopleState.meetingsLoaded = true
		model.meetingState.level = meetingLevelDetail
		model.meetingState.detail = &query.MessageDetail{
			ID: 802, MessageType: meetingMessageType, Subject: "Parked meeting",
			BodyText: strings.Repeat("context before the search result ", 8) +
				"needle appears after text that wraps differently",
		}
		model.meetingState.detailSearchQuery = "needle"
		model.findMeetingDetailMatches()
		narrowLines := len(model.meetingDetailLines())
		model.meetingState.detailScroll = 1000

		model = cycleToMode(t, model, modeEmail)
		model = resizeModel(t, model, 100, 12)
		model = cycleToMode(t, model, modePeople)

		assert.Less(len(model.meetingDetailLines()), narrowLines)
		assert.NotEmpty(model.meetingState.detailSearchMatches)
		assert.LessOrEqual(model.meetingState.detailScroll,
			max(len(model.meetingDetailLines())-model.detailPageSize(), 0))
		assert.Contains(stripANSI(model.renderPeopleView()), "needle")
	})
}
