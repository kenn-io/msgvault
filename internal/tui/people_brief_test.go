package tui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

type fakePeopleBriefBackend struct {
	*fakePeopleBackend

	briefRequests []int64
	brief         *peoplebrowser.PersonBrief
	briefErr      error

	enrollRequests   []briefEnrollmentCall
	enrollment       *peoplebrowser.PersonBriefEnrollment
	enrollErr        error
	generateRequests []int64
	run              *peoplebrowser.PersonBriefRun
	generateErr      error
	rejectRequests   []string
	rejected         *peoplebrowser.PersonBrief
	rejectErr        error
}

func (b *fakePeopleBriefBackend) GetPersonBrief(
	_ context.Context, personID int64,
) (*peoplebrowser.PersonBrief, error) {
	b.briefRequests = append(b.briefRequests, personID)
	if b.briefErr != nil {
		return nil, b.briefErr
	}
	return b.brief, nil
}

// briefEnrollmentCall is one recorded enrollment request, so a test asserts
// the person, the opt-in, and the tracking flag as one comparable value.
type briefEnrollmentCall struct {
	personID int64
	enrolled bool
	track    bool
}

func (b *fakePeopleBriefBackend) SetPersonBriefEnrollment(
	_ context.Context, personID int64, enrolled, track bool,
) (*peoplebrowser.PersonBriefEnrollment, error) {
	b.enrollRequests = append(b.enrollRequests, briefEnrollmentCall{
		personID: personID, enrolled: enrolled, track: track,
	})
	if b.enrollErr != nil {
		return nil, b.enrollErr
	}
	return b.enrollment, nil
}

func (b *fakePeopleBriefBackend) GeneratePersonBrief(
	_ context.Context, personID int64,
) (*peoplebrowser.PersonBriefRun, error) {
	b.generateRequests = append(b.generateRequests, personID)
	if b.generateErr != nil {
		return nil, b.generateErr
	}
	return b.run, nil
}

func (b *fakePeopleBriefBackend) RejectPersonBrief(
	_ context.Context, _ int64, reason string,
) (*peoplebrowser.PersonBrief, error) {
	b.rejectRequests = append(b.rejectRequests, reason)
	if b.rejectErr != nil {
		return nil, b.rejectErr
	}
	return b.rejected, nil
}

func testPersonBrief() *peoplebrowser.PersonBrief {
	observed := time.Date(2026, 8, 29, 17, 0, 0, 0, time.UTC)
	stale := time.Date(2026, 6, 4, 11, 0, 0, 0, time.UTC)
	return &peoplebrowser.PersonBrief{
		Version: 2, Status: "current",
		GeneratedAt:  time.Date(2026, 8, 29, 18, 42, 10, 0, time.UTC),
		RenderedText: "Last time you talked (Aug 29, chat): they were preparing for a role change. They said they spent the weekend learning to cook. Check before assuming: the move may have changed.",
		Sentences: []peoplebrowser.PersonBriefSentence{
			{Kind: peoplebrowser.PersonBriefLastInteraction, Index: 0, Text: "Last time you talked (Aug 29, chat): they were preparing for a role change."},
		},
		Items: []peoplebrowser.PersonBriefItem{
			{
				Kind: peoplebrowser.PersonBriefLastInteraction, Index: 0,
				Text: "they were preparing for a role change",
				Evidence: []peoplebrowser.PersonBriefEvidence{{
					Ordinal: 0, EvidenceID: 11, SourceRef: "message:1",
					Directness: "direct-self", EventTime: observed, Supported: true,
				}},
			},
			{
				Kind: peoplebrowser.PersonBriefHighlight, Index: 0,
				Text: "they spent the weekend learning to cook", Speaker: "person",
				Evidence: []peoplebrowser.PersonBriefEvidence{{
					Ordinal: 0, EvidenceID: 11, SourceRef: "message:1",
					Directness: "direct-self", EventTime: observed, Supported: true,
				}},
			},
			{
				Kind: peoplebrowser.PersonBriefFollowUp, Index: 0,
				Text: "how the transition went", Why: "they were mid-change",
				Evidence: []peoplebrowser.PersonBriefEvidence{{
					Ordinal: 0, EvidenceID: 11, SourceRef: "message:1",
					Directness: "direct-self", EventTime: observed, Supported: true,
				}},
			},
			{
				Kind: peoplebrowser.PersonBriefUncertainty, Index: 0,
				Text: "the move may have changed", Reason: "stale",
				Evidence: []peoplebrowser.PersonBriefEvidence{{
					Ordinal: 2, EvidenceID: 13, SourceRef: "message:3",
					Directness: "direct-self", EventTime: stale, Supported: false,
				}},
			},
		},
		DroppedItemCount: 1,
	}
}

