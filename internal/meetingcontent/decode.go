package meetingcontent

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math"
	"strconv"
	"strings"
	"time"
)

const maxRawBytes = 64 << 20

const reasonMissingField = "missing_field"
const reasonInvalidSection = "invalid_section"

func Decode(rawFormat string, raw, _ []byte) Content {
	if len(bytes.TrimSpace(raw)) == 0 {
		return unavailableContent("missing_raw")
	}
	if len(raw) > maxRawBytes {
		return unavailableContent("raw_too_large")
	}
	fields, ok := decodeFields(raw)
	if !ok {
		return unavailableContent("invalid_raw")
	}
	switch rawFormat {
	case "omi_json":
		return decodeOmi(fields)
	case "granola_json":
		return decodeGranola(fields)
	case "circleback_json":
		return decodeCircleback(fields)
	case "plaud_json":
		return decodePlaud(fields)
	case "notion_meeting_json":
		return decodeNotion(fields)
	case "muesli_json":
		return decodeMuesli(fields)
	case "pocket_json":
		return decodePocket(fields)
	case "twilio_call_json":
		return decodeTwilio(fields)
	case "twenty_json":
		return decodeTwenty(fields)
	case "meeting_json":
		return decodeGeneric(fields)
	default:
		return unavailableContent("unsupported_format")
	}
}

func ProjectionContent(content Content) Content {
	projection := content
	projection.Transcript.Text = ""
	projection.Transcript.Segments = nil
	projection.Actions = append([]Action{}, content.Actions...)
	projection.SourceParticipants = append([]Participant(nil), content.SourceParticipants...)
	return projection
}

func NormalizeStatus(source string) Status {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "pending", "open", "todo", "to_do", "incomplete", "in_progress":
		return StatusPending
	case "completed", "complete", "done":
		return StatusCompleted
	case "cancelled", "canceled":
		return StatusCancelled
	default:
		return StatusUnknown
	}
}

func unavailableContent(reason string) Content {
	return Content{
		Summary:        Section{State: StateUnavailable, Reason: reason},
		Notes:          Section{State: StateUnavailable, Reason: reason},
		Transcript:     Transcript{State: StateUnavailable, Reason: reason},
		Actions:        []Action{},
		ActionCoverage: CoverageUnavailable,
		ActionReason:   reason,
	}
}

func baseRecognizedContent() Content {
	return Content{Actions: []Action{}}
}

func decodeStringAliases(fields map[string]jsontext.Value, keys ...string) Section {
	present := false
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		present = true
		value, valid := optionalString(raw)
		if !valid {
			return Section{State: StateUnavailable, Reason: reasonInvalidSection}
		}
		if value = strings.TrimSpace(value); value != "" {
			return Section{State: StateAvailable, Text: value}
		}
	}
	if present {
		return Section{State: StateEmpty}
	}
	return Section{State: StateUnavailable, Reason: reasonMissingField}
}

func decodeStringField(fields map[string]jsontext.Value, key string) Section {
	raw, ok := fields[key]
	if !ok {
		return Section{State: StateUnavailable, Reason: reasonMissingField}
	}
	value, valid := optionalString(raw)
	if !valid {
		return Section{State: StateUnavailable, Reason: reasonInvalidSection}
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return Section{State: StateEmpty}
	}
	return Section{State: StateAvailable, Text: value}
}

func providerParticipants(fields map[string]jsontext.Value) []Participant {
	var organizer personWire
	var organizerPtr *personWire
	if raw, ok := fields["organizer"]; ok && !isNull(raw) && json.Unmarshal(raw, &organizer) == nil {
		organizerPtr = &organizer
	}
	var attendees []personWire
	_ = json.Unmarshal(fields["attendees"], &attendees)
	return participantsFromPeople(organizerPtr, attendees)
}

type personWire struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Phone string `json:"phone"`
}

func participantsFromPeople(organizer *personWire, attendees []personWire) []Participant {
	participants := make([]Participant, 0, len(attendees)+1)
	if organizer != nil {
		if participant, ok := participantFromPerson(*organizer, "from"); ok {
			participants = append(participants, participant)
		}
	}
	for _, attendee := range attendees {
		if participant, ok := participantFromPerson(attendee, "to"); ok {
			participants = append(participants, participant)
		}
	}
	return participants
}

func actionResult(actions []Action, invalid bool) ([]Action, Coverage, string) {
	if !invalid {
		return actions, CoverageAvailable, ""
	}
	if len(actions) > 0 {
		return actions, CoveragePartial, reasonInvalidSection
	}
	return actions, CoverageUnavailable, reasonInvalidSection
}

func transcriptDuration(segments []Segment) (float64, bool) {
	var offsets []float64
	var times []time.Time
	for _, segment := range segments {
		if segment.OffsetSeconds != nil && finite(*segment.OffsetSeconds) && *segment.OffsetSeconds >= 0 {
			offsets = append(offsets, *segment.OffsetSeconds)
		}
		if segment.StartedAt != nil {
			times = append(times, *segment.StartedAt)
		}
	}
	if len(offsets) >= 2 {
		minimum, maximum := offsets[0], offsets[0]
		for _, value := range offsets[1:] {
			minimum = min(minimum, value)
			maximum = max(maximum, value)
		}
		if maximum > minimum {
			return maximum - minimum, true
		}
	}
	if minimum, ok := minTime(times); ok {
		if maximum, maxOK := maxTime(times); maxOK && maximum.After(minimum) {
			return maximum.Sub(minimum).Seconds(), true
		}
	}
	return 0, false
}

