package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/query"
)

func TestMeetingListViewShowsMeetingColumns(t *testing.T) {
	model := NewBuilder().WithAccounts(
		query.AccountInfo{ID: 2, SourceType: "granola", Identifier: "work-notes"},
	).WithSize(120, 24).Build()
	model.mode = modeMeetings
	model.loading = false
	model.meetingState.messages = []query.MessageSummary{
		{
			ID:          10,
			SourceID:    2,
			Subject:     "Product review",
			FromName:    "Test Organizer",
			SentAt:      time.Date(2026, 7, 13, 16, 0, 0, 0, time.UTC),
			MessageType: meetingMessageType,
		},
	}

	view := stripANSI(model.renderView())

	assert.Contains(t, view, "[Meetings]")
	assert.Contains(t, view, "Date")
	assert.Contains(t, view, "Title")
	assert.Contains(t, view, "Organizer")
	assert.Contains(t, view, "Source")
	assert.Contains(t, view, "Product review")
	assert.Contains(t, view, "Test Organizer")
	assert.Contains(t, view, "Granola")
	assert.NotContains(t, view, "del")
}

func TestMeetingListViewUsesPlaceholderForUnknownSource(t *testing.T) {
	model := NewBuilder().WithSize(100, 24).Build()
	model.mode = modeMeetings
	model.loading = false
	model.meetingState.messages = []query.MessageSummary{{
		ID: 10, Subject: "Old daemon result", SentAt: time.Now(),
	}}

	view := stripANSI(model.renderView())

	assert.Contains(t, view, "—")
}

func TestMeetingEmptyStateGuidesSourceSetup(t *testing.T) {
	model := NewBuilder().WithSize(100, 24).Build()
	model.mode = modeMeetings
	model.loading = false

	view := stripANSI(model.renderView())

	assert.Contains(t, view, "No meeting sources configured")
	assert.Contains(t, view, "Granola")
	assert.Contains(t, view, "Circleback")
}

func TestMeetingListViewShowsSearchInput(t *testing.T) {
	model := NewBuilder().WithSize(100, 24).Build()
	model.mode = modeMeetings
	model.meetingState.messages = []query.MessageSummary{{ID: 1, Subject: "Planning"}}
	model.meetingState.searchActive = true
	model.meetingState.searchInput.SetValue("roadmap")

	view := stripANSI(model.renderView())

	assert.Contains(t, view, "[Transcript]")
	assert.Contains(t, view, "roadmap")
}

func TestMeetingSearchInputRemainsVisibleWithNoResults(t *testing.T) {
	model := NewBuilder().WithSize(100, 24).Build()
	model.mode = modeMeetings
	model.loading = false
	model.meetingState.searchActive = true
	model.meetingState.searchInput.SetValue("missing phrase")

	view := stripANSI(model.renderView())

	assert.Contains(t, view, "[Transcript]")
	assert.Contains(t, view, "missing phrase")
}

func TestMeetingDetailViewShowsTranscript(t *testing.T) {
	model := NewBuilder().WithAccounts(
		query.AccountInfo{ID: 2, SourceType: "granola", Identifier: "work-notes"},
	).WithSize(100, 24).Build()
	model.mode = modeMeetings
	model.loading = false
	model.meetingState.level = meetingLevelDetail
	model.meetingState.detail = &query.MessageDetail{
		ID:       10,
		SourceID: 2,
		Subject:  "Product review",
		SentAt:   time.Date(2026, 7, 13, 16, 0, 0, 0, time.UTC),
		From:     []query.Address{{Name: "Test Organizer", Email: "organizer@example.com"}},
		To:       []query.Address{{Name: "Test Attendee", Email: "attendee@example.com"}},
		BodyText: "A searchable transcript sentence.",
	}

	view := stripANSI(model.renderView())

	assert.Contains(t, view, "Title: Product review")
	assert.Contains(t, view, "When:")
	assert.Contains(t, view, "Organizer: Test Organizer")
	assert.Contains(t, view, "Attendees: Test Attendee")
	assert.Contains(t, view, "Source: Granola")
	assert.Contains(t, view, "Transcript / Notes")
	assert.Contains(t, view, "A searchable transcript sentence.")
}

func TestMeetingHelpOnlyShowsReadOnlyActions(t *testing.T) {
	model := NewBuilder().WithSize(100, 60).Build()
	model.mode = modeMeetings

	help := stripANSI(model.renderHelpModal())

	assert.Contains(t, help, "Search titles, people, transcripts, and notes")
	assert.Contains(t, help, "Select meeting source")
	assert.Contains(t, help, "Cycle Email/Texts/Meetings")
	assert.NotContains(t, help, "Stage for deletion")
	assert.NotContains(t, help, "Toggle selection")
}

func TestMeetingListFitsNarrowTerminal(t *testing.T) {
	model := NewBuilder().WithSize(32, 16).Build()
	model.mode = modeMeetings
	model.loading = false
	model.meetingState.messages = []query.MessageSummary{{
		ID: 1, Subject: "A very long planning meeting title 日本語", FromName: "Long Organizer Name", SentAt: time.Now(),
	}}

	view := model.renderView()

	for line := range strings.SplitSeq(view, "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), 32, "line exceeds terminal width: %q", stripANSI(line))
	}
}