// peopleBriefModel opens a promoted contact on the Overview tab with the brief
// already loaded, which is the state every reading test starts from.
func peopleBriefModel(backend peoplebrowser.Backend, brief *peoplebrowser.PersonBrief) Model {
	contact := testPerson(7, "Brief Person")
	contact.Profile = &query.PersonProfile{ID: 51, Revision: 2}
	model := peopleModel(backend)
	model.mode = modePeople
	model.presentationGeneration = 8
	model.peopleState.level = peopleLevelContact
	model.peopleState.tab = peopleTabOverview
	model.peopleState.participantID = contact.ID
	model.peopleState.contact = &contact
	model.peopleState.brief = brief
	model.peopleState.briefLoaded = true
	return model
}

func TestPeopleOverviewLoadsAndRendersTheBriefUnderTheContactState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	brief := testPersonBrief()
	backend := &fakePeopleBriefBackend{fakePeopleBackend: &fakePeopleBackend{}, brief: brief}
	model := peopleBriefModel(backend, nil)
	model.peopleState.briefLoaded = false

	contact := *model.peopleState.contact
	model.peopleState.contact = nil
	model.peopleState.contactLoading = true
	updated, load := model.Update(peopleContactLoadedMsg{
		contact: &contact, requestID: model.peopleState.requestID,
		participantID: contact.ID, presentationGeneration: model.presentationGeneration,
	})
	model = asModel(t, updated)
	require.NotNil(load)
	loaded := runPeopleCommandMessage[peopleBriefLoadedMsg](t, load)
	assert.Equal([]int64{51}, backend.briefRequests)
	model = sendMsg(t, model, loaded)
	require.NotNil(model.peopleState.brief)

	view := stripANSI(model.renderView())
	assert.Contains(view, "Last time we talked")
	assert.Contains(view, "Brief v2, 2026-08-29")
	assert.Contains(view, "they were preparing for a role change")

	lines := strings.Split(view, "\n")
	latest := lineIndexContaining(t, lines, "Latest interaction")
	version := lineIndexContaining(t, lines, "Brief v2, 2026-08-29")
	assert.Greater(version, latest, "the brief sits under the contact-state lines")
}

// lineIndexContaining returns the index of the first rendered line holding the
// substring, failing the test when no line does.
func lineIndexContaining(t *testing.T, lines []string, substring string) int {
	t.Helper()
	for index, line := range lines {
		if strings.Contains(line, substring) {
			return index
		}
	}
	require.FailNow(t, "no rendered line contains "+substring)
	return -1
}

