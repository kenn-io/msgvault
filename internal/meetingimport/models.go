// Package meetingimport validates and stores provider-neutral meeting
// transcripts submitted through the msgvault HTTP API.
package meetingimport

import (
	"errors"
	"fmt"
	"math"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	SourceType       = "meeting_import"
	ConversationType = "meeting"
	MessageType      = "meeting_transcript"
	RawFormat        = "meeting_json"
	MaxRequestBytes  = int64(16 << 20)

	maxSourceIdentifierBytes  = 128
	maxSourceDisplayNameBytes = 256
	maxExternalIDBytes        = 256
	maxTitleBytes             = 4096
)

var (
	ErrMalformedRequest = errors.New("malformed meeting import request")
	ErrRequestTooLarge  = errors.New("meeting import request too large")
	ErrValidation       = errors.New("meeting import validation failed")
)

type MeetingImportRequest struct {
	Source  Source  `json:"source"`
	Meeting Meeting `json:"meeting"`
}

// Request is kept as a concise internal alias for the meeting import wire
// contract. The named type gives generated API clients an unambiguous schema.
type Request = MeetingImportRequest

type Source struct {
	Identifier   string `json:"identifier"`
	DisplayName  string `json:"display_name,omitempty"`
	AccountEmail string `json:"account_email"`
}

