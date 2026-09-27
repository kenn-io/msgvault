package muesli

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
)

const rawSchemaVersion = 1

// SkipReason explains why a Muesli meeting is not archived; empty means it
// is archived.
type SkipReason string

const (
	SkipDeleted    SkipReason = "deleted"
	SkipInProgress SkipReason = "in_progress"
	SkipEmpty      SkipReason = "empty"
)

// Notes states. The first three are Muesli's MeetingNotesState values;
// summary_failed is msgvault's addition for Muesli's failure notice.
const (
	NotesMissing               = "missing"
	NotesRawTranscriptFallback = "raw_transcript_fallback"
	NotesSummaryFailed         = "summary_failed"
	NotesStructured            = "structured_notes"
)

// Eligibility reports whether the meeting should be archived. Deleted
// meetings are never archived, so a Muesli tombstone cannot overwrite an
// archived copy. Meetings still recording or processing wait for a later
// sync.
func (m Meeting) Eligibility() SkipReason {
	switch {
	case m.Deleted:
		return SkipDeleted
	case m.Status == "recording" || m.Status == "processing":
		return SkipInProgress
	case strings.TrimSpace(m.RawTranscript) == "" &&
		NotesState(m.FormattedNotes) != NotesStructured &&
		strings.TrimSpace(m.ManualNotes) == "":
		return SkipEmpty
	default:
		return ""
	}
}

// NotesState classifies Muesli's formatted notes the way Muesli's own
// MeetingRecord.notesState does, adding summary_failed.
func NotesState(notes string) string {
	normalized := strings.ToLower(strings.TrimSpace(notes))
	switch {
	case normalized == "":
		return NotesMissing
	case headingOnlyOrFirst(normalized, "## raw transcript"):
		return NotesRawTranscriptFallback
	case headingOnlyOrFirst(normalized, "## summary failed"):
		return NotesSummaryFailed
	default:
		return NotesStructured
	}
}

func headingOnlyOrFirst(normalized, heading string) bool {
	return normalized == heading || strings.HasPrefix(normalized, heading+"\n")
}

// SourceMessageID is the stable archive key. Muesli's id restarts after a
// database reset and its start time changes when a meeting is resumed, so the
// key pairs the id with the row's insert time.
func (m Meeting) SourceMessageID() (string, error) {
	created, err := parseCreatedAt(m.CreatedAt)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("meeting:%d:%s", m.ID, created.Format("20060102T150405Z")), nil
}

func parseCreatedAt(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("created_at is missing")
	}
	// SQLite's datetime('now') is UTC without a zone suffix.
	if parsed, err := time.Parse(time.DateTime, value); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("created_at %q is not a timestamp", value)
}

type rawParticipant struct {
	// Ref is a short hash of Muesli's participant identifier. It lets a later
	// sync carry this participant's Contacts identities forward while Contacts
	// is unreadable, without storing the Contacts identifier.
	Ref    string `json:"ref,omitempty"`
	Name   string `json:"name,omitempty"`
	Email  string `json:"email,omitempty"`
	Phone  string `json:"phone,omitempty"`
	Source string `json:"source,omitempty"`
	// Emails and Phones are the participant's Contacts identities.
	Emails []string `json:"emails,omitempty"`
	Phones []string `json:"phones,omitempty"`
}

type rawMeeting struct {
	ID               int64   `json:"id"`
	Title            string  `json:"title"`
	StartTime        string  `json:"start_time"`
	EndTime          string  `json:"end_time,omitempty"`
	DurationSeconds  float64 `json:"duration_seconds,omitzero"`
	Status           string  `json:"status,omitempty"`
	Source           string  `json:"source,omitempty"`
	FormattedNotes   string  `json:"formatted_notes,omitempty"`
	NotesState       string  `json:"notes_state"`
	ManualNotes      string  `json:"manual_notes,omitempty"`
	RawTranscript    string  `json:"raw_transcript,omitempty"`
	WordCount        int64   `json:"word_count,omitzero"`
	TemplateName     string  `json:"template_name,omitempty"`
	TemplateKind     string  `json:"template_kind,omitempty"`
	CalendarEventID  string  `json:"calendar_event_id,omitempty"`
	CalendarSource   string  `json:"calendar_source,omitempty"`
	CalendarSeriesID string  `json:"calendar_series_id,omitempty"`
	Folder           string  `json:"folder,omitempty"`
	FollowUpToID     int64   `json:"follow_up_to_id,omitzero"`
	CreatedAt        string  `json:"created_at"`
}