func TestPeopleOverviewBriefReportsAbsentFailedAndUnpromotedStates(t *testing.T) {
	t.Run("no version yet", func(t *testing.T) {
		backend := &fakePeopleBriefBackend{fakePeopleBackend: &fakePeopleBackend{}}
		model := peopleBriefModel(backend, nil)
		view := stripANSI(model.renderView())
		assert.Contains(t, view, "Last time we talked")
		assert.Contains(t, view, "No brief yet")
	})

	t.Run("load failure offers retry", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		backend := &fakePeopleBriefBackend{
			fakePeopleBackend: &fakePeopleBackend{},
			briefErr:          errors.New("brief unavailable"),
		}
		model := peopleBriefModel(backend, nil)
		model.peopleState.briefLoaded = false
		model.peopleState.briefLoading = true
		load := model.loadPeopleBrief(51)
		model = sendMsg(t, model, runPeopleCommandMessage[peopleBriefLoadedMsg](t, load))
		require.Error(model.peopleState.briefErr)
		view := stripANSI(model.renderView())
		assert.Contains(view, "Brief is unavailable")

		model, _ = sendKey(t, model, key('r'))
		require.NoError(model.peopleState.briefErr, "r clears the failure and reloads")
		assert.True(model.peopleState.briefLoading)
	})

	t.Run("observed contact", func(t *testing.T) {
		backend := &fakePeopleBriefBackend{fakePeopleBackend: &fakePeopleBackend{}}
		model := peopleBriefModel(backend, testPersonBrief())
		contact := *model.peopleState.contact
		contact.Profile = nil
		model.peopleState.contact = &contact
		view := stripANSI(model.renderView())
		assert.Contains(t, view, "Promote with p to see the brief")
		assert.NotContains(t, view, "Brief v2")
	})
}

func TestPeopleBriefStructuredViewFallsBackToWholeBriefEvidence(t *testing.T) {
	brief := testPersonBrief()
	brief.Sentences = nil
	brief.Evidence = brief.Items[3].Evidence
	for index := range brief.Items {
		brief.Items[index].Evidence = nil
	}
	backend := &fakePeopleBriefBackend{fakePeopleBackend: &fakePeopleBackend{}}
	model := peopleBriefModel(backend, brief)
	model.height = 50
	model.pageSize = 45
	model, _ = sendKey(t, model, key('b'))
	view := stripANSI(model.renderView())
	assert.Contains(t, view, "Whole brief evidence")
	assert.Contains(t, view, "2026-06-04 (unsupported)")
	assert.Equal(t, 1, strings.Count(view, "2026-06-04"), "whole-brief evidence is not attributed to individual items")
}

func TestPeopleBriefStructuredViewListsItemsWithEvidenceDates(t *testing.T) {
	assert := assert.New(t)
	backend := &fakePeopleBriefBackend{fakePeopleBackend: &fakePeopleBackend{}}
	model := peopleBriefModel(backend, testPersonBrief())
	// Five sections with their citations outgrow the fixture's page; the
	// pane scrolls, but this test reads the whole view at once.
	model.height = 40
	model.pageSize = 35

	model, _ = sendKey(t, model, key('b'))
	assert.True(model.peopleState.briefStructured)
	view := stripANSI(model.renderView())
	for _, heading := range []string{
		"Last interaction", "Highlights", "Follow-ups", "Appreciations", "Uncertainties",
	} {
		assert.Contains(view, heading)
	}
	assert.Less(strings.Index(view, "Last interaction"), strings.Index(view, "Highlights"),
		"the last interaction leads the structured view")
	assert.Contains(view, "they were preparing for a role change",
		"the last interaction item is shown, not only its sentence")
	assert.Contains(view, "they spent the weekend learning to cook")
	assert.Contains(view, "2026-08-29", "each item shows its evidence date")
	assert.Contains(view, "how the transition went")
	assert.Contains(view, "they were mid-change")
	assert.Contains(view, "2026-06-04")
	assert.Contains(view, "unsupported", "an invalidated citation stays visible and marked")
	assert.Contains(view, "Dropped items: 1")
	assert.Contains(view, "b/Esc overview", "the footer names the keys this view answers to")

	model, cmd := sendKey(t, model, key('n'))
	assert.Nil(cmd)
	assert.Equal(peopleOverlayNone, model.peopleState.form.overlay,
		"overview keys do not act on a surface the structured view has replaced")

	model, _ = sendKey(t, model, keyEsc())
	assert.False(model.peopleState.briefStructured)
	assert.Contains(stripANSI(model.renderView()), "Contact overview")
	assert.Equal(peopleLevelContact, model.peopleState.level, "Esc leaves the contact open")
}

