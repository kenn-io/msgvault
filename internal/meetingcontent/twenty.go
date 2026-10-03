package meetingcontent

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/mail"
	"strings"
	"time"
)

// Twenty evidence retains the returned provider fields rather than a second
// normalized transcript schema. All meeting surfaces decode this envelope.
func decodeTwenty(fields map[string]jsontext.Value) Content {
	version, ok := rawInteger(fields["schema_version"])
	if !ok || version != 1 {
		return unavailableContent("unsupported_schema")
	}
	var recording map[string]jsontext.Value
	if json.Unmarshal(fields["recording"], &recording) != nil || recording == nil {
		return unavailableContent("invalid_recording")
	}
	c := baseRecognizedContent()
	c.Notes = Section{State: StateUnsupported}
	c.Actions, c.ActionCoverage, c.ActionReason = []Action{}, CoverageUnsupported, "no_structured_actions"
	var summary map[string]jsontext.Value
	if raw := recording["summary"]; len(raw) == 0 || isNull(raw) {
		c.Summary = Section{State: StateUnavailable, Reason: reasonMissingField}
	} else if json.Unmarshal(raw, &summary) != nil || summary == nil {
		c.Summary = Section{State: StateUnavailable, Reason: reasonInvalidSection}
	} else {
		c.Summary = decodeStringField(summary, "markdown")
		// Call Recorder stores its failure notice in the summary field; the
		// raw evidence keeps the reason.
		if c.Summary.State == StateAvailable && strings.HasPrefix(c.Summary.Text, "## Summary unavailable") {
			c.Summary = Section{State: StateUnavailable, Reason: "summary_unavailable"}
		}
	}
	c.Transcript = decodeTwentyTranscript(recording["transcript"])
	var participants []struct {
		Name      string `json:"displayName"`
		Handle    string `json:"handle"`
		Organizer bool   `json:"isOrganizer"`
	}
	if json.Unmarshal(fields["participants"], &participants) == nil {
		for _, person := range participants {
			role := "to"
			if person.Organizer {
				role = "from"
			}
			// Calendar names remain display evidence even without an address.
			// Only a valid handle can create an archive identity relationship.
			if participant, ok := participantFromPerson(personWire{Name: person.Name, Email: twentyEmail(person.Handle)}, role); ok {
				c.SourceParticipants = append(c.SourceParticipants, participant)
			}
		}
	}
	start, startOK := rawTime(recording["startedAt"], false)
	end, endOK := rawTime(recording["endedAt"], false)
	if startOK && endOK && end.After(start) {
		setDuration(&c, end.Sub(start).Seconds(), DurationProvider)
		return c
	}
	var calendar map[string]jsontext.Value
	if json.Unmarshal(fields["calendar_event"], &calendar) == nil {
		start, startOK = rawTime(calendar["startsAt"], false)
		end, endOK = rawTime(calendar["endsAt"], false)
		if startOK && endOK && end.After(start) {
			setDuration(&c, end.Sub(start).Seconds(), DurationScheduled)
			return c
		}
	}
	if value, ok := twentyTranscriptSpan(recording["transcript"]); ok && c.Transcript.State == StateAvailable {
		setDuration(&c, value, DurationTranscriptSpan)
	}
	return c
}

// A diarized entry may cover an entire speech turn, so the last word's end
// offset supplies duration even when the transcript has just one segment.
func twentyTranscriptSpan(raw jsontext.Value) (float64, bool) {
	var entries []struct {
		Words []struct {
			Text  string                    `json:"text"`
			Start map[string]jsontext.Value `json:"start_timestamp"`
			End   map[string]jsontext.Value `json:"end_timestamp"`
		} `json:"words"`
	}
	if json.Unmarshal(raw, &entries) != nil {
		return 0, false
	}
	var first, last float64
	found := false
	var absolute []time.Time
	for _, entry := range entries {
		for _, word := range entry.Words {
			if strings.TrimSpace(word.Text) == "" {
				continue
			}
			for _, stamp := range []map[string]jsontext.Value{word.Start, word.End} {
				if instant, ok := rawTime(stamp["absolute"], false); ok {
					absolute = append(absolute, instant)
				}
				offset, ok := rawFiniteFloat(stamp["relative"])
				if !ok || offset < 0 {
					continue
				}
				if !found || offset < first {
					first = offset
				}
				if !found || offset > last {
					last = offset
				}
				found = true
			}
		}
	}
	if found && last > first {
		return last - first, true
	}
	if first, ok := minTime(absolute); ok {
		if last, lastOK := maxTime(absolute); lastOK && last.After(first) {
			return last.Sub(first).Seconds(), true
		}
	}
	return 0, false
}

func twentyEmail(value string) string {
	email := strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Name != "" || parsed.Address != email || strings.ContainsAny(email, " \t\r\n") || !strings.Contains(email, "@") {
		return ""
	}
	return email
}

func decodeTwentyTranscript(raw jsontext.Value) Transcript {
	if len(raw) == 0 || isNull(raw) {
		return Transcript{State: StateUnavailable, Reason: reasonMissingField}
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '{' {
		var marker struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(raw, &marker) == nil {
			switch marker.Status {
			case "PENDING":
				return Transcript{State: StateUnavailable, Reason: "pending"}
			case "FAILED":
				return Transcript{State: StateUnavailable, Reason: "failed"}
			case "EMPTY":
				return Transcript{State: StateEmpty}
			}
		}
		return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
	}
	var entries []struct {
		Participant *struct {
			Name string `json:"name"`
		} `json:"participant"`
		Words []struct {
			Text  string                    `json:"text"`
			Start map[string]jsontext.Value `json:"start_timestamp"`
			End   map[string]jsontext.Value `json:"end_timestamp"`
		} `json:"words"`
	}
	if json.Unmarshal(raw, &entries) != nil {
		return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
	}
	transcript := Transcript{State: StateEmpty}
	for _, entry := range entries {
		var words []string
		segment := Segment{Speaker: "Unknown speaker"}
		if entry.Participant != nil && strings.TrimSpace(entry.Participant.Name) != "" {
			segment.Speaker = strings.TrimSpace(entry.Participant.Name)
		}
		for _, word := range entry.Words {
			text := strings.TrimSpace(word.Text)
			if text == "" {
				continue
			}
			words = append(words, text)
			if value, present := word.Start["relative"]; present && !isNull(value) {
				offset, valid := rawFiniteFloat(value)
				if !valid || offset < 0 {
					return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
				}
				if segment.OffsetSeconds == nil {
					segment.OffsetSeconds = &offset
				}
			}
			if segment.StartedAt == nil {
				if start, ok := rawTime(word.Start["absolute"], false); ok {
					segment.StartedAt = &start
				}
			}
			if end, ok := rawTime(word.End["absolute"], false); ok {
				segment.EndedAt = &end
			}
		}
		if len(words) == 0 {
			continue
		}
		segment.Text = strings.Join(words, " ")
		transcript.Segments = append(transcript.Segments, segment)
	}
	if len(transcript.Segments) > 0 {
		transcript.State = StateAvailable
	}
	return transcript
}
