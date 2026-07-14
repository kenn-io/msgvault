package circleback

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	// SourceType is the sources.source_type value for Circleback accounts.
	SourceType = "circleback"
	// MessageType is the messages.message_type value for meeting notes.
	MessageType = "meeting_transcript"
	// ConversationType is the conversations.conversation_type value.
	ConversationType = "meeting"
	// RawFormat tags message_raw rows holding the composed tool payloads.
	RawFormat = "circleback_json"
)

// MCP tool names (from the Circleback MCP documentation). Kept as constants
// so a server-side rename is a one-line adjustment.
const (
	toolSearchMeetings = "SearchMeetings"
	toolReadMeetings   = "ReadMeetings"
	toolGetTranscripts = "GetTranscriptsForMeetings"
)

// Circleback's MCP tool outputs have no published schema, so every type here
// is defensive: all fields optional, IDs tolerate strings and numbers, and
// common field-name variants are declared side by side with accessor methods
// picking the first populated one. The verbatim payloads are archived in
// message_raw regardless, so nothing is lost to schema drift.

// FlexString decodes a JSON string OR number into a string.
type FlexString string

// UnmarshalJSON implements tolerant decoding.
func (f *FlexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FlexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = FlexString(n.String())
	return nil
}

// Attendee is a meeting participant. Email may be empty (external guests
// sometimes appear name-only).
type Attendee struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// Assignee tolerates the provider's string, object, and null action-item
// assignee shapes while exposing one stable display value to renderers.
type Assignee struct {
	Name        string `json:"name,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
	value       string
	raw         json.RawMessage
}

// UnmarshalJSON implements tolerant action-item assignee decoding.
func (a *Assignee) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	*a = Assignee{}
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		a.raw = append(a.raw[:0], b...)
		return nil
	}
	if b[0] == '"' {
		if err := json.Unmarshal(b, &a.value); err != nil {
			return err
		}
		a.raw = append(a.raw[:0], b...)
		return nil
	}
	type assigneeObject Assignee
	var object assigneeObject
	if err := json.Unmarshal(b, &object); err != nil {
		return err
	}
	*a = Assignee(object)
	a.raw = append(a.raw[:0], b...)
	return nil
}

// MarshalJSON keeps fallback re-encoding compatible with the input shape.
func (a *Assignee) MarshalJSON() ([]byte, error) {
	if a == nil {
		return []byte("null"), nil
	}
	if len(a.raw) > 0 {
		return a.raw, nil
	}
	if a.value != "" {
		return json.Marshal(a.value)
	}
	type assigneeObject Assignee
	return json.Marshal(assigneeObject(*a))
}

// Display returns the first nonblank assignee name or email.
func (a *Assignee) Display() string {
	if a == nil {
		return ""
	}
	for _, value := range []string{a.value, a.Name, a.DisplayName, a.Email} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// ActionItem is one assigned follow-up from a meeting.
type ActionItem struct {
	Title       string   `json:"title,omitempty"`
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Status      string   `json:"status,omitempty"`
	Assignee    Assignee `json:"assignee,omitzero"`
	DueDate     string   `json:"dueDate,omitempty"`
}

// DisplayTitle returns the first populated title variant.
func (a ActionItem) DisplayTitle() string {
	for _, value := range []string{a.Title, a.Name, a.Description} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// AssigneeLabel returns a stable display value across assignee shapes.
func (a ActionItem) AssigneeLabel() string {
	return a.Assignee.Display()
}

// Meeting is one meeting as returned by ReadMeetings/SearchMeetings.
type Meeting struct {
	ID   FlexString `json:"id"`
	Name string     `json:"name,omitempty"`
	// Title is an alternate key for Name.
	Title string `json:"title,omitempty"`

	CreatedAt string `json:"createdAt,omitempty"`
	StartTime string `json:"startTime,omitempty"`
	Date      string `json:"date,omitempty"`
	EndTime   string `json:"endTime,omitempty"`

	// Duration tolerates seconds-as-number and strings.
	Duration        json.Number `json:"duration,omitempty"`
	DurationSeconds json.Number `json:"durationSeconds,omitempty"`

	Attendees []Attendee `json:"attendees,omitempty"`
	Organizer *Attendee  `json:"organizer,omitempty"`

	Notes string `json:"notes,omitempty"`
	// Summary is an alternate key for Notes.
	Summary string `json:"summary,omitempty"`

	ActionItems []ActionItem `json:"actionItems,omitempty"`
	Insights    []Insight    `json:"insights,omitempty"`
	Tags        []string     `json:"tags,omitempty"`

	URL          string `json:"url,omitempty"`
	MeetingURL   string `json:"meetingUrl,omitempty"`
	RecordingURL string `json:"recordingUrl,omitempty"`

	// Raw preserves this meeting's verbatim tool-result JSON.
	Raw json.RawMessage `json:"-"`
}

// Insight is a custom workspace insight attached to a meeting. Shape is
// workspace-defined; keep name/content and tolerate anything else via Raw.
type Insight struct {
	Name    string `json:"name,omitempty"`
	Title   string `json:"title,omitempty"`
	Content string `json:"content,omitempty"`
	Value   string `json:"value,omitempty"`
}

// DisplayTitle returns the first populated insight label.
func (i Insight) DisplayTitle() string {
	for _, value := range []string{i.Title, i.Name} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// DisplayContent returns the first populated insight value.
func (i Insight) DisplayContent() string {
	for _, value := range []string{i.Content, i.Value} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// DisplayName returns the meeting's title from the first populated variant.
func (m *Meeting) DisplayName() string {
	if m.Name != "" {
		return m.Name
	}
	return m.Title
}

// NotesMarkdown returns the meeting notes from the first populated variant.
func (m *Meeting) NotesMarkdown() string {
	if m.Notes != "" {
		return m.Notes
	}
	return m.Summary
}

// PlatformURL returns the meeting link from the first populated variant.
func (m *Meeting) PlatformURL() string {
	if m.URL != "" {
		return m.URL
	}
	return m.MeetingURL
}

// StartedAt parses the meeting start from the first parsable time field.
func (m *Meeting) StartedAt() time.Time {
	if scheduled := m.ScheduledAt(); !scheduled.IsZero() {
		return scheduled
	}
	return parseFlexibleTime(m.CreatedAt)
}

// ScheduledAt parses only provider fields that explicitly describe when the
// meeting happens. CreatedAt is intentionally excluded: creation time is a
// watermark and may be far removed from the scheduled meeting lifecycle.
func (m *Meeting) ScheduledAt() time.Time {
	for _, s := range []string{m.StartTime, m.Date} {
		if t := parseFlexibleTime(s); !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// EndedAt parses the provider's explicit scheduled end time.
func (m *Meeting) EndedAt() time.Time {
	return parseFlexibleTime(m.EndTime)
}

// CreatedTime parses only the provider's creation timestamp for watermark
// bookkeeping. Scheduled time is a separate dimension and may be in the future.
func (m *Meeting) CreatedTime() time.Time {
	return parseFlexibleTime(m.CreatedAt)
}

// DurationSecs returns the meeting duration in seconds, 0 when unknown.
func (m *Meeting) DurationSecs() int64 {
	for _, n := range []json.Number{m.DurationSeconds, m.Duration} {
		if v, err := n.Int64(); err == nil && v > 0 {
			return v
		}
		if v, err := n.Float64(); err == nil && v > 0 {
			return int64(v)
		}
	}
	return 0
}

// TranscriptEntry is one utterance from GetTranscriptsForMeetings.
type TranscriptEntry struct {
	Speaker     string `json:"speaker,omitempty"`
	SpeakerName string `json:"speakerName,omitempty"`
	Text        string `json:"text,omitempty"`
	Content     string `json:"content,omitempty"`
	// Words is a historical alias only when its value is a JSON string.
	Words json.RawMessage `json:"words,omitempty"`

	// Timestamp and offset variants accept either JSON strings or numbers.
	Timestamp      FlexString `json:"timestamp,omitempty"`
	Start          FlexString `json:"start,omitempty"`
	StartTimestamp FlexString `json:"startTimestamp,omitempty"`
	Time           FlexString `json:"time,omitempty"`

	unrecognizedPayload bool
}

// UnmarshalJSON retains enough field-shape information to distinguish an
// explicit blank utterance from a nonblank entry using an unsupported key.
func (e *TranscriptEntry) UnmarshalJSON(b []byte) error {
	type transcriptEntryWire struct {
		Speaker        string          `json:"speaker,omitempty"`
		SpeakerName    string          `json:"speakerName,omitempty"`
		Text           string          `json:"text,omitempty"`
		Content        string          `json:"content,omitempty"`
		Words          json.RawMessage `json:"words,omitempty"`
		Timestamp      FlexString      `json:"timestamp,omitempty"`
		Start          FlexString      `json:"start,omitempty"`
		StartTimestamp FlexString      `json:"startTimestamp,omitempty"`
		Time           FlexString      `json:"time,omitempty"`
	}
	var wire transcriptEntryWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	recognized := false
	for _, key := range []string{"text", "content"} {
		if _, ok := fields[key]; ok {
			recognized = true
		}
	}
	unrecognized := false
	if words, ok := fields["words"]; ok {
		var value string
		if bytes.Equal(bytes.TrimSpace(words), []byte("null")) || json.Unmarshal(words, &value) == nil {
			recognized = true
		} else {
			unrecognized = true
		}
	}
	if !recognized && !unrecognized {
		knownMetadata := map[string]bool{
			"speaker": true, "speakerName": true, "timestamp": true,
			"start": true, "startTimestamp": true, "time": true,
		}
		for key, value := range fields {
			if !knownMetadata[key] && rawJSONNonBlank(value) {
				unrecognized = true
				break
			}
		}
	}
	*e = TranscriptEntry{
		Speaker:             wire.Speaker,
		SpeakerName:         wire.SpeakerName,
		Text:                wire.Text,
		Content:             wire.Content,
		Words:               wire.Words,
		Timestamp:           wire.Timestamp,
		Start:               wire.Start,
		StartTimestamp:      wire.StartTimestamp,
		Time:                wire.Time,
		unrecognizedPayload: unrecognized,
	}
	return nil
}

func rawJSONNonBlank(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) ||
		bytes.Equal(trimmed, []byte(`""`)) || bytes.Equal(trimmed, []byte("[]")) ||
		bytes.Equal(trimmed, []byte("{}")) {
		return false
	}
	if trimmed[0] == '"' {
		var value string
		return json.Unmarshal(trimmed, &value) != nil || strings.TrimSpace(value) != ""
	}
	return true
}

// SpeakerLabel returns the first populated speaker variant.
func (e *TranscriptEntry) SpeakerLabel() string {
	if e.Speaker != "" {
		return e.Speaker
	}
	return e.SpeakerName
}

// Utterance returns the first populated text variant.
func (e *TranscriptEntry) Utterance() string {
	for _, value := range []string{e.Text, e.Content} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	if len(e.Words) > 0 {
		var words string
		if err := json.Unmarshal(e.Words, &words); err == nil {
			return strings.TrimSpace(words)
		}
	}
	return ""
}

func (e *TranscriptEntry) numericOffset() (float64, bool) {
	for _, value := range []FlexString{e.Timestamp, e.Start, e.StartTimestamp, e.Time} {
		trimmed := strings.TrimSpace(string(value))
		if trimmed == "" {
			continue
		}
		seconds, err := strconv.ParseFloat(trimmed, 64)
		if err == nil {
			return seconds, true
		}
	}
	return 0, false
}

func (e *TranscriptEntry) timestampValues() []string {
	return []string{
		string(e.Timestamp),
		string(e.Start),
		string(e.StartTimestamp),
		string(e.Time),
	}
}

// TranscriptClassification distinguishes usable content, an explicit but
// empty recognized shape, and provider schema drift.
type TranscriptClassification string

const (
	TranscriptPresent         TranscriptClassification = "present"
	TranscriptRecognizedEmpty TranscriptClassification = "recognized-empty"
	TranscriptUnrecognized    TranscriptClassification = "unrecognized"
)

// Transcript is one meeting's transcript.
type Transcript struct {
	ID        FlexString        `json:"id"`
	MeetingID FlexString        `json:"meetingId"`
	Entries   []TranscriptEntry `json:"transcript"`
	// Text carries a plain-text transcript when the server returns prose
	// instead of structured entries.
	Text string `json:"text,omitempty"`

	// Raw preserves this transcript's verbatim tool-result JSON.
	Raw json.RawMessage `json:"-"`

	transcriptFieldPresent bool
	textFieldPresent       bool
}

// UnmarshalJSON records recognized raw-field presence so an explicit empty
// transcript remains distinguishable from an unrecognized result object.
func (t *Transcript) UnmarshalJSON(b []byte) error {
	type transcriptWire struct {
		ID        FlexString        `json:"id"`
		MeetingID FlexString        `json:"meetingId"`
		Entries   []TranscriptEntry `json:"transcript"`
		Text      string            `json:"text,omitempty"`
	}
	var wire transcriptWire
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	_, transcriptFieldPresent := fields["transcript"]
	_, textFieldPresent := fields["text"]
	*t = Transcript{
		ID:                     wire.ID,
		MeetingID:              wire.MeetingID,
		Entries:                wire.Entries,
		Text:                   wire.Text,
		transcriptFieldPresent: transcriptFieldPresent,
		textFieldPresent:       textFieldPresent,
	}
	return nil
}

// ResolvedID returns the official id field, falling back to meetingId.
func (t *Transcript) ResolvedID() string {
	if t == nil {
		return ""
	}
	for _, id := range []FlexString{t.ID, t.MeetingID} {
		if value := strings.TrimSpace(string(id)); value != "" {
			return value
		}
	}
	return ""
}

// Classification reports whether the result contains transcript content, is
// explicitly recognized-empty, or has no recognized transcript field.
func (t *Transcript) Classification() TranscriptClassification {
	if t == nil {
		return TranscriptUnrecognized
	}
	if strings.TrimSpace(t.Text) != "" {
		return TranscriptPresent
	}
	for _, entry := range t.Entries {
		if entry.Utterance() != "" {
			return TranscriptPresent
		}
	}
	for _, entry := range t.Entries {
		if entry.unrecognizedPayload {
			return TranscriptUnrecognized
		}
	}
	if t.textFieldPresent || t.transcriptFieldPresent {
		return TranscriptRecognizedEmpty
	}
	return TranscriptUnrecognized
}

// ContentEntries returns only structured entries with a nonblank utterance.
func (t *Transcript) ContentEntries() []TranscriptEntry {
	if t == nil {
		return nil
	}
	entries := make([]TranscriptEntry, 0, len(t.Entries))
	for _, entry := range t.Entries {
		if entry.Utterance() != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

// parseFlexibleTime accepts the timestamp shapes seen in tool outputs.
func parseFlexibleTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