// TestPeopleBriefStructuredViewShowsABriefMadeOfTheLastInteractionAlone pins
// the regression: the structured view listed only the four optional kinds, so
// a brief that carried nothing but its mandatory last interaction rendered as
// four "None" sections and looked empty.
func TestPeopleBriefStructuredViewShowsABriefMadeOfTheLastInteractionAlone(t *testing.T) {
	assert := assert.New(t)
	brief := testPersonBrief()
	brief.Items = brief.Items[:1]
	brief.DroppedItemCount = 0
	backend := &fakePeopleBriefBackend{fakePeopleBackend: &fakePeopleBackend{}}
	model := peopleBriefModel(backend, brief)

	model, _ = sendKey(t, model, key('b'))
	view := stripANSI(model.renderView())
	assert.Contains(view, "Last interaction")
	assert.Contains(view, "they were preparing for a role change")
	assert.Contains(view, "2026-08-29", "the interaction cites its evidence date")
	assert.Equal(4, strings.Count(view, "- None"),
		"only the four optional kinds are empty")
}

func TestPeopleBriefCommandsRunAgainstTheDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	enabled := time.Date(2026, 8, 29, 18, 0, 0, 0, time.UTC)
	backend := &fakePeopleBriefBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		enrollment: &peoplebrowser.PersonBriefEnrollment{
			PersonID: 51, Enrolled: true, EnabledAt: &enabled, Actor: "api",
		},
		run:      &peoplebrowser.PersonBriefRun{RunID: "run-1", AttemptID: "attempt-1", BriefVersion: 3},
		rejected: testPersonBrief(),
	}
	model := peopleBriefModel(backend, testPersonBrief())

	model = runPeopleBriefCommandForTest(t, model, "brief enroll")
	require.Len(backend.enrollRequests, 1)
	assert.Equal(briefEnrollmentCall{personID: 51, enrolled: true, track: true},
		backend.enrollRequests[0], "the palette enrolls and adds the tracking row it requires")
	assert.Contains(stripANSI(model.renderView()), "enrolled")

	model = runPeopleBriefCommandForTest(t, model, "brief generate")
	assert.Equal([]int64{51}, backend.generateRequests)
	view := stripANSI(model.renderView())
	assert.Contains(view, "attempt-1")
	assert.Contains(view, "version 3")

	backend.run = &peoplebrowser.PersonBriefRun{RunID: "run-2", AttemptID: "attempt-2", BriefFailureClass: "budget"}
	model = runPeopleBriefCommandForTest(t, model, "brief generate")
	assert.Contains(stripANSI(model.renderView()), "budget")

	// After the rejection the daemon has no current version, so the re-read
	// answers nil. The overview must fall back to the no-brief state rather
	// than keep showing the rejected paragraph.
	backend.brief = nil
	model, _ = sendKey(t, model, key(':'))
	model = typePeopleBriefCommand(t, model, "brief reject wrong thread")
	model, command := sendKey(t, model, keyEnter())
	require.NotNil(command)
	updated, reload := model.Update(runPeopleCommandMessage[peopleBriefCommandMsg](t, command))
	model = asModel(t, updated)
	assert.Equal([]string{"wrong thread"}, backend.rejectRequests)
	assert.Contains(stripANSI(model.renderView()), "rejected")
	require.NotNil(reload, "a rejection re-reads the brief from the daemon")
	model = sendMsg(t, model, runPeopleCommandMessage[peopleBriefLoadedMsg](t, reload))
	view = stripANSI(model.renderView())
	assert.Contains(view, "No brief yet")
	assert.Contains(view, "Brief v2 rejected.", "the notice still names the rejected version")
	assert.NotContains(view, "Brief v2, 2026-08-29", "but the version line is gone")
	assert.NotContains(view, "they were preparing for a role change")
}

