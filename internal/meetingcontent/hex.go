package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/hex/evidence"
)

func decodeHex(fields map[string]jsontext.Value) Content {
	var version int
	if json.Unmarshal(fields["version"], &version) != nil || version != evidence.Version {
		return unavailableContent("unsupported_version")
	}
	var m evidence.Meeting
	if json.Unmarshal(fields["meeting"], &m) != nil || m.Validate() != nil {
		return unavailableContent(reasonInvalidSection)
	}
	content := baseRecognizedContent()
	content.Summary = Section{State: StateEmpty}
	content.Notes = Section{State: StateEmpty}
	content.Transcript = Transcript{State: StateEmpty}
	if m.Manifest.Notes != nil && strings.TrimSpace(string(m.Manifest.Notes.Summary)) != "" {
		content.Summary = Section{State: StateAvailable, Text: string(m.Manifest.Notes.Summary)}
	}
	var notes []string
	if m.Manifest.Notes != nil && m.Manifest.Notes.Title != "" {
		notes = append(notes, "Notes title: "+m.Manifest.Notes.Title)
	}
	if m.Manifest.ProvisionalTitle != "" {
		notes = append(notes, "Provisional title: "+m.Manifest.ProvisionalTitle)
	}
	if m.Manifest.Calendar != nil {
		notes = append(notes, "Calendar: "+m.Manifest.Calendar.Title)
		people := []personWire{}
		for _, value := range m.Manifest.Calendar.Attendees {
			person := personWire{Name: value}
			if email := evidence.AttendeeEmail(value); email != "" {
				person = personWire{Email: email}
			}
			people = append(people, person)
		}
		content.SourceParticipants = participantsFromPeople(nil, people)
	}
	for _, chapter := range m.Manifest.Chapters {
		notes = append(notes, FormatTranscriptLine(time.Duration(chapter.StartMS)*time.Millisecond, chapter.Title, chapter.Summary))
	}
	if len(notes) > 0 {
		content.Notes = Section{State: StateAvailable, Text: strings.Join(notes, "\n")}
	}
	var lines []string
	for _, entry := range m.Entries {
		offset := float64(entry.StartMS) / 1000
		end := time.UnixMilli(m.Manifest.CreatedAtMS + entry.EndMS).UTC()
		content.Transcript.Segments = append(content.Transcript.Segments, Segment{Speaker: entry.Speaker(), Text: entry.Text, OffsetSeconds: &offset, EndedAt: &end})
		lines = append(lines, FormatTranscriptLine(time.Duration(entry.StartMS)*time.Millisecond, entry.Speaker(), entry.Text))
	}
	if len(lines) > 0 {
		content.Transcript.State = StateAvailable
		content.Transcript.Text = strings.Join(lines, "\n")
	}
	if m.TranscriptState == "missing" || m.TranscriptState == "unreadable" {
		content.Transcript.State = StateUnavailable
	}
	if m.TranscriptState != "final" {
		content.Transcript.Reason = "hex_" + m.TranscriptState
	}
	content.ActionCoverage = CoverageUnsupported
	content.ActionReason = "no_structured_actions"
	if m.Manifest.DurationMS != nil {
		seconds := float64(*m.Manifest.DurationMS) / 1000
		content.DurationSeconds = &seconds
		content.DurationBasis = DurationProvider
	} else if m.Manifest.EndedAtMS != nil && *m.Manifest.EndedAtMS > m.Manifest.CreatedAtMS {
		setDuration(&content, float64(*m.Manifest.EndedAtMS-m.Manifest.CreatedAtMS)/1000, DurationProvider)
	} else {
		var end int64
		for _, entry := range m.Entries {
			if entry.EndMS > end {
				end = entry.EndMS
			}
		}
		setDuration(&content, float64(end)/1000, DurationTranscriptSpan)
	}

	return content
}