// rawEvidence is the archived muesli_json document. It holds only content
// that describes the meeting: no audio paths, template prompts, screen text,
// sync bookkeeping, or Contacts identifiers.
type rawEvidence struct {
	SchemaVersion int              `json:"schema_version"`
	Meeting       rawMeeting       `json:"meeting"`
	Participants  []rawParticipant `json:"participants,omitempty"`
}

type meetingMetadata struct {
	Platform             string  `json:"platform"`
	SourceIdentifier     string  `json:"source_identifier,omitempty"`
	MuesliID             int64   `json:"muesli_id"`
	Status               string  `json:"status,omitempty"`
	Source               string  `json:"source,omitempty"`
	StartedAt            string  `json:"started_at"`
	EndedAt              string  `json:"ended_at,omitempty"`
	DurationSeconds      float64 `json:"duration_seconds,omitzero"`
	NotesState           string  `json:"notes_state"`
	HasSummary           bool    `json:"has_summary"`
	HasNotes             bool    `json:"has_notes"`
	HasTranscript        bool    `json:"has_transcript"`
	TemplateName         string  `json:"template_name,omitempty"`
	Folder               string  `json:"folder,omitempty"`
	CalendarEventID      string  `json:"calendar_event_id,omitempty"`
	CalendarSource       string  `json:"calendar_source,omitempty"`
	FollowUpToID         int64   `json:"follow_up_to_id,omitzero"`
	NameOnlyParticipants int     `json:"name_only_participants,omitzero"`
	Contacts             string  `json:"contacts,omitempty"`
	ContactsResolved     int     `json:"contacts_resolved,omitzero"`
	ContactsCarried      int     `json:"contacts_carried_forward,omitzero"`
	ContactsUnresolved   int     `json:"contacts_unresolved,omitzero"`
	ContactsPhoneSkipped int     `json:"contacts_phones_skipped,omitzero"`
}

// ArchiveSnapshot converts the meeting into the canonical archive shape.
// Muesli records no organizer, so the configured account (the person who
// recorded the meeting) is the organizer.
func (m Meeting) ArchiveSnapshot(sourceID int64, identifier, accountEmail string) (meetingarchive.Snapshot, error) {
	key, err := m.SourceMessageID()
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	created, _ := parseCreatedAt(m.CreatedAt)
	started, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(m.StartTime))
	if err != nil {
		return meetingarchive.Snapshot{}, fmt.Errorf("start_time %q is not a timestamp", m.StartTime)
	}
	started = started.UTC()
	ended := m.endedAt(started)

	title := strings.Join(strings.Fields(m.Title), " ")
	if title == "" {
		title = "Meeting on " + started.Format(time.DateOnly)
	}
	notesState := NotesState(m.FormattedNotes)
	summary := ""
	if notesState == NotesStructured {
		summary = strings.TrimSpace(m.FormattedNotes)
	}
	notes := strings.TrimSpace(m.ManualNotes)
	transcript := strings.TrimSpace(m.RawTranscript)

	people := dedupeParticipants(m.Participants)
	var attendees []meetingarchive.Person
	var labels []string
	nameOnly := 0
	var resolved, carried, unresolved, skippedPhones int
	rawPeople := make([]rawParticipant, 0, len(people))
	for _, participant := range people {
		person := participant.archivePerson()
		rawPeople = append(rawPeople, participant.raw(person))
		label := person.Name
		if label == "" {
			label = person.Email
		}
		if label == "" {
			label = person.Phone
		}
		if label != "" {
			labels = append(labels, label)
		}
		skippedPhones += participant.SkippedPhones
		switch participant.Resolution {
		case resolutionResolved:
			resolved++
		case resolutionCarried:
			carried++
		default:
			unresolved++
		}
		if person.PrimaryKey() == "" {
			nameOnly++
			continue
		}
		attendees = append(attendees, person)
	}
	contactsState := ""
	if m.ContactsState != "" {
		contactsState = string(m.ContactsState)
	}
	if contactsState == "" || m.ContactsState == ContactsOff {
		resolved, carried, unresolved, skippedPhones = 0, 0, 0, 0
	}

	body := meetingBody(title, started, ended, labels, summary, notes, transcript)
	raw, err := json.Marshal(rawEvidence{
		SchemaVersion: rawSchemaVersion,
		Meeting: rawMeeting{
			ID: m.ID, Title: m.Title, StartTime: m.StartTime, EndTime: m.EndTime,
			DurationSeconds: m.DurationSeconds, Status: m.Status, Source: m.Source,
			FormattedNotes: m.FormattedNotes, NotesState: notesState, ManualNotes: m.ManualNotes,
			RawTranscript: m.RawTranscript, WordCount: m.WordCount,
			TemplateName: m.TemplateName, TemplateKind: m.TemplateKind,
			CalendarEventID: m.CalendarEventID, CalendarSource: m.CalendarSource,
			CalendarSeriesID: m.CalendarSeriesID, Folder: m.Folder, FollowUpToID: m.FollowUpToID,
			CreatedAt: created.Format(time.RFC3339),
		},
		Participants: rawPeople,
	}, json.Deterministic(true))
	if err != nil {
		return meetingarchive.Snapshot{}, fmt.Errorf("marshal Muesli raw evidence: %w", err)
	}
	metadata, err := json.Marshal(meetingMetadata{
		Platform: SourceType, SourceIdentifier: identifier, MuesliID: m.ID,
		Status: m.Status, Source: m.Source,
		StartedAt: started.Format(time.RFC3339Nano), EndedAt: formatOptionalTime(ended),
		DurationSeconds: m.DurationSeconds, NotesState: notesState,
		HasSummary: summary != "", HasNotes: notes != "", HasTranscript: transcript != "",
		TemplateName: m.TemplateName, Folder: m.Folder,
		CalendarEventID: m.CalendarEventID, CalendarSource: m.CalendarSource,
		FollowUpToID: m.FollowUpToID, NameOnlyParticipants: nameOnly,
		Contacts: contactsState, ContactsResolved: resolved,
		ContactsCarried: carried, ContactsUnresolved: unresolved,
		ContactsPhoneSkipped: skippedPhones,
	}, json.Deterministic(true))
	if err != nil {
		return meetingarchive.Snapshot{}, fmt.Errorf("marshal Muesli meeting metadata: %w", err)
	}

	var organizer *meetingarchive.Person
	if email := strings.ToLower(strings.TrimSpace(accountEmail)); email != "" {
		organizer = &meetingarchive.Person{Email: email}
	}
	return meetingarchive.Snapshot{
		SourceID: sourceID, AccountEmail: accountEmail,
		SourceMessageID: key, SourceConversationID: key,
		Title: title, StartedAt: started, Body: body, Snippet: snippet(body),
		Metadata: metadata, Raw: raw, RawFormat: RawFormat,
		Organizer: organizer, Attendees: attendees,
	}, nil
}