func TestPeopleBriefCommandOverlayRefusesUnknownAndFailedCommands(t *testing.T) {
	assert := assert.New(t)
	backend := &fakePeopleBriefBackend{
		fakePeopleBackend: &fakePeopleBackend{},
		generateErr:       errors.New("provider budget exhausted"),
	}
	model := peopleBriefModel(backend, testPersonBrief())

	model, _ = sendKey(t, model, key(':'))
	require.Equal(t, peopleOverlayBriefCommand, model.peopleState.form.overlay)
	model = typePeopleBriefCommand(t, model, "brief summarise")
	model, cmd := sendKey(t, model, keyEnter())
	assert.Nil(cmd, "an unknown command never reaches the daemon")
	assert.Equal(peopleOverlayBriefCommand, model.peopleState.form.overlay)
	assert.Contains(stripANSI(model.renderView()), "brief enroll")
	assert.Empty(backend.generateRequests)

	model, _ = sendKey(t, model, keyEsc())
	assert.Equal(peopleOverlayNone, model.peopleState.form.overlay)

	model = runPeopleBriefCommandForTest(t, model, "brief generate")
	assert.Contains(stripANSI(model.renderView()), "provider budget exhausted")
}

// TestPeopleBriefCommandReportsARefusedGenerationInPlainWords covers the four
// gates a manual generation can hit. Each one used to reach the owner as an
// HTTP status line, or, before the daemon refused them at all, as a run that
// silently stored nothing. The failures are real daemonclient.APIError values,
// which is what the daemon client hands the palette for a 409.
func TestPeopleBriefCommandReportsARefusedGenerationInPlainWords(t *testing.T) {
	for _, test := range []struct {
		code string
		want string
	}{
		{code: "person_brief_not_enrolled",
			want: "No brief: this person is not enrolled. Run brief enroll first."},
		{code: "person_brief_lane_disabled",
			want: "No brief: brief generation is turned off in the daemon configuration."},
		{code: "person_brief_policy_refused",
			want: "No brief: the people inference profile does not allow sensitive " +
				"content, which every brief carries."},
		{code: "person_brief_no_supported_lane",
			want: "No brief: the people inference profile does not allow conversation " +
				"text, the only source a brief reads in this version."},
		{code: "person_brief_busy",
			want: "No brief: another worker is sweeping this person right now. " +
				"Retry once it finishes."},
	} {
		t.Run(test.code, func(t *testing.T) {
			backend := &fakePeopleBriefBackend{
				fakePeopleBackend: &fakePeopleBackend{},
				generateErr: &daemonclient.APIError{
					Status: http.StatusConflict, Code: test.code, Message: "refused",
				},
			}
			model := peopleBriefModel(backend, testPersonBrief())
			model = runPeopleBriefCommandForTest(t, model, "brief generate")
			assert.Equal(t, test.want, model.peopleState.briefNotice)
			assert.NotContains(t, model.peopleState.briefNotice, "API error")
		})
	}
}

// briefAndAttributesBackend serves both Overview lanes so one lane's failure
// can be observed not to hold the other hostage.
type briefAndAttributesBackend struct {
	*fakePeopleAttributesBackend

	briefRequests []int64
	brief         *peoplebrowser.PersonBrief
	briefErr      error
}

func (b *briefAndAttributesBackend) GetPersonBrief(
	_ context.Context, personID int64,
) (*peoplebrowser.PersonBrief, error) {
	b.briefRequests = append(b.briefRequests, personID)
	if b.briefErr != nil {
		return nil, b.briefErr
	}
	return b.brief, nil
}

func TestPeopleOverviewRetryRestartsTheBriefAndTheNotesTogether(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &briefAndAttributesBackend{
		fakePeopleAttributesBackend: &fakePeopleAttributesBackend{},
		briefErr:                    errors.New("brief unavailable"),
	}
	model := peopleBriefModel(backend, nil)
	model.peopleState.briefLoaded = false
	model.peopleState.briefErr = errors.New("load brief failed: brief unavailable")

	for range 2 {
		var retry tea.Cmd
		model, retry = sendKey(t, model, key('r'))
		require.NotNil(retry, "r restarts the Overview loads")
		for _, msg := range runBatchCommand(t, retry) {
			model = sendMsg(t, model, msg)
		}
	}

	assert.Equal([]int64{51, 51}, backend.briefRequests)
	assert.Equal([]int64{51, 51}, backend.attributeRequests,
		"a brief that keeps failing does not block the notes reload")
	require.Error(model.peopleState.briefErr)
	assert.Contains(stripANSI(model.renderView()), "Brief is unavailable")
}

