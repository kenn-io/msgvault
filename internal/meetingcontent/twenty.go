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
	entries, ok := twentyEntries(raw)
	if !ok {
		return 0, false
	}
	var first, last float64
	found := false
	var absolute []time.Time
	for _, entry := range entries {
		for _, word := range entry.words {
			for _, stamp := range []map[string]jsontext.Value{word.start, word.end} {
				if instant, ok := rawTime(stamp["absolute"], false); ok {
					absolute = append(absolute, instant)
				}
				offset, ok := twentyOffset(stamp)
				if !ok {
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

type twentyEntry struct {
	speaker string
	words   []twentyWord
}

type twentyWord struct {
	text       string
	start, end map[string]jsontext.Value
}

// twentyEntries reads a diarized transcript the way Twenty's own parser
// does: a malformed entry or word drops only itself, and an entry needs at
// least one word with nonblank text. ok is false when raw isn't an array.
func twentyEntries(raw jsontext.Value) ([]twentyEntry, bool) {
	var values []jsontext.Value
	if json.Unmarshal(raw, &values) != nil {
		return nil, false
	}
	entries := make([]twentyEntry, 0, len(values))
	for _, value := range values {
		var fields struct {
			Participant jsontext.Value   `json:"participant"`
			Words       []jsontext.Value `json:"words"`
		}
		if json.Unmarshal(value, &fields) != nil {
			continue
		}
		entry := twentyEntry{words: twentyWords(fields.Words)}
		if len(entry.words) == 0 {
			continue
		}
		var participant struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(fields.Participant, &participant) == nil {
			entry.speaker = strings.TrimSpace(participant.Name)
		}
		entries = append(entries, entry)
	}
	return entries, true
}

func twentyWords(values []jsontext.Value) []twentyWord {
	words := make([]twentyWord, 0, len(values))
	for _, value := range values {
		var fields map[string]jsontext.Value
		if json.Unmarshal(value, &fields) != nil {
			continue
		}
		var text string
		if json.Unmarshal(fields["text"], &text) != nil || strings.TrimSpace(text) == "" {
			continue
		}
		word := twentyWord{text: strings.TrimSpace(text)}
		if json.Unmarshal(fields["start_timestamp"], &word.start) != nil {
			word.start = nil
		}
		if json.Unmarshal(fields["end_timestamp"], &word.end) != nil {
			word.end = nil
		}
		words = append(words, word)
	}
	return words
}

// twentyOffset returns a word timestamp's offset from the recording start.
// A negative or nonfinite offset is ignored like a missing one.
func twentyOffset(stamp map[string]jsontext.Value) (float64, bool) {
	offset, ok := rawFiniteFloat(stamp["relative"])
	return offset, ok && offset >= 0
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
	entries, ok := twentyEntries(raw)
	if !ok {
		return Transcript{State: StateUnavailable, Reason: reasonInvalidSection}
	}
	transcript := Transcript{State: StateEmpty}
	for _, entry := range entries {
		segment := Segment{Speaker: "Unknown speaker"}
		if entry.speaker != "" {
			segment.Speaker = entry.speaker
		}
		texts := make([]string, 0, len(entry.words))
		for _, word := range entry.words {
			texts = append(texts, word.text)
			if offset, ok := twentyOffset(word.start); ok && segment.OffsetSeconds == nil {
				segment.OffsetSeconds = &offset
			}
			if segment.StartedAt == nil {
				if start, ok := rawTime(word.start["absolute"], false); ok {
					segment.StartedAt = &start
				}
			}
			if end, ok := rawTime(word.end["absolute"], false); ok {
				segment.EndedAt = &end
			}
		}
		segment.Text = strings.Join(texts, " ")
		transcript.Segments = append(transcript.Segments, segment)
	}
	if len(transcript.Segments) > 0 {
		transcript.State = StateAvailable
	}
	return transcript
}
