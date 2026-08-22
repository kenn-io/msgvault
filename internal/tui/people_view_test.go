package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/query"
)

func TestPeopleDirectoryViewShowsIdentityAndActivityColumns(t *testing.T) {
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.width = 120
	model.peopleState.initialized = true
	model.peopleState.totalCount = 7
	model.peopleState.rows = []query.PersonSummary{testPerson(1, "Test Person")}

	view := stripANSI(model.renderView())

	assert.Contains(t, view, "[People]")
	assert.Contains(t, view, "People Directory")
	assert.Contains(t, view, "Label")
	assert.Contains(t, view, "Identifiers")
	assert.Contains(t, view, "Last interaction")
	assert.Contains(t, view, "Activity")
	assert.Contains(t, view, "Files")
	assert.Contains(t, view, "▶  Test Person")
	assert.Contains(t, view, "person-1@example.test")
	assert.Contains(t, view, "2026-08-01 12:30")
	assert.Contains(t, view, "10")
	assert.Contains(t, view, "1")
	assert.Contains(t, view, "1 of 7")
}

func TestPeopleDirectoryViewKeepsIdentifiersBeforeCountsBelowEightyColumns(t *testing.T) {
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.width = 72
	model.peopleState.initialized = true
	model.peopleState.rows = []query.PersonSummary{testPerson(1, "Test Person")}

	view := stripANSI(model.renderView())

	assert.Contains(t, view, "Test Person")
	assert.Contains(t, view, "person-1@example.test")
	assert.NotContains(t, view, "Activity")
	assert.NotContains(t, view, "Files")
	for _, line := range strings.Split(view, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 72, "line must fit narrow terminal: %q", line)
	}
}

func TestPeopleDirectoryViewKeepsMarkerAndLabelAtVeryNarrowWidth(t *testing.T) {
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.width = 36
	model.peopleState.initialized = true
	model.peopleState.rows = []query.PersonSummary{testPerson(1, "Test Person")}

	view := stripANSI(model.renderView())

	assert.Contains(t, view, "▶  Test Person")
	assert.NotContains(t, view, "person-1@example.test")
	for _, line := range strings.Split(view, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 36, "line must fit very narrow terminal: %q", line)
	}
}

func TestPeopleContactViewShowsHeaderAndSixTabShell(t *testing.T) {
	contact := testPerson(2, "Workspace Person")
	contact.MeetingCount = 4
	contact.DisplayName = "Observed Name"
	contact.SourceCounts = []query.SourceCount{{SourceType: "gmail", Count: 3}, {SourceType: "beeper", Count: 7}}
	contact.Profile = &query.PersonProfile{ID: 42, Revision: 1}
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.width = 120
	model.peopleState.level = peopleLevelContact
	model.peopleState.contact = &contact
	model.peopleState.participantID = contact.ID

	view := stripANSI(model.renderView())

	assert.Contains(t, view, "Workspace Person")
	assert.Contains(t, view, "curated profile")
	for _, tab := range []string{"Overview", "Attributes", "Inboxes", "Meetings", "Files", "Activity"} {
		assert.Contains(t, view, tab)
	}
	assert.Contains(t, view, "[Overview]")
	assert.Contains(t, view, "Observed Name")
	assert.Contains(t, view, "person-2@example.test")
	assert.Contains(t, view, "gmail, beeper")
	assert.Contains(t, view, "First interaction")
	assert.Contains(t, view, "Latest interaction")
	assert.Contains(t, view, "Activity: 20")
	assert.Contains(t, view, "Meetings: 4")
	assert.Contains(t, view, "Files: 2")
}

func TestPeopleContactNarrowHeaderRetainsIdentifierAndAllCounts(t *testing.T) {
	contact := testPerson(2, "Workspace Person")
	contact.MeetingCount = 4
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.width = 60
	model.peopleState.level = peopleLevelContact
	model.peopleState.contact = &contact

	view := stripANSI(model.peopleHeaderView())
	assert.Contains(t, view, "person-2@example.test")
	assert.Contains(t, view, "A:20")
	assert.Contains(t, view, "M:4")
	assert.Contains(t, view, "F:2")
	for _, line := range strings.Split(view, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 60)
	}
}

func TestDisplayPeopleInboxIdentifierIsRuneSafe(t *testing.T) {
	label := displayPeopleInboxIdentifier("東京_chat")
	assert.True(t, utf8.ValidString(label))
	assert.Equal(t, "東京 Chat", label)
}

func TestPeopleContactTabShellNamesActiveTab(t *testing.T) {
	contact := testPerson(3, "Tabbed Person")
	for _, tc := range []struct {
		tab  peopleTab
		name string
	}{
		{peopleTabOverview, "Overview"},
		{peopleTabAttributes, "Attributes"},
		{peopleTabInboxes, "Inboxes"},
		{peopleTabMeetings, "Meetings"},
		{peopleTabFiles, "Files"},
		{peopleTabActivity, "Activity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := peopleModel(&fakePeopleBackend{})
			model.mode = modePeople
			model.width = 120
			model.peopleState.level = peopleLevelContact
			model.peopleState.contact = &contact
			model.peopleState.tab = tc.tab

			view := stripANSI(model.renderView())

			assert.Contains(t, view, "["+tc.name+"]")
		})
	}
}

func TestPeopleDirectoryViewShowsSearchAndExplicitStates(t *testing.T) {
	t.Run("search", func(t *testing.T) {
		model := peopleModel(&fakePeopleBackend{})
		model.mode = modePeople
		model.peopleState.searchActive = true
		model.peopleState.searchInput.SetValue("person@example.test")
		view := stripANSI(model.renderView())
		assert.Contains(t, view, "Search people:")
		assert.Contains(t, view, "person@example.test")
	})

	t.Run("loading", func(t *testing.T) {
		model := peopleModel(&fakePeopleBackend{})
		model.mode = modePeople
		model.loading = true
		model.peopleState.directoryLoading = true
		assert.Contains(t, stripANSI(model.renderView()), "Loading people")
	})

	t.Run("empty", func(t *testing.T) {
		model := peopleModel(&fakePeopleBackend{})
		model.mode = modePeople
		model.peopleState.initialized = true
		assert.Contains(t, stripANSI(model.renderView()), "No people found")
	})

	t.Run("retryable error", func(t *testing.T) {
		model := peopleModel(&fakePeopleBackend{})
		model.mode = modePeople
		model.peopleState.err = errPeopleDirectoryChanged
		view := stripANSI(model.renderView())
		assert.Contains(t, view, "people directory changed")
		assert.Contains(t, view, "r retry")
	})

	t.Run("focused search with partial-row error", func(t *testing.T) {
		model := peopleModel(&fakePeopleBackend{})
		model.mode = modePeople
		model.peopleState.initialized = true
		model.peopleState.searchQuery = "committed"
		model.peopleState.rows = []query.PersonSummary{testPerson(1, "Partial Person")}
		model.peopleState.err = errPeopleDirectoryChanged

		model, _ = sendKey(t, model, key('/'))
		view := stripANSI(model.renderView())
		assert.Contains(t, view, "Search people:")
		assert.Contains(t, view, "committed")
	})
}

func TestPeopleHelpDescribesPeopleNavigationWithoutEmailActions(t *testing.T) {
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.height = 80

	help := stripANSI(model.renderHelpModal())

	assert.Contains(t, help, "Search names and identifiers")
	assert.Contains(t, help, "Cycle Email/Texts/Meetings/People")
	assert.Contains(t, help, "Cycle contact tabs")
	assert.NotContains(t, help, "Stage for deletion")
}
