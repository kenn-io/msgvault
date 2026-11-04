package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

func decodeHex(fields map[string]jsontext.Value) Content {
	var m hexMeeting
	if json.Unmarshal(fields["meeting"], &m) != nil || m.Manifest.SchemaVersion != 1 {
		return unavailableContent(reasonInvalidSection)
	}
	content := baseRecognizedContent()
	content.Summary = Section{State: StateEmpty}
	content.Notes = Section{State: StateEmpty}
	content.Transcript = Transcript{State: StateEmpty}
	if m.Manifest.Notes != nil && strings.TrimSpace(m.Manifest.Notes.Summary) != "" {
		content.Summary = Section{State: StateAvailable, Text: m.Manifest.Notes.Summary}
	}
	var notes []string
	if m.Manifest.Calendar != nil {
		notes = append(notes, "Calendar: "+m.Manifest.Calendar.Title)
		people := []personWire{}
		for _, value := range m.Manifest.Calendar.Attendees {
			person := personWire{Name: value}
			if email := hexAttendeeEmail(value); email != "" {
				person = personWire{Email: email}
			}
			people = append(people, person)
		}
		content.SourceParticipants = participantsFromPeople(nil, people)
	}
	for _, chapter := range m.Manifest.Chapters {
		notes = append(notes, hexTranscriptLine(time.Duration(chapter.StartMS)*time.Millisecond, chapter.Title, chapter.Summary))
	}
	if len(notes) > 0 {
		content.Notes = Section{State: StateAvailable, Text: strings.Join(notes, "\n")}
	}
	var lines []string
	for _, entry := range m.Entries {
		offset := float64(entry.StartMS) / 1000
		content.Transcript.Segments = append(content.Transcript.Segments, Segment{Speaker: entry.Speaker(), Text: entry.Text, OffsetSeconds: &offset})
		lines = append(lines, hexTranscriptLine(time.Duration(entry.StartMS)*time.Millisecond, entry.Speaker(), entry.Text))
	}
	if len(lines) > 0 {
		content.Transcript.State = StateAvailable
		content.Transcript.Text = strings.Join(lines, "\n")
	}
	if m.TranscriptState != "final" {
		content.Transcript.Reason = "hex_" + m.TranscriptState
	}
	content.ActionCoverage = CoverageUnsupported
	content.ActionReason = "no_structured_actions"
	if m.Manifest.DurationMS != nil {
		setDuration(&content, float64(*m.Manifest.DurationMS)/1000, DurationProvider)
	} else if m.Manifest.EndedAtMS != nil {
		setDuration(&content, float64(*m.Manifest.EndedAtMS-m.Manifest.CreatedAtMS)/1000, DurationProvider)
	}
	return content
}

// The evidence wire is decoded independently of the importer: Store consumes
// this package, so importing a provider would introduce an archive cycle.
type hexMeeting struct {
	Manifest struct {
		SchemaVersion int `json:"schema_version"`
		Notes         *struct {
			Summary string `json:"summary"`
		} `json:"notes"`
		Calendar *struct {
			Title     string   `json:"title"`
			Attendees []string `json:"attendees"`
		} `json:"calendar"`
		Chapters []struct {
			StartMS int64  `json:"start_ms"`
			Title   string `json:"title"`
			Summary string `json:"summary"`
		} `json:"chapters"`
		DurationMS  *int64 `json:"duration_ms"`
		EndedAtMS   *int64 `json:"ended_at_ms"`
		CreatedAtMS int64  `json:"created_at_ms"`
	} `json:"manifest"`
	Entries         []hexEntry `json:"entries"`
	TranscriptState string     `json:"transcript_state"`
}
type hexEntry struct {
	Source  string `json:"source"`
	StartMS int64  `json:"start_ms"`
	Text    string `json:"text"`
}

func (e hexEntry) Speaker() string {
	if e.Source == "microphone" {
		return "You"
	}
	return "Them"
}
func hexAttendeeEmail(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value || !strings.Contains(value, "@") {
		return ""
	}
	return strings.ToLower(value)
}
func hexTranscriptLine(offset time.Duration, speaker, text string) string {
	total := int64(offset / time.Second)
	stamp := fmt.Sprintf("[%02d:%02d]", total/60, total%60)
	if total >= 3600 {
		stamp = fmt.Sprintf("[%d:%02d:%02d]", total/3600, total/60%60, total%60)
	}
	return stamp + " " + speaker + ": " + text
}
