package chatwoot

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

func providerTime(raw jsontext.Value) time.Time {
	var numeric float64
	if json.Unmarshal(raw, &numeric) == nil && numeric > 0 && !math.IsInf(numeric, 0) && numeric < float64(math.MaxInt64/1000) {
		return time.UnixMilli(int64(numeric * 1000)).UTC()
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		parsed, _ := time.Parse(time.RFC3339Nano, text)
		return parsed
	}
	return time.Time{}
}

func callEvidence(m Message) Call {
	var call Call
	if data, ok := m.ContentAttributes["data"]; ok {
		_ = json.Unmarshal(data, &call)
		var fallback struct {
			CallID     int64  `json:"call_id"`
			CallSID    string `json:"call_sid"`
			Source     string `json:"call_source"`
			Direction  string `json:"call_direction"`
			AcceptedBy Actor  `json:"accepted_by"`
		}
		_ = json.Unmarshal(data, &fallback)
		call.ID = fallback.CallID
		call.ProviderCallID = fallback.CallSID
		call.Provider = fallback.Source
		call.Direction = fallback.Direction
		if call.AcceptedByAgentID == 0 {
			call.AcceptedByAgentID = fallback.AcceptedBy.ID
			call.AcceptedByAgentName = actorName(fallback.AcceptedBy)
		}
	}
	if m.Call != nil {
		fallback := call
		call = *m.Call
		if call.ID == 0 {
			call.ID = fallback.ID
		}
		if call.ProviderCallID == "" {
			call.ProviderCallID = fallback.ProviderCallID
		}
		if call.Provider == "" {
			call.Provider = fallback.Provider
		}
		if call.Direction == "" {
			call.Direction = fallback.Direction
		}
		if call.Status == "" {
			call.Status = fallback.Status
		}
		if call.DurationSeconds == nil {
			call.DurationSeconds = fallback.DurationSeconds
		}
		if call.AcceptedByAgentID == 0 {
			call.AcceptedByAgentID = fallback.AcceptedByAgentID
		}
		if call.AcceptedByAgentName == "" && call.AcceptedByAgentID == fallback.AcceptedByAgentID {
			call.AcceptedByAgentName = fallback.AcceptedByAgentName
		}
		if len(call.StartedAt) == 0 || string(call.StartedAt) == "null" {
			call.StartedAt = fallback.StartedAt
		}
		if len(call.EndedAt) == 0 || string(call.EndedAt) == "null" {
			call.EndedAt = fallback.EndedAt
		}
		if call.FromNumber == "" {
			call.FromNumber = fallback.FromNumber
		}
		if call.ToNumber == "" {
			call.ToNumber = fallback.ToNumber
		}
		if call.RecordingURL == "" {
			call.RecordingURL = fallback.RecordingURL
		}
		if call.Transcript == "" {
			call.Transcript = fallback.Transcript
		}
	}
	switch call.Direction {
	case "inbound":
		call.Direction = "incoming"
	case "outbound":
		call.Direction = "outgoing"
	}
	return call
}

