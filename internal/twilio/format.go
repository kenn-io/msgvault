package twilio

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

type archivedCall struct {
	LatestAcquisition  Evidence                     `json:"latest_acquisition"`
	Call               Call                         `json:"call"`
	Recordings         []Recording                  `json:"recordings,omitempty"`
	RecordingArtifacts map[string]recordingArtifact `json:"recording_artifacts,omitempty"`
	Evidence           Evidence                     `json:"evidence"`
	StartedAt          string                       `json:"started_at,omitempty"`
	EndedAt            string                       `json:"ended_at,omitempty"`
	DurationSeconds    float64                      `json:"duration_seconds"`
	Attendees          []archivePerson              `json:"attendees,omitempty"`
	Transcript         *string                      `json:"transcript,omitempty"`
	Segments           []Segment                    `json:"transcript_segments,omitempty"`
}
type recordingArtifact struct {
	State       attachmentpolicy.DownloadState `json:"state,omitempty"`
	SkipReason  attachmentpolicy.SkipReason    `json:"skip_reason,omitempty"`
	Filename    string                         `json:"filename,omitempty"`
	MIMEType    string                         `json:"mime_type,omitempty"`
	ContentHash string                         `json:"content_hash,omitempty"`
	StoragePath string                         `json:"storage_path,omitempty"`
	Size        int64                          `json:"size,omitempty"`
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
	artifacts := old.RecordingArtifacts
	if artifacts == nil {
		artifacts = map[string]recordingArtifact{}
	}
	out := archivedCall{Call: call, Recordings: mergeRecordings(old.Recordings, recordings), RecordingArtifacts: artifacts}
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
	out.Evidence.RelayEvents = mergeRawEvidence(old.Evidence.RelayEvents, evidence.RelayEvents)
	out.Evidence.Conversations = mergeRawEvidence(old.Evidence.Conversations, evidence.Conversations)
	out.LatestAcquisition = evidence
	return out
}

// Keep the exact correlation evidence that led to archived speech. A later
// successful empty collection or feature absence must not erase provenance.
func mergeRawEvidence(old, fresh []jsontext.Value) []jsontext.Value {
	out := make([]jsontext.Value, 0, len(old)+len(fresh))
	seen := map[string]bool{}
	for _, items := range [][]jsontext.Value{old, fresh} {
		for _, item := range items {
			key := string(item)
			if !seen[key] {
				seen[key] = true
				out = append(out, item)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
func transcriptRank(kind string) int {
	switch kind {
	case "orchestrator":
		return 4
	case "classic":
		return 3
	case "batch":
		return 2
	case "legacy":
		return 1
	}
	return 0
}
func canonicalSegments(transcripts []Transcript) ([]Segment, bool) {
	// Orchestrator has an authoritative call channel and no documented mapping
	// from opaque resourceId to individual RE jobs. Use the call-scoped view as
	// one representation instead of duplicating it across recordings/Relay.
	var callView []Segment
	seen := map[string]bool{}
	for _, tr := range transcripts {
		if !tr.Usable || (tr.Kind != "orchestrator" && tr.Kind != "batch") {
			continue
		}
		if len(tr.Communications) > 0 {
			for _, comm := range tr.Communications {
				if comm.Content.Type != "TEXT" || strings.TrimSpace(comm.Content.Text) == "" || seen[comm.ID] {
					continue
				}
				seen[comm.ID] = true
				speaker := comm.Author.ParticipantID
				if speaker == "" {
					speaker = comm.Author.Address
				}
				segment := Segment{Speaker: speaker, Text: comm.Content.Text, Scope: comm.ChannelID}
				if at := ParseTime(comm.OccurredAt); !at.IsZero() {
					segment.StartedAt = &at
				}
				callView = append(callView, segment)
			}
		} else if tr.Kind == "orchestrator" {
			callView = append(callView, tr.Segments...)
		}
	}
	sort.SliceStable(callView, func(i, j int) bool {
		a, b := callView[i].StartedAt, callView[j].StartedAt
		if a == nil {
			return false
		}
		if b == nil {
			return true
		}
		return a.Before(*b)
	})
	if len(callView) > 0 {
		return callView, true
	}
	selected := map[string]Transcript{}
	complete := false
	for _, tr := range transcripts {
		if tr.Complete && tr.Kind != "batch" {
			complete = true
		}
		if !tr.Usable {
			continue
		}
		key := tr.SourceID
		if key == "" {
			key = tr.ID
		}
		prior, ok := selected[key]
		if !ok || transcriptRank(tr.Kind) > transcriptRank(prior.Kind) {
			selected[key] = tr
		}
	}
	keys := make([]string, 0, len(selected))
	for key := range selected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := []Segment{}
	for _, key := range keys {
		out = append(out, selected[key].Segments...)
	}
	return out, complete
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
	start, end := ParseTime(a.Call.StartTime), ParseTime(a.Call.EndTime)
	if start.IsZero() {
		for _, r := range a.Recordings {
			start = ParseTime(r.StartTime)
			if !start.IsZero() {
				break
			}
		}
	}
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
	segments, complete := canonicalSegments(a.Evidence.Transcripts)
	scopeSet := map[string]bool{}
	for _, s := range segments {
		scopeSet[s.Scope] = true
	}
	var text strings.Builder
	for _, s := range segments {
		if len(scopeSet) > 1 && s.Scope != "" {
			s.Speaker = s.Scope + " / " + s.Speaker
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
	return meetingarchive.Snapshot{SourceID: sourceID, AccountEmail: email, SourceMessageID: a.Call.SID, SourceConversationID: a.Call.SID, Title: "Twilio call", StartedAt: start, Body: body, Snippet: snippet, Metadata: metadata, Raw: raw, RawFormat: RawFormat, Attendees: people}, nil
}
