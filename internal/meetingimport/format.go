package meetingimport

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Snapshot struct {
	SourceIdentifier  string
	SourceDisplayName string
	AccountEmail      string
	SourceMessageID   string
	Title             string
	StartedAt         time.Time
	Body              string
	Snippet           string
	Metadata          []byte
	Raw               []byte
	Organizer         *Person
	Attendees         []Person
}

type canonicalMeeting struct {
	ExternalID         string              `json:"external_id"`
	Title              string              `json:"title,omitempty"`
	StartedAt          string              `json:"started_at"`
	EndedAt            string              `json:"ended_at,omitempty"`
	SummaryMarkdown    string              `json:"summary_markdown,omitempty"`
	SummaryText        string              `json:"summary_text,omitempty"`
	Transcript         string              `json:"transcript,omitempty"`
	TranscriptSegments []TranscriptSegment `json:"transcript_segments,omitempty"`
	Organizer          *Person             `json:"organizer,omitempty"`
	Attendees          []Person            `json:"attendees,omitempty"`
	Metadata           map[string]any      `json:"metadata,omitempty"`
}

type messageMetadata struct {
	Platform               string         `json:"platform"`
	ExternalMeetingID      string         `json:"external_meeting_id"`
	SourceIdentifier       string         `json:"source_identifier"`
	StartedAt              string         `json:"started_at"`
	EndedAt                string         `json:"ended_at,omitempty"`
	DurationSeconds        int64          `json:"duration_seconds"`
	OrganizerEmail         string         `json:"organizer_email,omitempty"`
	AttendeeCount          int            `json:"attendee_count"`
	HasSummary             bool           `json:"has_summary"`
	HasTranscript          bool           `json:"has_transcript"`
	TranscriptSegmentCount int            `json:"transcript_segment_count"`
	ProviderMetadata       map[string]any `json:"provider_metadata,omitempty"`
}

func BuildSnapshot(req NormalizedRequest) (Snapshot, error) {
	meeting := req.Meeting
	title := meeting.Title
	if title == "" {
		title = "Meeting on " + meeting.StartedAt.UTC().Format(time.DateOnly)
	}

	body := buildBody(title, meeting)
	raw, err := json.Marshal(buildCanonicalMeeting(meeting))
	if err != nil {
		return Snapshot{}, fmt.Errorf("marshal canonical meeting: %w", err)
	}
	metadata, err := json.Marshal(buildMessageMetadata(req))
	if err != nil {
		return Snapshot{}, fmt.Errorf("marshal meeting metadata: %w", err)
	}

	return Snapshot{
		SourceIdentifier:  req.Source.Identifier,
		SourceDisplayName: req.Source.DisplayName,
		AccountEmail:      req.Source.AccountEmail,
		SourceMessageID:   "meeting:" + meeting.ExternalID,
		Title:             title,
		StartedAt:         meeting.StartedAt.UTC(),
		Body:              body,
		Snippet:           snippet(body),
		Metadata:          metadata,
		Raw:               raw,
		Organizer:         meeting.Organizer,
		Attendees:         meeting.Attendees,
	}, nil
}

func buildBody(title string, meeting NormalizedMeeting) string {
	var b strings.Builder
	writeLine := func(line string) {
		if line == "" {
			return
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}

	writeLine(title)
	writeLine(formatWhen(meeting.StartedAt, meeting.EndedAt))

	names := make([]string, 0, len(meeting.Attendees))
	for _, attendee := range meeting.Attendees {
		if attendee.Name != "" {
			names = append(names, attendee.Name)
		}
	}
	if len(names) > 0 {
		writeLine("Attendees: " + strings.Join(names, ", "))
	}

	summary := meeting.SummaryMarkdown
	if summary == "" {
		summary = meeting.SummaryText
	}
	if summary != "" {
		b.WriteByte('\n')
		writeLine(summary)
	}

	if meeting.Transcript != "" {
		b.WriteString("\nTranscript:\n")
		writeLine(meeting.Transcript)
	} else if len(meeting.TranscriptSegments) > 0 {
		b.WriteString("\nTranscript:\n")
		for _, segment := range meeting.TranscriptSegments {
			writeLine(formatSegment(segment))
		}
	}
	return strings.TrimSpace(b.String())
}

func formatWhen(start time.Time, end *time.Time) string {
	line := "When: " + start.UTC().Format("2006-01-02 15:04")
	if end != nil {
		line += " - " + end.UTC().Format("15:04")
	}
	return line
}

func formatSegment(segment TranscriptSegment) string {
	label := segment.Speaker + ": " + segment.Text
	if segment.OffsetSeconds == nil {
		return label
	}
	total := int(*segment.OffsetSeconds)
	hours := total / 3600
	minutes := (total % 3600) / 60
	seconds := total % 60
	if hours > 0 {
		return fmt.Sprintf("[%d:%02d:%02d] %s", hours, minutes, seconds, label)
	}
	return fmt.Sprintf("[%02d:%02d] %s", minutes, seconds, label)
}

func snippet(body string) string {
	const maxRunes = 200
	runes := []rune(strings.TrimSpace(body))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes])
}

func buildCanonicalMeeting(meeting NormalizedMeeting) canonicalMeeting {
	endedAt := ""
	if meeting.EndedAt != nil {
		endedAt = meeting.EndedAt.UTC().Format(time.RFC3339Nano)
	}
	return canonicalMeeting{
		ExternalID:         meeting.ExternalID,
		Title:              meeting.Title,
		StartedAt:          meeting.StartedAt.UTC().Format(time.RFC3339Nano),
		EndedAt:            endedAt,
		SummaryMarkdown:    meeting.SummaryMarkdown,
		SummaryText:        meeting.SummaryText,
		Transcript:         meeting.Transcript,
		TranscriptSegments: meeting.TranscriptSegments,
		Organizer:          meeting.Organizer,
		Attendees:          meeting.Attendees,
		Metadata:           meeting.Metadata,
	}
}

func buildMessageMetadata(req NormalizedRequest) messageMetadata {
	meeting := req.Meeting
	metadata := messageMetadata{
		Platform:               SourceType,
		ExternalMeetingID:      meeting.ExternalID,
		SourceIdentifier:       req.Source.Identifier,
		StartedAt:              meeting.StartedAt.UTC().Format(time.RFC3339Nano),
		AttendeeCount:          len(meeting.Attendees),
		HasSummary:             meeting.SummaryMarkdown != "" || meeting.SummaryText != "",
		HasTranscript:          meeting.Transcript != "" || len(meeting.TranscriptSegments) > 0,
		TranscriptSegmentCount: len(meeting.TranscriptSegments),
		ProviderMetadata:       meeting.Metadata,
	}
	if meeting.EndedAt != nil {
		metadata.EndedAt = meeting.EndedAt.UTC().Format(time.RFC3339Nano)
		metadata.DurationSeconds = int64(meeting.EndedAt.Sub(meeting.StartedAt).Seconds())
	}
	if meeting.Organizer != nil {
		metadata.OrganizerEmail = meeting.Organizer.Email
	}
	return metadata
}