func (imp *Importer) persistCall(ctx context.Context, sourceID int64, c Conversation, m Message, chatMessageID int64, contact Actor, contactID int64, sender Actor, senderID int64, chatMedia map[string]store.AttachmentRef, opts ImportOptions, sum *ImportSummary) (int64, error) {
	call := callEvidence(m)
	transcript := call.Transcript
	for _, a := range m.Attachments {
		if a.TranscribedText != "" {
			if transcript == "" {
				transcript = a.TranscribedText
			}
		}
	}
	handler := Actor{ID: call.AcceptedByAgentID, Type: actorUser, Name: call.AcceptedByAgentName}
	handlerID, err := imp.resolveActor(ctx, sourceID, handler)
	if err != nil {
		return 0, err
	}
	organizer, organizerID := sender, senderID
	if call.Direction == "incoming" {
		organizer, organizerID = contact, contactID
	}
	if call.Direction == "outgoing" && actorKind(sender) != actorUser && actorKind(sender) != "agent_bot" {
		organizer, organizerID = Actor{}, 0
	}
	person := func(a Actor, pid int64) meetingarchive.Person {
		return meetingarchive.Person{ParticipantID: pid, Name: actorName(a), Email: a.Email, Phone: a.PhoneNumber}
	}
	var attendees []meetingarchive.Person
	if contactID > 0 && contactID != organizerID {
		attendees = append(attendees, person(contact, contactID))
	}
	if handlerID > 0 && handlerID != organizerID && handlerID != contactID {
		attendees = append(attendees, person(handler, handlerID))
	}
	if senderID > 0 && senderID != organizerID && senderID != contactID && senderID != handlerID {
		attendees = append(attendees, person(sender, senderID))
	}
	var owner *meetingarchive.Person
	if organizerID > 0 {
		p := person(organizer, organizerID)
		owner = &p
	}
	title := "Chatwoot call"
	if actorName(contact) != "" {
		title += " with " + actorName(contact)
	}
	started, ended := providerTime(call.StartedAt), providerTime(call.EndedAt)
	normalized := map[string]any{"title": title, "transcript": transcript, "status": call.Status, "direction": call.Direction, "chatwoot": m.Raw, "organizer": owner, "attendees": attendees}
	if transcript == "" {
		delete(normalized, "transcript")
	}
	// Marshal provider-resolved people with the canonical lower-case wire keys.
	personWire := func(p meetingarchive.Person) map[string]any {
		return map[string]any{"participant_id": p.ParticipantID, "name": p.Name, "email": p.Email, "phone": p.Phone}
	}
	if owner != nil {
		normalized["organizer"] = personWire(*owner)
	}
	people := make([]map[string]any, 0, len(attendees))
	for _, p := range attendees {
		people = append(people, personWire(p))
	}
	normalized["attendees"] = people
	if !started.IsZero() {
		normalized["started_at"] = started
	}
	if !ended.IsZero() {
		normalized["ended_at"] = ended
	}
	if call.DurationSeconds != nil && *call.DurationSeconds >= 0 && !math.IsNaN(*call.DurationSeconds) && !math.IsInf(*call.DurationSeconds, 0) {
		normalized["duration_seconds"] = *call.DurationSeconds
	}
	metadata, err := json.Marshal(map[string]any{"provider": SourceType, "chat_message_id": chatMessageID, "conversation_id": c.ID, "inbox_id": opts.InboxID, "call": call, "handling_agent": handler, "handling_agent_participant_id": handlerID}, json.Deterministic(true))
	if err != nil {
		return 0, err
	}
	raw, err := json.Marshal(normalized, json.Deterministic(true))
	if err != nil {
		return 0, err
	}
	occurred := started
	if occurred.IsZero() && m.CreatedAt > 0 {
		occurred = time.Unix(m.CreatedAt, 0).UTC()
	}
	fromMe := imp.personalActor(organizer, opts)
	result, err := meetingarchive.New(imp.store).Upsert(ctx, meetingarchive.Snapshot{
		SourceID: sourceID, SourceMessageID: "call:" + strconv.FormatInt(m.ID, 10), SourceConversationID: "call:" + strconv.FormatInt(c.ID, 10) + ":" + strconv.FormatInt(m.ID, 10),
		Title: title, StartedAt: occurred, Body: strings.TrimSpace(transcript), Snippet: snippet(transcript), Raw: raw, RawFormat: "meeting_json", Metadata: metadata, Organizer: owner, Attendees: attendees, OwnerAttribution: &fromMe,
	}, meetingarchive.UpsertOptions{})
	if err != nil {
		return result.MessageID, err
	}
	audio := make([]Attachment, 0, len(m.Attachments)+1)
	for _, a := range m.Attachments {
		if a.FileType == "audio" || strings.HasPrefix(a.ContentType, "audio/") {
			audio = append(audio, a)
		}
	}
	sameRecording := false
	for _, a := range audio {
		remote := a.DataURL
		if remote == "" {
			remote = a.ExternalURL
		}
		if mediaIdentity(remote) == mediaIdentity(call.RecordingURL) {
			sameRecording = true
		}
	}
	if !sameRecording && call.RecordingURL != "" {
		audio = append(audio, Attachment{ID: -m.ID, FileType: "audio", DataURL: call.RecordingURL, TranscribedText: call.Transcript})
	}
	if err = imp.persistMedia(ctx, result.MessageID, audio, opts, sum, chatMedia); err != nil {
		return result.MessageID, err
	}
	sum.Meetings++
	return result.MessageID, nil
}
