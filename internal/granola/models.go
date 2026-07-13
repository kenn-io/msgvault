// Package granola imports meeting notes and transcripts from the Granola
// public API (https://docs.granola.ai) into the msgvault store. Each note
// becomes one conversation of type "meeting" holding a single
// "meeting_transcript" message whose body carries the AI summary and the
// full transcript.
package granola

import (
	"encoding/json"
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

// ListNotesOutput is the GET /v1/notes response envelope.
type ListNotesOutput struct {
	Notes   []NoteSummary `json:"notes"`
	HasMore bool          `json:"hasMore"`
	Cursor  string        `json:"cursor"`
}
