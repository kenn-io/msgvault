package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/textutil"
)

// briefCommandUsage names every command the brief palette accepts. It is shown
// whenever the palette is open and whenever a typed command is not one of them.
const briefCommandUsage = "brief enroll · brief generate · brief reject <reason>"

// peopleBriefUnpromoted is the overview's stand-in for a contact that has no
// durable person yet, because a brief is addressed by person ID.
const peopleBriefUnpromoted = "Promote with p to see the brief"

// briefDateFormat renders a generation or evidence date. The paragraph carries
// its own short dates; these are the machine-checkable ones beside them.
const briefDateFormat = "2006-01-02"

type peopleBriefCommand uint8

const (
	peopleBriefCommandNone peopleBriefCommand = iota
	peopleBriefCommandEnroll
	peopleBriefCommandGenerate
	peopleBriefCommandReject
)

type peopleBriefLoadedMsg struct {
	brief                  *peoplebrowser.PersonBrief
	err                    error
	requestID              uint64
	participantID          int64
	personID               int64
	presentationGeneration uint64
}

type peopleBriefCommandMsg struct {
	command                peopleBriefCommand
	enrollment             *peoplebrowser.PersonBriefEnrollment
	run                    *peoplebrowser.PersonBriefRun
	brief                  *peoplebrowser.PersonBrief
	err                    error
	requestID              uint64
	participantID          int64
	personID               int64
	presentationGeneration uint64
}

// peopleBriefReader reports the backend's optional brief read surface.
func (m Model) peopleBriefReader() (peoplebrowser.PersonBriefReader, bool) {
	reader, ok := m.peopleBackend.(peoplebrowser.PersonBriefReader)
	return reader, ok
}

// peopleBriefWriter reports the backend's optional brief mutation surface.
func (m Model) peopleBriefWriter() (peoplebrowser.PersonBriefWriter, bool) {
	writer, ok := m.peopleBackend.(peoplebrowser.PersonBriefWriter)
	return writer, ok
}

// peopleBriefPersonID returns the durable person the open contact resolves to,
// or zero when the contact has not been promoted.
func (m Model) peopleBriefPersonID() int64 {
	contact := m.peopleState.contact
	if contact == nil || contact.Profile == nil {
		return 0
	}
	return contact.Profile.ID
}

// beginPeopleBriefLoad starts a brief read for the open contact, or reports
// that there is nothing to read.
func (m *Model) beginPeopleBriefLoad() tea.Cmd {
	personID := m.peopleBriefPersonID()
	if personID <= 0 {
		return nil
	}
	if _, ok := m.peopleBriefReader(); !ok {
		return nil
	}
	m.peopleState.briefLoading = true
	m.peopleState.briefErr = nil
	return m.loadPeopleBrief(personID)
}

// peopleBriefRetryDue reports that the Overview's brief failed or never
// loaded and that the backend can serve one, so r has something to restart.
func (m Model) peopleBriefRetryDue() bool {
	if m.peopleState.briefLoading || m.peopleBriefPersonID() <= 0 {
		return false
	}
	if _, ok := m.peopleBriefReader(); !ok {
		return false
	}
	return m.peopleState.briefErr != nil || !m.peopleState.briefLoaded
}

