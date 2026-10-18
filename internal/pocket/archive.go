package pocket

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
)

type evidence struct {
	Version    int                    `json:"schema_version"`
	Recording  jsontext.Value         `json:"recording"`
	Content    meetingcontent.Content `json:"content"`
	Title      string                 `json:"title"`
	StartedAt  time.Time              `json:"started_at"`
	RecordedAt string                 `json:"recorded_at,omitempty"`
	Owner      *RecordingOwner        `json:"owner,omitempty"`
}

func buildSnapshot(st *store.Store, sourceID int64, account Account, rec Recording) (meetingarchive.Snapshot, error) {
	content, err := Normalize(rec)
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	existing, err := st.MessageExistsBatch(sourceID, []string{rec.ID})
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	if id := existing[rec.ID]; id != 0 {
		raw, e := st.GetMessageRaw(id)
		if e != nil {
			return meetingarchive.Snapshot{}, e
		}
		var previous evidence
		if len(raw) > maxEvidenceBytes || json.Unmarshal(raw, &previous) != nil || previous.Version != 1 {
			return meetingarchive.Snapshot{}, errors.New("invalid archived Pocket evidence")
		}
		decoded := meetingcontent.Decode("pocket_json", raw, nil)
		if decoded.Summary.Reason == "invalid_raw" {
			return meetingarchive.Snapshot{}, errors.New("invalid archived Pocket content")
		}
		var previousRecording Recording
		_ = json.Unmarshal(previous.Recording, &previousRecording)
		previousOwner := previous.Owner
		if previousOwner == nil {
			previousOwner = previousRecording.Owner
		}
		rec.Owner = mergeRecordingOwner(rec.Owner, previousOwner)
		previousRecordedAt := previous.RecordedAt
		if previousRecordedAt == "" {
			previousRecordedAt = previousRecording.RecordedAt
		}
		if rec.RecordedAt == "" {
			if previousRecordedAt != "" {
				rec.RecordedAt = previousRecordedAt
			}
			if !previous.StartedAt.IsZero() {
				rec.StartedAt = previous.StartedAt
			} else if previousRecordedAt != "" {
				rec.StartedAt, _ = time.Parse(time.RFC3339Nano, previousRecordedAt)
			}
		}
		if content.Summary.State == meetingcontent.StateUnavailable {
			content.Summary = previous.Content.Summary
		}
		if content.Transcript.State == meetingcontent.StateUnavailable {
			content.Transcript = previous.Content.Transcript
			content.SourceParticipants = previous.Content.SourceParticipants
		}
		if content.ActionCoverage == meetingcontent.CoverageUnavailable {
			content.Actions = previous.Content.Actions
			content.ActionCoverage = previous.Content.ActionCoverage
			content.ActionReason = previous.Content.ActionReason
		}
		if content.DurationSeconds == nil {
			content.DurationSeconds = previous.Content.DurationSeconds
			content.DurationBasis = previous.Content.DurationBasis
		}
		if strings.TrimSpace(rec.Title) == "" {
			rec.Title = previous.Title
		}
		if rec.StartedAt.IsZero() {
			rec.StartedAt = previous.StartedAt
		}
	}
	return makeSnapshot(sourceID, account, rec, content)
}

func mergeRecordingOwner(current, previous *RecordingOwner) *RecordingOwner {
	if current == nil {
		if previous == nil {
			return nil
		}
		merged := *previous
		return &merged
	}
	merged := *current
	if previous == nil {
		return &merged
	}
	if strings.TrimSpace(merged.Email) == "" {
		merged.Email = previous.Email
	}
	if strings.TrimSpace(merged.UserID) == "" {
		merged.UserID = previous.UserID
	}
	if strings.TrimSpace(merged.Name) == "" {
		merged.Name = previous.Name
	}
	return &merged
}

func makeSnapshot(sourceID int64, account Account, rec Recording, content meetingcontent.Content) (meetingarchive.Snapshot, error) {
	title := strings.TrimSpace(rec.Title)
	if title == "" {
		title = "Pocket recording"
	}
	raw, err := json.Marshal(evidence{Version: 1, Recording: rec.Raw, Content: content, Title: title, StartedAt: rec.StartedAt, RecordedAt: rec.RecordedAt, Owner: rec.Owner}, json.Deterministic(true))
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	if len(raw) > maxEvidenceBytes {
		return meetingarchive.Snapshot{}, errors.New("pocket archive evidence exceeds 64 MiB")
	}
	metadata, err := json.Marshal(meetingcontent.ProjectionContent(content), json.Deterministic(true))
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	var body strings.Builder
	if content.Summary.Text != "" {
		body.WriteString(content.Summary.Text)
		body.WriteString("\n\n")
	}
	for _, action := range content.Actions {
		body.WriteString(action.Title)
		if action.Description != "" {
			body.WriteString(": ")
			body.WriteString(action.Description)
		}
		body.WriteByte('\n')
	}
	for _, seg := range content.Transcript.Segments {
		if seg.Speaker != "" {
			body.WriteString(seg.Speaker)
			body.WriteString(": ")
		}
		body.WriteString(seg.Text)
		body.WriteByte('\n')
	}
	body.WriteString(content.Transcript.Text)
	text := strings.TrimSpace(body.String())
	snippet := []rune(text)
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	snapshot := meetingarchive.Snapshot{SourceID: sourceID, AccountEmail: account.Email, SourceMessageID: rec.ID, SourceConversationID: rec.ID, Title: title, StartedAt: rec.StartedAt, Body: text, Snippet: string(snippet), Raw: raw, RawFormat: "pocket_json", Metadata: metadata}
	if rec.Owner != nil && strings.TrimSpace(rec.Owner.Email) != "" {
		snapshot.Organizer = &meetingarchive.Person{Email: rec.Owner.Email, Name: rec.Owner.Name}
	}
	return snapshot, nil
}