func setDuration(content *Content, seconds float64, basis DurationBasis) {
	if !finite(seconds) || seconds <= 0 {
		return
	}
	content.DurationSeconds = &seconds
	content.DurationBasis = basis
}

func rawPositiveFloat(raw jsontext.Value) (float64, bool) {
	value, ok := rawFiniteFloat(raw)
	return value, ok && value > 0
}

func rawFiniteFloat(raw jsontext.Value) (float64, bool) {
	if len(raw) == 0 || isNull(raw) {
		return 0, false
	}
	trimmed := bytes.TrimSpace(raw)
	var text string
	if len(trimmed) > 0 && trimmed[0] == '"' {
		if json.Unmarshal(trimmed, &text) != nil {
			return 0, false
		}
	} else {
		text = string(trimmed)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	return value, err == nil && finite(value)
}

func rawInteger(raw jsontext.Value) (int, bool) {
	value, ok := rawFiniteFloat(raw)
	if !ok || value != math.Trunc(value) {
		return 0, false
	}
	return int(value), true
}

func rawTime(raw jsontext.Value, flexible bool) (time.Time, bool) {
	value, valid := optionalString(raw)
	if !valid || strings.TrimSpace(value) == "" {
		return time.Time{}, false
	}
	if parsed, ok := parseTimeString(value); ok {
		return parsed, true
	}
	if flexible {
		for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", time.DateOnly} {
			if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
				return parsed.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

func parseTimeString(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func minTime(values []time.Time) (time.Time, bool) {
	var result time.Time
	for _, value := range values {
		if value.IsZero() {
			continue
		}
		if result.IsZero() || value.Before(result) {
			result = value
		}
	}
	return result, !result.IsZero()
}

func maxTime(values []time.Time) (time.Time, bool) {
	var result time.Time
	for _, value := range values {
		if value.After(result) {
			result = value
		}
	}
	return result, !result.IsZero()
}

func optionalString(raw jsontext.Value) (string, bool) {
	if len(raw) == 0 || isNull(raw) {
		return "", len(raw) == 0
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func firstPresent(fields map[string]jsontext.Value, keys ...string) (jsontext.Value, bool) {
	for _, key := range keys {
		if raw, ok := fields[key]; ok {
			return raw, true
		}
	}
	return nil, false
}

func normalizeExplicitEmail(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func isNull(raw jsontext.Value) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func participantFromPerson(person personWire, role string) (Participant, bool) {
	participant := Participant{
		Name: strings.TrimSpace(person.Name), Email: normalizeExplicitEmail(person.Email),
		Phone: strings.TrimSpace(person.Phone), Role: role,
	}
	return participant, participant.Name != "" || participant.Email != "" || participant.Phone != ""
}

// ExplicitParticipants returns only structured organizer/attendee identities
// whose raw contract records participation. Display fallbacks are excluded.
// Unusable or ambiguous source data yields no positive evidence.
func ExplicitParticipants(rawFormat string, raw []byte) []Participant {
	fields, ok := decodeFields(raw)
	if !ok {
		return nil
	}
	var organizer *personWire
	var attendees []personWire
	switch rawFormat {
	case "circleback_json":
		var valid bool
		fields, valid = decodeFields(fields["meeting"])
		if !valid {
			return nil
		}
		fallthrough
	case "meeting_json":
		var valid bool
		organizer, attendees, valid = explicitPeople(fields["organizer"], fields["attendees"])
		if !valid {
			return nil
		}
	case "granola_json":
		var calendar struct {
			Organiser string `json:"organiser"`
		}
		if value := fields["calendar_event"]; len(value) > 0 && !isNull(value) {
			if json.Unmarshal(value, &calendar) != nil {
				return nil
			}
		}
		if strings.TrimSpace(calendar.Organiser) != "" {
			organizer = &personWire{Email: calendar.Organiser}
		}
		_, people, valid := explicitPeople(nil, fields["attendees"])
		if !valid {
			return nil
		}
		attendees = people
	default:
		return nil
	}
	return participantsFromPeople(organizer, attendees)
}

func decodeFields(raw []byte) (map[string]jsontext.Value, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || len(raw) > maxRawBytes {
		return nil, false
	}
	var fields map[string]jsontext.Value
	err := json.Unmarshal(raw, &fields)
	return fields, err == nil && fields != nil
}

func explicitPeople(organizerRaw, attendeesRaw jsontext.Value) (*personWire, []personWire, bool) {
	var organizer *personWire
	if len(organizerRaw) > 0 && !isNull(organizerRaw) {
		organizer = &personWire{}
		if json.Unmarshal(organizerRaw, organizer) != nil || !hasPersonIdentity(*organizer) {
			return nil, nil, false
		}
	}
	var attendees []personWire
	if len(attendeesRaw) > 0 && !isNull(attendeesRaw) {
		var people []*personWire
		if json.Unmarshal(attendeesRaw, &people) != nil {
			return nil, nil, false
		}
		for _, person := range people {
			if person == nil {
				return nil, nil, false
			}
			// Producers omit addressless guests from archived recipients.
			if hasPersonIdentity(*person) {
				attendees = append(attendees, *person)
			}
		}
	}
	return organizer, attendees, true
}
func hasPersonIdentity(person personWire) bool {
	return strings.TrimSpace(person.Email) != "" || strings.TrimSpace(person.Phone) != ""
}