func TestPeopleTabSwitchSettlesAnInFlightBriefRequest(t *testing.T) {
	// Switching tabs takes a fresh request ID, which drops the answer still in
	// flight before its handler can clear the loading flag. The settle block
	// has to clear it, or updatePeopleLoading keeps the spinner armed forever.
	settledModel := func(t *testing.T, inFlight func(*Model) tea.Cmd) (Model, tea.Cmd) {
		t.Helper()
		backend := &fakePeopleBriefBackend{
			fakePeopleBackend: &fakePeopleBackend{}, brief: testPersonBrief(),
			run: &peoplebrowser.PersonBriefRun{RunID: "run-1", AttemptID: "attempt-1", BriefVersion: 3},
		}
		model := peopleBriefModel(backend, testPersonBrief())
		model.peopleState.attributesLoaded = true
		pending := inFlight(&model)
		require.NotNil(t, pending, "the request must be in flight before the tab switch")
		require.True(t, model.loading, "an in-flight brief request holds the spinner")
		updated, _ := sendKey(t, model, keyTab())
		return updated, pending
	}

	t.Run("brief load", func(t *testing.T) {
		assert := assert.New(t)
		model, pending := settledModel(t, func(m *Model) tea.Cmd {
			m.peopleState.brief = nil
			m.peopleState.briefLoaded = false
			cmd := m.beginPeopleBriefLoad()
			m.loading = true
			return cmd
		})
		assert.Equal(peopleTabAttributes, model.peopleState.tab)
		assert.False(model.peopleState.briefLoading)
		assert.False(model.loading, "the spinner stops when the tab settles the brief read")
		model = sendMsg(t, model, runPeopleCommandMessage[peopleBriefLoadedMsg](t, pending))
		assert.False(model.loading, "the dropped answer does not re-arm the spinner")
	})

	t.Run("brief command", func(t *testing.T) {
		assert := assert.New(t)
		model, pending := settledModel(t, func(m *Model) tea.Cmd {
			m.peopleState.requestID++
			cmd := m.runPeopleBriefCommand(peopleBriefCommandGenerate, 51, "")
			m.peopleState.briefCommandRunning = true
			m.loading = true
			return cmd
		})
		assert.Equal(peopleTabAttributes, model.peopleState.tab)
		assert.False(model.peopleState.briefCommandRunning)
		assert.False(model.loading, "a stranded command flag would never heal on its own")
		model = sendMsg(t, model, runPeopleCommandMessage[peopleBriefCommandMsg](t, pending))
		assert.False(model.loading)
	})
}

func TestPeoplePromotionStartsTheBriefLoad(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &briefAndAttributesBackend{
		fakePeopleAttributesBackend: &fakePeopleAttributesBackend{
			promoted: &store.Person{ID: 51, VCardUID: "person-51", Revision: 1,
				ParticipantIDs: []int64{7}},
		},
		brief: testPersonBrief(),
	}
	model := peopleBriefModel(backend, nil)
	model.peopleState.briefLoaded = false
	contact := *model.peopleState.contact
	contact.Profile = nil
	model.peopleState.contact = &contact

	model, promote := sendKey(t, model, key('p'))
	require.NotNil(promote)
	updated, loads := model.Update(runPeopleCommandMessage[peoplePromotedMsg](t, promote))
	model = asModel(t, updated)
	require.NotNil(model.peopleState.contact.Profile)
	require.NotNil(loads, "promotion starts the loads the new person ID unlocked")
	for _, msg := range runBatchCommand(t, loads) {
		model = sendMsg(t, model, msg)
	}

	assert.Equal([]int64{51}, backend.briefRequests,
		"promotion is the first moment the contact has a person ID to read a brief for")
	assert.NotContains(stripANSI(model.renderView()), "The brief has not loaded")
}

