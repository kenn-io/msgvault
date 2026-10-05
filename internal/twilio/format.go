package twilio

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

type archivedCall struct {
	Call            Call            `json:"call"`
	Recordings      []Recording     `json:"recordings,omitempty"`
	Evidence        Evidence        `json:"evidence"`
	StartedAt       string          `json:"started_at,omitempty"`
	EndedAt         string          `json:"ended_at,omitempty"`
	DurationSeconds float64         `json:"duration_seconds"`
	Attendees       []archivePerson `json:"attendees,omitempty"`
	Transcript      *string         `json:"transcript,omitempty"`
	Segments        []Segment       `json:"transcript_segments,omitempty"`
}
type archivePerson struct {
	Phone string `json:"phone"`
}

func loadArchive(ctx context.Context, st *store.Store, sourceID int64, id string) (archivedCall, error) {
	var previous archivedCall
	rows, err := st.MessageMetadataBatch(sourceID, []string{id})
	if err != nil {
		return previous, err
	}
	if existing, ok := rows[id]; ok {
		raw, err := st.GetMessageRawContext(ctx, existing.ID)
		if err != nil {
			return previous, err
		}
		if err := json.Unmarshal(raw, &previous); err != nil {
			return previous, fmt.Errorf("decode archived Twilio call: %w", err)
		}
	}
	return previous, nil
}
func mergeRecordings(old, fresh []Recording) []Recording {
	byID := map[string]Recording{}
	for _, r := range old {
		byID[r.SID] = r
	}
	for _, r := range fresh {
		byID[r.SID] = r
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Recording, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}
func mergeArchive(old archivedCall, call Call, recordings []Recording, evidence Evidence) archivedCall {
	if call.StartTime == "" {
		call.StartTime = old.Call.StartTime
	}
	if call.EndTime == "" {
		call.EndTime = old.Call.EndTime
	}
	if call.Duration == "" {
		call.Duration = old.Call.Duration
	}
	if call.From == "" {
		call.From = old.Call.From
	}
	if call.To == "" {
		call.To = old.Call.To
	}
	out := archivedCall{Call: call, Recordings: recordings}
	byID := map[string]Transcript{}
	for _, tr := range old.Evidence.Transcripts {
		byID[tr.Kind+":"+tr.ID] = tr
	}
	for _, tr := range evidence.Transcripts {
		key := tr.Kind + ":" + tr.ID
		previous, exists := byID[key]
		if exists && previous.Usable && !tr.Usable {
			continue
		}
		if !tr.Complete && exists && previous.Complete {
			continue
		}
		byID[key] = tr
	}
	keys := make([]string, 0, len(byID))
	for key := range byID {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out.Evidence.Transcripts = append(out.Evidence.Transcripts, byID[key])
	}
	out.Evidence.Diagnostics = evidence.Diagnostics
	return out
}

// canonicalSegments picks one transcript per recording, preferring Conversation
// Intelligence over legacy transcription and then the newest, in recording
// start order.
func canonicalSegments(transcripts []Transcript, recordings []Recording) ([]Segment, bool) {
	selected := map[string]Transcript{}
	complete := false
	for _, tr := range transcripts {
		complete = complete || tr.Complete
		if !tr.Usable {
			continue
		}
		key := tr.SourceID
		if key == "" {
			key = tr.ID
		}
		if prior, ok := selected[key]; !ok || supersedes(tr, prior) {
			selected[key] = tr
		}
	}
	keys := make([]string, 0, len(selected))
	for key := range selected {
		keys = append(keys, key)
	}
	started := map[string]time.Time{}
	for _, recording := range recordings {
		at := ParseTime(recording.StartTime)
		if at.IsZero() {
			at = ParseTime(recording.DateCreated)
		}
		started[recording.SID] = at
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := started[keys[i]], started[keys[j]]
		if !a.Equal(b) {
			return a.Before(b)
		}
		return keys[i] < keys[j]
	})
	out := []Segment{}
	for _, key := range keys {
		out = append(out, selected[key].Segments...)
	}
	return out, complete
}