func (m Meeting) endedAt(started time.Time) time.Time {
	if value := strings.TrimSpace(m.EndTime); value != "" {
		if ended, err := time.Parse(time.RFC3339Nano, value); err == nil && ended.After(started) {
			return ended.UTC()
		}
	}
	if m.DurationSeconds > 0 {
		return started.Add(time.Duration(m.DurationSeconds * float64(time.Second)))
	}
	return time.Time{}
}

// dedupeParticipants drops repeated emails (Muesli can list the same person
// from the calendar and from Contacts) and fully blank rows. The first row
// keeps its place; a later duplicate only supplies a name the first lacks.
func dedupeParticipants(participants []Participant) []Participant {
	out := make([]Participant, 0, len(participants))
	index := map[string]int{}
	for _, person := range participants {
		person.Name = strings.TrimSpace(person.Name)
		person.Email = strings.ToLower(strings.TrimSpace(person.Email))
		if person.Name == "" && person.Email == "" && len(person.ContactEmails) == 0 && len(person.ContactPhones) == 0 {
			continue
		}
		if person.Email != "" {
			if at, seen := index[person.Email]; seen {
				if out[at].Name == "" {
					out[at].Name = person.Name
				}
				if out[at].Resolution == "" && person.Resolution != "" {
					out[at].ContactEmails, out[at].ContactPhones = person.ContactEmails, person.ContactPhones
					out[at].Anchor, out[at].Resolution = person.Anchor, person.Resolution
				}
				continue
			}
			index[person.Email] = len(out)
		}
		out = append(out, person)
	}
	return out
}

func meetingBody(title string, start, end time.Time, attendees []string, summary, notes, transcript string) string {
	lines := []string{title}
	when := "When: " + start.Format("2006-01-02 15:04")
	if !end.IsZero() {
		when += " - " + end.UTC().Format("15:04")
	}
	lines = append(lines, when)
	if len(attendees) > 0 {
		lines = append(lines, "Attendees: "+strings.Join(attendees, ", "))
	}
	for _, section := range []struct{ label, content string }{
		{"Summary", summary}, {"Notes", notes}, {"Transcript", transcript},
	} {
		if section.content != "" {
			lines = append(lines, "", section.label+":", section.content)
		}
	}
	return strings.Join(lines, "\n")
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func snippet(body string) string {
	runes := []rune(strings.TrimSpace(body))
	if len(runes) > 200 {
		runes = runes[:200]
	}
	return string(runes)
}