func TestPeopleBriefSurfacesAreAbsentWithoutABriefBackend(t *testing.T) {
	assert := assert.New(t)
	model := peopleBriefModel(&fakePeopleBackend{}, nil)

	view := stripANSI(model.renderView())
	assert.NotContains(view, "Last time we talked")

	model, cmd := sendKey(t, model, key('b'))
	assert.Nil(cmd)
	assert.False(model.peopleState.briefStructured)

	model, cmd = sendKey(t, model, key(':'))
	assert.Nil(cmd)
	assert.Equal(peopleOverlayNone, model.peopleState.form.overlay)
}

func TestPeopleHelpListsTheBriefKeys(t *testing.T) {
	model := peopleBriefModel(&fakePeopleBriefBackend{fakePeopleBackend: &fakePeopleBackend{}}, testPersonBrief())
	model, _ = sendKey(t, model, key('?'))
	help := stripANSI(model.renderView())
	assert.Contains(t, help, "Brief structured view")
	assert.Contains(t, help, "Brief commands")
}

// typePeopleBriefCommand types a command into the open brief command overlay.
func typePeopleBriefCommand(t *testing.T, model Model, command string) Model {
	t.Helper()
	for _, r := range command {
		model, _ = sendKey(t, model, key(r))
	}
	return model
}

// runPeopleBriefCommandForTest opens the overlay, types one command, submits
// it, and delivers the daemon's answer back to the model.
func runPeopleBriefCommandForTest(t *testing.T, model Model, command string) Model {
	t.Helper()
	model, _ = sendKey(t, model, key(':'))
	require.Equal(t, peopleOverlayBriefCommand, model.peopleState.form.overlay)
	model = typePeopleBriefCommand(t, model, command)
	var cmd tea.Cmd
	model, cmd = sendKey(t, model, keyEnter())
	require.NotNil(t, cmd, "a valid brief command runs against the daemon")
	return sendMsg(t, model, runPeopleCommandMessage[peopleBriefCommandMsg](t, cmd))
}

// briefOverviewBackend serves every Overview lane at once — brief reads, brief
// commands, notes, and the relationship calendar — so a request-ID bump on one
// lane can be observed against the loads the other lanes still have running.
type briefOverviewBackend struct {
	*fakePeopleBriefBackend

	attributeRequests []int64
	calendarRequests  []peoplebrowser.CalendarRequest
}

func (b *briefOverviewBackend) ListAttributes(
	_ context.Context, personID int64,
) (*peoplebrowser.Attributes, error) {
	b.attributeRequests = append(b.attributeRequests, personID)
	return &peoplebrowser.Attributes{PersonID: personID}, nil
}

func (b *briefOverviewBackend) RelationshipCalendar(
	_ context.Context, request peoplebrowser.CalendarRequest,
) (*query.RelationshipCalendarResponse, error) {
	b.calendarRequests = append(b.calendarRequests, request)
	return &query.RelationshipCalendarResponse{
		CanonicalID: request.ParticipantID, Year: request.Year, Timezone: request.Timezone,
		// testPerson carries this revision, and a calendar that disagrees with
		// the contact restarts the whole contact load instead of settling.
		CacheRevision: "cache-1",
	}, nil
}

func briefOverviewModel(backend *briefOverviewBackend, brief *peoplebrowser.PersonBrief) Model {
	model := peopleBriefModel(backend, brief)
	model.peopleState.location = time.UTC
	return model
}