// supersedes reports whether tr replaces prior as a recording's text: a
// retranscription leaves the old transcript behind as evidence only.
func supersedes(tr, prior Transcript) bool {
	if classic, priorClassic := tr.Kind == "classic", prior.Kind == "classic"; classic != priorClassic {
		return classic
	}
	created, priorCreated := ParseTime(tr.DateCreated), ParseTime(prior.DateCreated)
	if !created.Equal(priorCreated) {
		return created.After(priorCreated)
	}
	return tr.ID > prior.ID
}

func parseDurationSeconds(value string) (float64, bool) {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > 365*24*3600 {
		return 0, false
	}
	return seconds, true
}
func durationSeconds(value string) float64 {
	seconds, _ := parseDurationSeconds(value)
	return seconds
}
func (a archivedCall) snapshot(sourceID int64, email string) (meetingarchive.Snapshot, error) {
	start, end := callStart(a), ParseTime(a.Call.EndTime)
	if !start.IsZero() {
		a.StartedAt = start.Format(time.RFC3339Nano)
	}
	if !end.IsZero() {
		a.EndedAt = end.Format(time.RFC3339Nano)
	}
	var durationValid bool
	a.DurationSeconds, durationValid = parseDurationSeconds(a.Call.Duration)
	if !durationValid && end.After(start) && !start.IsZero() {
		a.DurationSeconds = end.Sub(start).Seconds()
	}
	people := []meetingarchive.Person{}
	for _, phone := range []string{a.Call.From, a.Call.To} {
		person := (meetingarchive.Person{Phone: phone}).Normalized()
		if person.Phone != "" {
			people = append(people, person)
			a.Attendees = append(a.Attendees, archivePerson{Phone: person.Phone})
		}
	}
	segments, complete := canonicalSegments(a.Evidence.Transcripts, a.Recordings)
	// Segments come in recording order, so numbering scopes as they appear
	// names recordings without putting their SIDs in the text.
	numbers := map[string]int{}
	for _, s := range segments {
		if _, ok := numbers[s.Scope]; !ok && s.Scope != "" {
			numbers[s.Scope] = len(numbers) + 1
		}
	}
	var text strings.Builder
	for _, s := range segments {
		if n := numbers[s.Scope]; len(numbers) > 1 && n > 0 {
			label := fmt.Sprintf("Recording %d", n)
			if s.Speaker != "" {
				label += " / " + s.Speaker
			}
			s.Speaker = label
			s.OffsetSeconds = nil
		}
		a.Segments = append(a.Segments, s)
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		if s.Speaker != "" {
			text.WriteString(s.Speaker + ": ")
		}
		text.WriteString(s.Text)
	}
	if complete || len(segments) > 0 {
		value := text.String()
		a.Transcript = &value
	}
	raw, err := json.Marshal(a, json.Deterministic(true))
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	metadata, err := json.Marshal(map[string]any{"call_sid": a.Call.SID, "parent_call_sid": a.Call.ParentCallSID, "direction": a.Call.Direction, "status": a.Call.Status, "duration_seconds": a.DurationSeconds, "recording_count": len(a.Recordings)}, json.Deterministic(true))
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	body := text.String()
	snippet := body
	if runes := []rune(snippet); len(runes) > 500 {
		snippet = string(runes[:500])
	}
	return meetingarchive.Snapshot{SourceID: sourceID, AccountEmail: email, SourceMessageID: a.Call.SID, SourceConversationID: a.Call.SID, Title: callTitle(a.Call), StartedAt: start, Body: body, Snippet: snippet, Metadata: metadata, Raw: raw, RawFormat: RawFormat, Attendees: people}, nil
}

// callTitle names the other party: the caller on an inbound call, the callee
// on an outbound one. Without a direction or number it falls back to "Twilio call".
func callTitle(call Call) string {
	direction := strings.ToLower(call.Direction)
	other := ""
	switch {
	case direction == "inbound":
		other = call.From
	case strings.HasPrefix(direction, "outbound"):
		other = call.To
	}
	return meetingarchive.CallTitle("Twilio call", other, direction == "inbound")
}
