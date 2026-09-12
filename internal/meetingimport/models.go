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

	maxSourceIdentifierChars  = 128
	maxSourceDisplayNameChars = 256
	maxExternalIDChars        = 256
	maxTitleChars             = 4096
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
	Identifier   string `json:"identifier" maxLength:"128"`
	DisplayName  string `json:"display_name,omitempty" maxLength:"256"`
	AccountEmail string `json:"account_email" format:"email"`
}

type Meeting struct {
	ActionItems        *[]MeetingActionItem `json:"action_items,omitempty"`
	ExternalID         string               `json:"external_id" maxLength:"256"`
	Title              string               `json:"title,omitempty" maxLength:"4096"`
	StartedAt          string               `json:"started_at" format:"date-time"`
	EndedAt            string               `json:"ended_at,omitempty" format:"date-time"`
	SummaryMarkdown    string               `json:"summary_markdown,omitempty"`
	SummaryText        string               `json:"summary_text,omitempty"`
	Transcript         string               `json:"transcript,omitempty"`
	TranscriptSegments []TranscriptSegment  `json:"transcript_segments,omitempty"`
	Organizer          *MeetingPerson       `json:"organizer,omitempty"`
	Attendees          []MeetingPerson      `json:"attendees,omitempty"`
	Metadata           map[string]any       `json:"metadata,omitempty"`
}

// MeetingActionItem is source-reported action evidence. Status and due dates
// retain the source vocabulary; normalized query status is derived by Store.
type MeetingActionItem struct {
	SourceID      string `json:"source_id,omitempty"`
	Title         string `json:"title"`
	Description   string `json:"description,omitempty"`
	AssigneeName  string `json:"assignee_name,omitempty"`
	AssigneeEmail string `json:"assignee_email,omitempty"`
	Status        string `json:"status,omitempty"`
	DueDate       string `json:"due_date,omitempty"`
}

type MeetingPerson struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email" format:"email"`
}

type TranscriptSegment struct {
	Speaker       string   `json:"speaker"`
	Text          string   `json:"text"`
	OffsetSeconds *float64 `json:"offset_seconds,omitempty" minimum:"0"`
}

type NormalizedRequest struct {
	Source  Source
	Meeting NormalizedMeeting
}

type NormalizedMeeting struct {
	ActionItems        *[]MeetingActionItem
	ExternalID         string
	Title              string
	StartedAt          time.Time
	EndedAt            *time.Time
	SummaryMarkdown    string
	SummaryText        string
	Transcript         string
	TranscriptSegments []TranscriptSegment
	Organizer          *MeetingPerson
	Attendees          []MeetingPerson
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
	if err := validateBoundedRequired("source.identifier", source.Identifier, maxSourceIdentifierChars); err != nil {
		return Source{}, err
	}

	source.DisplayName = strings.TrimSpace(source.DisplayName)
	if err := validateBoundedOptional("source.display_name", source.DisplayName, maxSourceDisplayNameChars); err != nil {
		return Source{}, err
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
	if err := validateBoundedRequired("meeting.external_id", meeting.ExternalID, maxExternalIDChars); err != nil {
		return NormalizedMeeting{}, err
	}
	meeting.Title = strings.TrimSpace(meeting.Title)
	if err := validateBoundedOptional("meeting.title", meeting.Title, maxTitleChars); err != nil {
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

	var organizer *MeetingPerson
	if meeting.Organizer != nil {
		normalizedOrganizer, normalizeErr := normalizePerson("meeting.organizer", *meeting.Organizer)
		if normalizeErr != nil {
			return NormalizedMeeting{}, normalizeErr
		}
		organizer = &normalizedOrganizer
	}
	attendees, err := normalizeAttendees(meeting.Attendees)
	if err != nil {
		return NormalizedMeeting{}, err
	}

	actions, err := normalizeActions(meeting.ActionItems)
	if err != nil {
		return NormalizedMeeting{}, err
	}

	return NormalizedMeeting{
		ActionItems:        actions,
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

func normalizeActions(items *[]MeetingActionItem) (*[]MeetingActionItem, error) {
	if items == nil {
		return nil, nil //nolint:nilnil // Omitted source actions stay distinct from an explicitly empty list.
	}
	if len(*items) > 1000 {
		return nil, validationError("meeting.action_items must have at most 1000 items")
	}
	out := make([]MeetingActionItem, len(*items))
	for i, item := range *items {
		field := fmt.Sprintf("meeting.action_items[%d]", i)
		item.Title = strings.TrimSpace(item.Title)
		if err := validateBoundedRequired(field+".title", item.Title, maxTitleChars); err != nil {
			return nil, err
		}
		for _, value := range []struct {
			name, text string
			limit      int
		}{
			{"description", item.Description, 65536},
			{"source_id", item.SourceID, 256},
			{"assignee_name", item.AssigneeName, 256},
			{"due_date", item.DueDate, 256},
			{"status", item.Status, 128},
		} {
			if err := validateBoundedOptional(field+"."+value.name, value.text, value.limit); err != nil {
				return nil, err
			}
		}
		if item.AssigneeEmail != "" {
			email, err := normalizeEmail(field+".assignee_email", item.AssigneeEmail)
			if err != nil {
				return nil, err
			}
			item.AssigneeEmail = email
		}
		out[i] = item
	}
	return &out, nil
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

func normalizePerson(field string, person MeetingPerson) (MeetingPerson, error) {
	email, err := normalizeEmail(field+".email", person.Email)
	if err != nil {
		return MeetingPerson{}, err
	}
	return MeetingPerson{
		Name:  strings.TrimSpace(person.Name),
		Email: email,
	}, nil
}

func normalizeAttendees(attendees []MeetingPerson) ([]MeetingPerson, error) {
	out := make([]MeetingPerson, 0, len(attendees))
	seen := make(map[string]struct{}, len(attendees))
	for idx := range attendees {
		person, err := normalizePerson(
			fmt.Sprintf("meeting.attendees[%d]", idx),
			attendees[idx],
		)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(person.Email)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, person)
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

func validateBoundedRequired(field, value string, maxChars int) error {
	if value == "" {
		return validationError("%s is required", field)
	}
	return validateBoundedOptional(field, value, maxChars)
}

func validateBoundedOptional(field, value string, maxChars int) error {
	if !utf8.ValidString(value) {
		return validationError("%s must be valid UTF-8", field)
	}
	if utf8.RuneCountInString(value) > maxChars {
		return validationError("%s must be at most %d characters", field, maxChars)
	}
	return nil
}

func validationError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, args...))
}