// TestPeopleOverviewRetrySettlesTheLoadsItsRequestIDSupersedes covers both ways
// r used to strand a lane: only the lanes it decided were due are restarted,
// but the fresh request ID drops every answer still in flight, and the flags
// those answers would have cleared are matched on that same ID.
func TestPeopleOverviewRetrySettlesTheLoadsItsRequestIDSupersedes(t *testing.T) {
	t.Run("relationship retry restarts the notes load", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		backend := &briefOverviewBackend{fakePeopleBriefBackend: &fakePeopleBriefBackend{
			fakePeopleBackend: &fakePeopleBackend{}, brief: testPersonBrief(),
		}}
		model := briefOverviewModel(backend, testPersonBrief())
		model.peopleState.attributesLoading = true
		superseded := model.loadPeopleAttributes(51, peopleTabOverview)
		model.peopleState.relationshipErr = errors.New("load relationship: calendar unavailable")
		model.loading = true

		model, retry := sendKey(t, model, key('r'))
		require.NotNil(retry, "a failed calendar still retries")

		model = sendMsg(t, model, runPeopleCommandMessage[peopleAttributesLoadedMsg](t, superseded))
		for _, msg := range runBatchCommand(t, retry) {
			model = sendMsg(t, model, msg)
		}
		assert.False(model.peopleState.attributesLoading)
		assert.False(model.loading, "a superseded notes load cannot hold the spinner")
		assert.Equal([]int64{51, 51}, backend.attributeRequests,
			"the retry restarts the notes load its own bump superseded")
		assert.True(model.peopleState.attributesLoaded)
		assert.NoError(model.peopleState.relationshipErr)
	})

	t.Run("notes retry settles an in-flight brief read", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		backend := &briefOverviewBackend{fakePeopleBriefBackend: &fakePeopleBriefBackend{
			fakePeopleBackend: &fakePeopleBackend{}, brief: testPersonBrief(),
		}}
		model := briefOverviewModel(backend, nil)
		model.peopleState.briefLoaded = false
		pending := model.beginPeopleBriefLoad()
		require.NotNil(pending, "the brief read must be in flight before the retry")
		model.loading = true

		model, retry := sendKey(t, model, key('r'))
		require.NotNil(retry, "the notes lane is due")
		assert.False(model.peopleState.briefLoading,
			"the fresh request ID drops the brief answer, so its flag settles here")

		model = sendMsg(t, model, runPeopleCommandMessage[peopleBriefLoadedMsg](t, pending))
		assert.False(model.peopleState.briefLoading, "the dropped answer does not re-arm the flag")
		for _, msg := range runBatchCommand(t, retry) {
			model = sendMsg(t, model, msg)
		}
		assert.False(model.loading, "the spinner stops once the retried notes load settles")
	})
}

// TestPeopleBriefCommandSettlesAnInFlightNotesLoad pins the palette half of the
// same family: submitting a command takes a fresh request ID while the notes
// load started with the previous one is still running.
func TestPeopleBriefCommandSettlesAnInFlightNotesLoad(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &briefOverviewBackend{fakePeopleBriefBackend: &fakePeopleBriefBackend{
		fakePeopleBackend: &fakePeopleBackend{}, brief: testPersonBrief(),
		run: &peoplebrowser.PersonBriefRun{RunID: "run-1", AttemptID: "attempt-1", BriefVersion: 3},
	}}
	model := briefOverviewModel(backend, testPersonBrief())
	model.peopleState.attributesLoading = true
	superseded := model.loadPeopleAttributes(51, peopleTabOverview)
	model.loading = true

	model, _ = sendKey(t, model, key(':'))
	require.Equal(peopleOverlayBriefCommand, model.peopleState.form.overlay)
	model = typePeopleBriefCommand(t, model, "brief generate")
	model, command := sendKey(t, model, keyEnter())
	require.NotNil(command, "a valid brief command runs against the daemon")
	assert.False(model.peopleState.attributesLoading,
		"the command's fresh request ID settles the notes load it superseded")
	assert.True(model.peopleState.briefCommandRunning)

	model = sendMsg(t, model, runPeopleCommandMessage[peopleAttributesLoadedMsg](t, superseded))
	assert.True(model.loading, "the running command still holds the spinner")

	updated, reload := model.Update(runPeopleCommandMessage[peopleBriefCommandMsg](t, command))
	model = asModel(t, updated)
	require.NotNil(reload, "a completed mutation re-reads the current version")
	for _, msg := range runBatchCommand(t, reload) {
		model = sendMsg(t, model, msg)
	}
	assert.False(model.peopleState.briefCommandRunning)
	assert.False(model.peopleState.attributesLoading)
	assert.False(model.loading, "no lane is left holding the spinner")
}
