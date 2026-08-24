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
	assert := assert.New(t)
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.width = 120
	model.peopleState.initialized = true
	model.peopleState.totalCount = 7
	model.peopleState.rows = []query.PersonSummary{testPerson(1, "Test Person")}

	view := stripANSI(model.renderView())

	assert.Contains(view, "[People]")
	assert.Contains(view, "People Directory")
	assert.Contains(view, "Label")
	assert.Contains(view, "Identifiers")
	assert.Contains(view, "Last interaction")
	assert.Contains(view, "Activity")
	assert.Contains(view, "Files")
	assert.Contains(view, "▶  Test Person")
	assert.Contains(view, "person-1@example.test")
	assert.Contains(view, "2026-08-01 12:30")
	assert.Contains(view, "10")
	assert.Contains(view, "1")
	assert.Contains(view, "1 of 7")
}

func TestPeopleDirectoryViewKeepsIdentifiersBeforeCountsBelowEightyColumns(t *testing.T) {
	assert := assert.New(t)
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.width = 72
	model.peopleState.initialized = true
	model.peopleState.rows = []query.PersonSummary{testPerson(1, "Test Person")}

	view := stripANSI(model.renderView())

	assert.Contains(view, "Test Person")
	assert.Contains(view, "person-1@example.test")
	assert.NotContains(view, "Activity")
	assert.NotContains(view, "Files")
	for line := range strings.SplitSeq(view, "\n") {
		assert.LessOrEqual(ansi.StringWidth(line), 72, "line must fit narrow terminal: %q", line)
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
	for line := range strings.SplitSeq(view, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 36, "line must fit very narrow terminal: %q", line)
	}
}

func TestPeopleContactViewShowsHeaderAndSixTabShell(t *testing.T) {
	assert := assert.New(t)
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

	assert.Contains(view, "Workspace Person")
	assert.Contains(view, "curated profile")
	for _, tab := range []string{"Overview", "Attributes", "Inboxes", "Meetings", "Files", "Activity"} {
		assert.Contains(view, tab)
	}
	assert.Contains(view, "[Overview]")
	assert.Contains(view, "Observed Name")
	assert.Contains(view, "person-2@example.test")
	assert.Contains(view, "gmail, beeper")
	assert.Contains(view, "First interaction")
	assert.Contains(view, "Latest interaction")
	assert.Contains(view, "Activity: 20")
	assert.Contains(view, "Meetings: 4")
	assert.Contains(view, "Files: 2")
}

func TestPeopleContactNarrowHeaderRetainsIdentifierAndAllCounts(t *testing.T) {
	assert := assert.New(t)
	contact := testPerson(2, "Workspace Person")
	contact.MeetingCount = 4
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.width = 60
	model.peopleState.level = peopleLevelContact
	model.peopleState.contact = &contact

	view := stripANSI(model.peopleHeaderView())
	assert.Contains(view, "person-2@example.test")
	assert.Contains(view, "A:20")
	assert.Contains(view, "M:4")
	assert.Contains(view, "F:2")
	for line := range strings.SplitSeq(view, "\n") {
		assert.LessOrEqual(ansi.StringWidth(line), 60)
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
	assert := assert.New(t)
	model := peopleModel(&fakePeopleBackend{})
	model.mode = modePeople
	model.height = 80

	help := stripANSI(model.renderHelpModal())

	assert.Contains(help, "Search names and identifiers")
	assert.Contains(help, "Cycle Email/Texts/Meetings/People")
	assert.Contains(help, "Cycle contact tabs")
	assert.NotContains(help, "Stage for deletion")
}