func (m Model) loadPeopleBrief(personID int64) tea.Cmd {
	reader, ok := m.peopleBriefReader()
	if !ok {
		return nil
	}
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	presentationGeneration := m.presentationGeneration
	return safeCmdWithPanic(
		func() tea.Msg {
			brief, err := reader.GetPersonBrief(context.Background(), personID)
			return peopleBriefLoadedMsg{
				brief: brief, err: err, requestID: requestID,
				participantID: participantID, personID: personID,
				presentationGeneration: presentationGeneration,
			}
		},
		func(r any) tea.Msg {
			return peopleBriefLoadedMsg{
				err: fmt.Errorf("people brief panic: %v", r), requestID: requestID,
				participantID: participantID, personID: personID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) handlePeopleBriefLoaded(msg peopleBriefLoadedMsg) (tea.Model, tea.Cmd) {
	if !m.peopleAsyncContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID, msg.personID,
		peopleTabOverview,
	) {
		return m, nil
	}
	m.peopleState.briefLoading = false
	if msg.err != nil {
		m.peopleState.briefErr = fmt.Errorf("load brief failed: %w", msg.err)
		m.peopleState.briefStructured = false
		m.updatePeopleLoading()
		return m, nil
	}
	m.peopleState.brief = msg.brief
	m.peopleState.briefLoaded = true
	m.peopleState.briefErr = nil
	if msg.brief == nil {
		m.peopleState.briefStructured = false
	}
	m.updatePeopleLoading()
	return m, nil
}

func (m Model) runPeopleBriefCommand(
	command peopleBriefCommand, personID int64, reason string,
) tea.Cmd {
	writer, ok := m.peopleBriefWriter()
	if !ok {
		return nil
	}
	requestID := m.peopleState.requestID
	participantID := m.peopleState.participantID
	presentationGeneration := m.presentationGeneration
	result := func() peopleBriefCommandMsg {
		msg := peopleBriefCommandMsg{
			command: command, requestID: requestID, participantID: participantID,
			personID: personID, presentationGeneration: presentationGeneration,
		}
		switch command {
		case peopleBriefCommandEnroll:
			// The palette has no flags, so enrolling also adds the tracking
			// row enrollment requires; without it the daemon refuses.
			msg.enrollment, msg.err = writer.SetPersonBriefEnrollment(
				context.Background(), personID, true, true)
		case peopleBriefCommandGenerate:
			msg.run, msg.err = writer.GeneratePersonBrief(context.Background(), personID)
		case peopleBriefCommandReject:
			msg.brief, msg.err = writer.RejectPersonBrief(context.Background(), personID, reason)
		case peopleBriefCommandNone:
			msg.err = errors.New("unknown brief command")
		}
		return msg
	}
	return safeCmdWithPanic(
		func() tea.Msg { return result() },
		func(r any) tea.Msg {
			return peopleBriefCommandMsg{
				command: command, err: fmt.Errorf("people brief command panic: %v", r),
				requestID: requestID, participantID: participantID, personID: personID,
				presentationGeneration: presentationGeneration,
			}
		},
	)
}

func (m Model) handlePeopleBriefCommand(msg peopleBriefCommandMsg) (tea.Model, tea.Cmd) {
	if !m.peopleAsyncContextMatches(
		msg.presentationGeneration, msg.requestID, msg.participantID, msg.personID,
		peopleTabOverview,
	) {
		return m, nil
	}
	m.peopleState.briefCommandRunning = false
	if msg.err != nil {
		m.peopleState.briefNotice = peopleBriefCommandFailureNotice(msg.err)
		m.updatePeopleLoading()
		return m, nil
	}
	switch msg.command {
	case peopleBriefCommandEnroll:
		m.peopleState.briefNotice = "Person enrolled for briefs; tracking is on."
	case peopleBriefCommandGenerate:
		m.peopleState.briefNotice = peopleBriefRunNotice(msg.run)
	case peopleBriefCommandReject:
		m.peopleState.briefNotice = peopleBriefRejectNotice(msg.brief)
	case peopleBriefCommandNone:
		m.peopleState.briefNotice = ""
	}
	if msg.command == peopleBriefCommandNone {
		m.updatePeopleLoading()
		return m, nil
	}
	// Every mutation can change the current version, so re-read it rather than
	// projecting the daemon's answer onto the cached brief. The re-read takes a
	// fresh request ID, which supersedes a notes load still in flight, so
	// bumpRequestID settles that one instead of stranding the spinner on it.
	m.peopleState.bumpRequestID()
	if cmd := m.beginPeopleBriefLoad(); cmd != nil {
		m.loading = true
		return m, tea.Batch(m.startSpinner(), cmd)
	}
	m.updatePeopleLoading()
	return m, nil
}

// peopleBriefCommandFailureNotice turns a refused brief command into the
// sentence the owner can act on. The daemon answers each gate a manual
// generation can hit with its own stable code, so the palette says what stands
// in the way instead of echoing an HTTP status line.
func peopleBriefCommandFailureNotice(err error) string {
	var coded interface{ APIErrorCode() string }
	if errors.As(err, &coded) {
		switch coded.APIErrorCode() {
		case "person_brief_not_enrolled":
			return "No brief: this person is not enrolled. Run brief enroll first."
		case "person_brief_lane_disabled":
			return "No brief: brief generation is turned off in the daemon configuration."
		case "person_brief_policy_refused":
			return "No brief: the people inference profile does not allow sensitive " +
				"content, which every brief carries."
		case "person_brief_no_supported_lane":
			return "No brief: the people inference profile does not allow conversation " +
				"text, the only source a brief reads in this version."
		case "person_brief_busy":
			return "No brief: another worker is sweeping this person right now. " +
				"Retry once it finishes."
		}
	}
	return "Brief command failed: " + textutil.SanitizeTerminal(err.Error())
}

func peopleBriefRunNotice(run *peoplebrowser.PersonBriefRun) string {
	if run == nil {
		return "Brief generation returned no run."
	}
	attempt := run.AttemptID
	if attempt == "" {
		attempt = "none"
	}
	if run.BriefVersion > 0 {
		return "Brief attempt " + textutil.SanitizeTerminal(attempt) +
			": version " + strconv.Itoa(run.BriefVersion) + "."
	}
	failure := run.BriefFailureClass
	if failure == "" {
		failure = "no reason reported"
	}
	return "Brief attempt " + textutil.SanitizeTerminal(attempt) +
		": no new version (" + textutil.SanitizeTerminal(failure) + ")."
}

func peopleBriefRejectNotice(brief *peoplebrowser.PersonBrief) string {
	if brief == nil {
		return "Brief rejected."
	}
	return "Brief v" + strconv.Itoa(brief.Version) + " rejected."
}

// parsePeopleBriefCommand maps one typed palette line onto a command. The
// reason is everything after "brief reject" and may be empty.
func parsePeopleBriefCommand(line string) (peopleBriefCommand, string) {
	trimmed := strings.TrimSpace(line)
	rest, ok := strings.CutPrefix(trimmed, "brief")
	if !ok || (rest != "" && !strings.HasPrefix(rest, " ")) {
		return peopleBriefCommandNone, ""
	}
	rest = strings.TrimSpace(rest)
	switch {
	case rest == "enroll":
		return peopleBriefCommandEnroll, ""
	case rest == "generate":
		return peopleBriefCommandGenerate, ""
	case rest == "reject":
		return peopleBriefCommandReject, ""
	case strings.HasPrefix(rest, "reject "):
		return peopleBriefCommandReject, strings.TrimSpace(strings.TrimPrefix(rest, "reject "))
	default:
		return peopleBriefCommandNone, ""
	}
}

func newPeopleBriefCommandForm() peopleFormState {
	input := textinput.New()
	input.Placeholder = "brief generate"
	input.CharLimit = 200
	input.SetWidth(54)
	input.Focus()
	return peopleFormState{overlay: peopleOverlayBriefCommand, commandInput: input}
}

// handlePeopleBriefKey owns the brief's keys on the Overview tab: b toggles the
// structured view, : opens the command palette, and Esc leaves the structured
// view without leaving the contact. None of them needs a command: the palette's
// input takes focus when the overlay is built.
func (m Model) handlePeopleBriefKey(msg tea.KeyPressMsg) (Model, bool) {
	if m.peopleState.briefStructured {
		switch msg.String() {
		case keyNameEsc, keyNameBackspace, "b":
			m.peopleState.briefStructured = false
			m.peopleState.scrollOffset = 0
			return m, true
		case "[", "]", "n":
			// The relationship year and the notes editor belong to the
			// overview, which this view has replaced. Swallow them rather than
			// acting on a surface the reader cannot see.
			return m, true
		}
	}
	switch msg.String() {
	case "b":
		if m.peopleState.brief == nil {
			return m, false
		}
		m.peopleState.briefStructured = true
		m.peopleState.scrollOffset = 0
		return m, true
	case ":":
		if m.peopleBriefPersonID() <= 0 {
			return m, false
		}
		if _, ok := m.peopleBriefWriter(); !ok {
			return m, false
		}
		m.peopleState.briefNotice = ""
		m.peopleState.form = newPeopleBriefCommandForm()
		return m, true
	}
	return m, false
}

// handlePeopleBriefCommandKey drives the palette. It never runs while a
// command is in flight: submitting closes the overlay before dispatching, so
// the outcome lands on the Overview's info line rather than in a modal the
// reader has to dismiss.
func (m Model) handlePeopleBriefCommandKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	form := &m.peopleState.form
	switch msg.String() {
	case keyNameEsc:
		form.close()
		m.updatePeopleLoading()
		return m, nil
	case keyNameCtrlC:
		m.quitting = true
		return m, tea.Quit
	case keyNameEnter:
		return m.submitPeopleBriefCommand()
	}
	var cmd tea.Cmd
	form.commandInput, cmd = form.commandInput.Update(msg)
	form.notice = ""
	return m, cmd
}

func (m Model) submitPeopleBriefCommand() (tea.Model, tea.Cmd) {
	form := &m.peopleState.form
	personID := m.peopleBriefPersonID()
	if personID <= 0 {
		form.notice = peopleBriefUnpromoted
		return m, nil
	}
	command, reason := parsePeopleBriefCommand(form.commandInput.Value())
	if command == peopleBriefCommandNone {
		form.notice = "Unknown command. Try " + briefCommandUsage
		return m, nil
	}
	if _, ok := m.peopleBriefWriter(); !ok {
		form.notice = "Brief commands are unavailable on this backend."
		return m, nil
	}
	form.close()
	// The command captures the request ID its answer is matched against, so
	// bump it first: an answer that arrives after the contact moved on is
	// dropped by peopleAsyncContextMatches. The same bump supersedes a notes
	// load still in flight, which bumpRequestID settles rather than strands.
	m.peopleState.bumpRequestID()
	cmd := m.runPeopleBriefCommand(command, personID, reason)
	m.peopleState.briefCommandRunning = true
	m.peopleState.briefNotice = peopleBriefRunningNotice(command)
	m.loading = true
	return m, tea.Batch(m.startSpinner(), cmd)
}

func peopleBriefRunningNotice(command peopleBriefCommand) string {
	switch command {
	case peopleBriefCommandEnroll:
		return "Enrolling for briefs..."
	case peopleBriefCommandGenerate:
		return "Generating a brief; this spends provider budget..."
	case peopleBriefCommandReject:
		return "Rejecting the current brief..."
	case peopleBriefCommandNone:
		return ""
	}
	return ""
}

func (m Model) peopleBriefCommandFormView() string {
	form := m.peopleState.form
	lines := []string{
		m.styles.modalTitle.Render("Brief commands"), "",
		"> " + form.commandInput.View(), "", briefCommandUsage,
	}
	if form.notice != "" {
		lines = append(lines, "", m.styles.err.Render(form.notice))
	}
	return strings.Join(lines, "\n") + "\n\nEnter runs · Esc cancels"
}

// peopleOverviewBriefLines renders the brief under the contact-state lines: the
// stored version and date, then the paragraph the owner reads.
func (m Model) peopleOverviewBriefLines() []string {
	if _, ok := m.peopleBriefReader(); !ok {
		return nil
	}
	heading := []string{"", " Last time we talked"}
	if m.peopleBriefPersonID() <= 0 {
		return append(heading, " "+peopleBriefUnpromoted)
	}
	switch {
	case m.peopleState.briefErr != nil:
		return append(heading, " Brief is unavailable. Press r to retry.")
	case m.peopleState.briefLoading && m.peopleState.brief == nil:
		return append(heading, " "+m.spinnerIndicator()+" Loading brief...")
	case !m.peopleState.briefLoaded:
		return append(heading, " The brief has not loaded. Press r to retry.")
	case m.peopleState.brief == nil:
		return append(heading, " No brief yet. Press : to run "+briefCommandUsage)
	}
	brief := m.peopleState.brief
	lines := make([]string, 0, len(heading)+4)
	lines = append(lines, heading...)
	lines = append(lines, " "+peopleBriefVersionLine(brief))
	for _, line := range wrapText(
		textutil.SanitizeTerminal(brief.RenderedText), max(m.width-1, 1),
	) {
		lines = append(lines, " "+line)
	}
	return append(lines, " Press b for the structured view")
}

func peopleBriefVersionLine(brief *peoplebrowser.PersonBrief) string {
	line := "Brief v" + strconv.Itoa(brief.Version) + ", " + formatPeopleBriefDate(brief.GeneratedAt)
	if brief.RejectedAt != nil {
		line += " (rejected)"
	}
	return line
}

// peopleBriefStructuredLines renders one section per structured kind with the
// date of every archive item the kind's entries cite. The last interaction
// leads: it is the one item every brief carries, so a brief made of nothing
// else must still show it rather than read as empty.
func (m Model) peopleBriefStructuredLines() []string {
	brief := m.peopleState.brief
	if brief == nil {
		return []string{" No brief to expand"}
	}
	header := " " + peopleBriefVersionLine(brief) + " — structured view"
	if brief.DroppedItemCount > 0 {
		header += " · Dropped items: " + strconv.Itoa(brief.DroppedItemCount)
	}
	lines := []string{header}
	if brief.RejectedAt != nil {
		rejected := " Rejected " + formatPeopleBriefDate(*brief.RejectedAt)
		if reason := strings.TrimSpace(brief.RejectedReason); reason != "" {
			rejected += ": " + textutil.SanitizeTerminal(reason)
		}
		lines = append(lines, rejected)
	}
	if len(brief.Evidence) > 0 && !slices.ContainsFunc(brief.Items, func(item peoplebrowser.PersonBriefItem) bool {
		return len(item.Evidence) > 0
	}) {
		lines = append(lines, "", " Whole brief evidence (item links unavailable)")
		lines = append(lines, peopleBriefEvidenceLines(brief.Evidence)...)
	}
	for _, section := range []struct {
		heading string
		kind    peoplebrowser.PersonBriefItemKind
	}{
		{"Last interaction", peoplebrowser.PersonBriefLastInteraction},
		{"Highlights", peoplebrowser.PersonBriefHighlight},
		{"Follow-ups", peoplebrowser.PersonBriefFollowUp},
		{"Appreciations", peoplebrowser.PersonBriefAppreciation},
		{"Uncertainties", peoplebrowser.PersonBriefUncertainty},
	} {
		lines = append(lines, "", " "+section.heading)
		lines = append(lines, m.peopleBriefSectionLines(brief.Items, section.kind)...)
	}
	return append(lines, "", " Esc returns to the overview")
}

func (m Model) peopleBriefSectionLines(
	items []peoplebrowser.PersonBriefItem, kind peoplebrowser.PersonBriefItemKind,
) []string {
	var lines []string
	for _, item := range items {
		if item.Kind != kind {
			continue
		}
		for index, line := range wrapText(
			textutil.SanitizeTerminal(peopleBriefItemText(item)), max(m.width-4, 1),
		) {
			prefix := "   "
			if index == 0 {
				prefix = " - "
			}
			lines = append(lines, prefix+line)
		}
		if why := strings.TrimSpace(item.Why); why != "" {
			lines = append(lines, "     why: "+textutil.SanitizeTerminal(why))
		}
		lines = append(lines, peopleBriefEvidenceLines(item.Evidence)...)
	}
	if len(lines) == 0 {
		return []string{" - None"}
	}
	return lines
}

func peopleBriefItemText(item peoplebrowser.PersonBriefItem) string {
	text := item.Text
	if reason := strings.TrimSpace(item.Reason); reason != "" {
		text += " (" + reason + ")"
	}
	return text
}

func peopleBriefEvidenceLines(evidence []peoplebrowser.PersonBriefEvidence) []string {
	if len(evidence) == 0 {
		return []string{"     no evidence reference"}
	}
	parts := make([]string, 0, len(evidence))
	for _, reference := range evidence {
		part := formatPeopleBriefDate(reference.EventTime)
		if !reference.Supported {
			part += " (unsupported)"
		}
		parts = append(parts, part)
	}
	return []string{"     " + strings.Join(parts, ", ")}
}

func formatPeopleBriefDate(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.Format(briefDateFormat)
}