type Meeting struct {
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

type Person struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type TranscriptSegment struct {
	Speaker       string   `json:"speaker"`
	Text          string   `json:"text"`
	OffsetSeconds *float64 `json:"offset_seconds,omitempty"`
}

type NormalizedRequest struct {
	Source  Source
	Meeting NormalizedMeeting
}

type NormalizedMeeting struct {
	ExternalID         string
	Title              string
	StartedAt          time.Time
	EndedAt            *time.Time
	SummaryMarkdown    string
	SummaryText        string
	Transcript         string
	TranscriptSegments []TranscriptSegment
	Organizer          *Person
	Attendees          []Person
	Metadata           map[string]any
}

func (r Request) Normalize() (NormalizedRequest, error) {
	source, err := normalizeSource(r.Source)
	if err != nil {
		return NormalizedRequest{}, err
	}
	meeting, err := normalizeMeeting(r.Meeting)
	if err != nil {
		return NormalizedRequest{}, err
	}
	return NormalizedRequest{Source: source, Meeting: meeting}, nil
}

func normalizeSource(source Source) (Source, error) {
	source.Identifier = strings.TrimSpace(source.Identifier)
	if err := validateBoundedRequired("source.identifier", source.Identifier, maxSourceIdentifierBytes); err != nil {
		return Source{}, err
	}

	source.DisplayName = strings.TrimSpace(source.DisplayName)
	if err := validateBoundedOptional("source.display_name", source.DisplayName, maxSourceDisplayNameBytes); err != nil {
		return Source{}, err
	}
	if source.DisplayName == "" {
		source.DisplayName = source.Identifier
	}

	accountEmail, err := normalizeEmail("source.account_email", source.AccountEmail)
	if err != nil {
		return Source{}, err
	}
	source.AccountEmail = accountEmail
	return source, nil
}

func normalizeMeeting(meeting Meeting) (NormalizedMeeting, error) {
	meeting.ExternalID = strings.TrimSpace(meeting.ExternalID)
	if err := validateBoundedRequired("meeting.external_id", meeting.ExternalID, maxExternalIDBytes); err != nil {
		return NormalizedMeeting{}, err
	}
	meeting.Title = strings.TrimSpace(meeting.Title)
	if err := validateBoundedOptional("meeting.title", meeting.Title, maxTitleBytes); err != nil {
		return NormalizedMeeting{}, err
	}

	startedAt, err := parseTimestamp("meeting.started_at", meeting.StartedAt)
	if err != nil {
		return NormalizedMeeting{}, err
	}
	var endedAt *time.Time
	if strings.TrimSpace(meeting.EndedAt) != "" {
		parsed, parseErr := parseTimestamp("meeting.ended_at", meeting.EndedAt)
		if parseErr != nil {
			return NormalizedMeeting{}, parseErr
		}
		if parsed.Before(startedAt) {
			return NormalizedMeeting{}, validationError("meeting.ended_at must not be before meeting.started_at")
		}
		endedAt = &parsed
	}

	summaryMarkdown := strings.TrimSpace(meeting.SummaryMarkdown)
	summaryText := strings.TrimSpace(meeting.SummaryText)
	transcript := strings.TrimSpace(meeting.Transcript)
	segments, err := normalizeSegments(meeting.TranscriptSegments)
	if err != nil {
		return NormalizedMeeting{}, err
	}
	if transcript != "" && len(segments) > 0 {
		return NormalizedMeeting{}, validationError("meeting.transcript and meeting.transcript_segments are mutually exclusive")
	}
	if summaryMarkdown == "" && summaryText == "" && transcript == "" && len(segments) == 0 {
		return NormalizedMeeting{}, validationError("meeting requires a summary or transcript")
	}

	organizer, err := normalizeOptionalPerson("meeting.organizer", meeting.Organizer)
	if err != nil {
		return NormalizedMeeting{}, err
	}
	attendees, err := normalizeAttendees(meeting.Attendees)
	if err != nil {
		return NormalizedMeeting{}, err
	}

	return NormalizedMeeting{
		ExternalID:         meeting.ExternalID,
		Title:              meeting.Title,
		StartedAt:          startedAt,
		EndedAt:            endedAt,
		SummaryMarkdown:    summaryMarkdown,
		SummaryText:        summaryText,
		Transcript:         transcript,
		TranscriptSegments: segments,
		Organizer:          organizer,
		Attendees:          attendees,
		Metadata:           meeting.Metadata,
	}, nil
}

func normalizeSegments(segments []TranscriptSegment) ([]TranscriptSegment, error) {
	if len(segments) == 0 {
		return nil, nil
	}
	out := make([]TranscriptSegment, len(segments))
	var previousOffset float64
	havePreviousOffset := false
	for idx, segment := range segments {
		segment.Speaker = strings.TrimSpace(segment.Speaker)
		if segment.Speaker == "" {
			return nil, validationError("meeting.transcript_segments[%d].speaker is required", idx)
		}
		segment.Text = strings.TrimSpace(segment.Text)
		if segment.Text == "" {
			return nil, validationError("meeting.transcript_segments[%d].text is required", idx)
		}
		if segment.OffsetSeconds != nil {
			offset := *segment.OffsetSeconds
			if math.IsNaN(offset) || math.IsInf(offset, 0) || offset < 0 {
				return nil, validationError("meeting.transcript_segments[%d].offset_seconds must be finite and non-negative", idx)
			}
			if havePreviousOffset && offset < previousOffset {
				return nil, validationError("meeting.transcript_segments offsets must be non-decreasing")
			}
			previousOffset = offset
			havePreviousOffset = true
		}
		out[idx] = segment
	}
	return out, nil
}

func normalizeOptionalPerson(field string, person *Person) (*Person, error) {
	if person == nil {
		return nil, nil
	}
	email, err := normalizeEmail(field+".email", person.Email)
	if err != nil {
		return nil, err
	}
	return &Person{
		Name:  strings.TrimSpace(person.Name),
		Email: email,
	}, nil
}

func normalizeAttendees(attendees []Person) ([]Person, error) {
	out := make([]Person, 0, len(attendees))
	seen := make(map[string]struct{}, len(attendees))
	for idx := range attendees {
		person, err := normalizeOptionalPerson(
			fmt.Sprintf("meeting.attendees[%d]", idx),
			&attendees[idx],
		)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(person.Email)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, *person)
	}
	return out, nil
}

func normalizeEmail(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", validationError("%s is required", field)
	}
	if !utf8.ValidString(value) {
		return "", validationError("%s must be valid UTF-8", field)
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || !strings.EqualFold(parsed.Address, value) {
		return "", validationError("%s must be one email address without a display name", field)
	}
	return strings.ToLower(parsed.Address), nil
}

func parseTimestamp(field, value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, validationError("%s is required", field)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, validationError("%s must be an RFC3339 timestamp with an explicit offset", field)
	}
	return parsed.UTC(), nil
}

func validateBoundedRequired(field, value string, maxBytes int) error {
	if value == "" {
		return validationError("%s is required", field)
	}
	return validateBoundedOptional(field, value, maxBytes)
}

func validateBoundedOptional(field, value string, maxBytes int) error {
	if !utf8.ValidString(value) {
		return validationError("%s must be valid UTF-8", field)
	}
	if len(value) > maxBytes {
		return validationError("%s must be at most %d UTF-8 bytes", field, maxBytes)
	}
	return nil
}

func validationError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, args...))
}
