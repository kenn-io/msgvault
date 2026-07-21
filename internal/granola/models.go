// Package granola imports meeting notes and transcripts from the Granola
// public API (https://docs.granola.ai) into the msgvault store. Each note
// becomes one conversation of type "meeting" holding a single
// "meeting_transcript" message whose body carries the AI summary and the
// full transcript.
package granola

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	// SourceType is the sources.source_type value for Granola accounts.
	SourceType = "granola"
	// MessageType is the messages.message_type value for meeting notes.
	MessageType = "meeting_transcript"
	// ConversationType is the conversations.conversation_type value.
	ConversationType = "meeting"
	// RawFormat tags message_raw rows holding the verbatim API response.
	RawFormat = "granola_json"
	// DefaultBaseURL is the production API host.
	DefaultBaseURL = "https://public-api.granola.ai"
)

type apiTimestamp time.Time

func (t *apiTimestamp) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*t = apiTimestamp(time.Time{})
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode Granola timestamp: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		parsed, err = time.Parse(time.DateOnly, value)
	}
	if err != nil {
		return fmt.Errorf("parse Granola timestamp %q: %w", value, err)
	}
	*t = apiTimestamp(parsed)
	return nil
}

// User is a note owner or meeting attendee. Email is always present; name may
// be empty.
type User struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Speaker identifies who produced a transcript segment. Source is
// "microphone" (the local user) or "speaker" (the remote side). Name is set
// only when Granola could identify the speaker; DiarizationLabel carries
// anonymous "Speaker A/B/..." buckets when diarization is available.
type Speaker struct {
	Source           string `json:"source"`
	DiarizationLabel string `json:"diarization_label,omitempty"`
	Name             string `json:"name,omitempty"`
}

// TranscriptSegment is one utterance. Start/end times are absolute
// timestamps, not offsets into the meeting.
type TranscriptSegment struct {
	Speaker   Speaker   `json:"speaker"`
	Text      string    `json:"text"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
}

func (s *TranscriptSegment) UnmarshalJSON(data []byte) error {
	type plain TranscriptSegment
	decoded := struct {
		*plain

		StartTime apiTimestamp `json:"start_time"`
		EndTime   apiTimestamp `json:"end_time"`
	}{plain: (*plain)(s)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	s.StartTime = time.Time(decoded.StartTime)
	s.EndTime = time.Time(decoded.EndTime)
	return nil
}

// CalendarInvitee is an invitee on the calendar event backing a note.
type CalendarInvitee struct {
	Email string `json:"email"`
}

// CalendarEvent is the calendar event a note was captured against. All
// fields are nullable in the API; zero values mean absent. Note the British
// spelling of "organiser" in the wire format.
type CalendarEvent struct {
	EventTitle         string            `json:"event_title"`
	Invitees           []CalendarInvitee `json:"invitees"`
	Organiser          string            `json:"organiser"`
	CalendarEventID    string            `json:"calendar_event_id"`
	ScheduledStartTime time.Time         `json:"scheduled_start_time"`
	ScheduledEndTime   time.Time         `json:"scheduled_end_time"`
}

func (e *CalendarEvent) UnmarshalJSON(data []byte) error {
	type plain CalendarEvent
	decoded := struct {
		*plain

		ScheduledStartTime apiTimestamp `json:"scheduled_start_time"`
		ScheduledEndTime   apiTimestamp `json:"scheduled_end_time"`
	}{plain: (*plain)(e)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	e.ScheduledStartTime = time.Time(decoded.ScheduledStartTime)
	e.ScheduledEndTime = time.Time(decoded.ScheduledEndTime)
	return nil
}

// Folder is a Granola workspace folder.
type Folder struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	ParentFolderID *string `json:"parent_folder_id"`
}

// NoteSummary is a list-endpoint item.
type NoteSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Owner     User      `json:"owner"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *NoteSummary) UnmarshalJSON(data []byte) error {
	type plain NoteSummary
	decoded := struct {
		*plain

		CreatedAt apiTimestamp `json:"created_at"`
		UpdatedAt apiTimestamp `json:"updated_at"`
	}{plain: (*plain)(s)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	s.CreatedAt = time.Time(decoded.CreatedAt)
	s.UpdatedAt = time.Time(decoded.UpdatedAt)
	return nil
}

// Note is the full note returned by GET /v1/notes/{id}?include=transcript.
// Raw preserves the verbatim response body for archival in message_raw.
type Note struct {
	NoteSummary

	WebURL           string              `json:"web_url"`
	CalendarEvent    *CalendarEvent      `json:"calendar_event"`
	Attendees        []User              `json:"attendees"`
	FolderMembership []Folder            `json:"folder_membership"`
	SummaryText      string              `json:"summary_text"`
	SummaryMarkdown  string              `json:"summary_markdown"`
	Transcript       []TranscriptSegment `json:"transcript"`

	Raw json.RawMessage `json:"-"`
}

func (n *Note) UnmarshalJSON(data []byte) error {
	var summary NoteSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return err
	}
	var fields struct {
		WebURL           string              `json:"web_url"`
		CalendarEvent    *CalendarEvent      `json:"calendar_event"`
		Attendees        []User              `json:"attendees"`
		FolderMembership []Folder            `json:"folder_membership"`
		SummaryText      string              `json:"summary_text"`
		SummaryMarkdown  string              `json:"summary_markdown"`
		Transcript       []TranscriptSegment `json:"transcript"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*n = Note{
		NoteSummary:      summary,
		WebURL:           fields.WebURL,
		CalendarEvent:    fields.CalendarEvent,
		Attendees:        fields.Attendees,
		FolderMembership: fields.FolderMembership,
		SummaryText:      fields.SummaryText,
		SummaryMarkdown:  fields.SummaryMarkdown,
		Transcript:       fields.Transcript,
	}
	return nil
}

// ListNotesOutput is the GET /v1/notes response envelope.
type ListNotesOutput struct {
	Notes   []NoteSummary `json:"notes"`
	HasMore bool          `json:"hasMore"`
	Cursor  string        `json:"cursor"`
}
