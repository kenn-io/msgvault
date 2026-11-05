package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/hex/evidence"
)

func decodeHex(raw []byte) Content {
	envelope, err := evidence.Decode(raw)
	if err != nil {
		// Preserve invalid_raw for malformed/non-object input without building
		// a raw field map on the successful typed decode path.
		var fields map[string]jsontext.Value
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return unavailableContent("invalid_raw")
		}
	}
	if errors.Is(err, evidence.ErrUnsupportedVersion) {
		return unavailableContent("unsupported_version")
	}
	if err != nil {
		return unavailableContent(reasonInvalidSection)
	}
	m := envelope.Meeting
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
	if m.TranscriptState == "missing" {
		content.Transcript.State = StateUnavailable
	}
	if m.TranscriptState != "final" {
		content.Transcript.Reason = "hex_" + m.TranscriptState
	}
	if m.TranscriptPartial {
		content.Transcript.Reason = "hex_partial"
		if len(lines) == 0 {
			content.Transcript.State = StateUnavailable
		}
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
		var start, end int64
		for i, entry := range m.Entries {
			if i == 0 || entry.StartMS < start {
				start = entry.StartMS
			}
			if entry.EndMS > end {
				end = entry.EndMS
			}
		}
		setDuration(&content, float64(end-start)/1000, DurationTranscriptSpan)
	}

	return content
}
