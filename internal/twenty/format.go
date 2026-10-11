package twenty

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingcontent"
)

func archiveSnapshot(sourceID int64, accountEmail string, recording Recording, calendar *Calendar) (meetingarchive.Snapshot, bool, error) {
	var event jsontext.Value
	participants := []jsontext.Value{}
	if calendar != nil {
		event = calendar.Raw
		participants = append(participants, calendar.Participants...)
	}
	raw, err := json.Marshal(struct {
		Version      int              `json:"schema_version"`
		Recording    jsontext.Value   `json:"recording"`
		Calendar     jsontext.Value   `json:"calendar_event"`
		Participants []jsontext.Value `json:"participants"`
	}{1, recording.Raw, event, participants}, json.Deterministic(true))
	if err != nil {
		return meetingarchive.Snapshot{}, false, errors.New("invalid Twenty recording evidence")
	}
	content := meetingcontent.Decode(RawFormat, raw, nil)
	if content.Summary.Reason == "raw_too_large" {
		return meetingarchive.Snapshot{}, false, errors.New("twenty archive evidence exceeds the meeting content size limit")
	}
	if content.Summary.State != meetingcontent.StateAvailable && content.Transcript.State != meetingcontent.StateAvailable {
		return meetingarchive.Snapshot{}, false, nil
	}
	var calendarFields struct {
		StartsAt string `json:"startsAt"`
		Title    string `json:"title"`
	}
	if calendar != nil {
		if err := json.Unmarshal(calendar.Raw, &calendarFields); err != nil {
			return meetingarchive.Snapshot{}, false, errors.New("invalid Twenty calendar evidence")
		}
	}
	var started time.Time
	for _, value := range []string{recording.StartedAt, calendarFields.StartsAt, recording.CreatedAt} {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
			started = parsed.UTC()
			break
		}
	}
	if started.IsZero() {
		return meetingarchive.Snapshot{}, false, errors.New("twenty recording has no valid occurrence time")
	}
	title := strings.TrimSpace(recording.Title)
	if title == "" {
		title = strings.TrimSpace(calendarFields.Title)
	}
	if title == "" {
		title = "Untitled meeting"
	}
	snapshot := meetingarchive.Snapshot{SourceID: sourceID, AccountEmail: accountEmail, SourceMessageID: "recording:" + recording.ID, SourceConversationID: "recording:" + recording.ID, Title: title, StartedAt: started, Raw: raw, RawFormat: RawFormat}
	var names []string
	for _, person := range content.SourceParticipants {
		if person.Name != "" {
			names = append(names, person.Name)
		}
		if person.Email == "" {
			continue
		}
		archived := meetingarchive.Person{Name: person.Name, Email: person.Email}
		if person.Role == "from" && snapshot.Organizer == nil {
			snapshot.Organizer = &archived
		} else {
			snapshot.Attendees = append(snapshot.Attendees, archived)
		}
	}
	var body strings.Builder
	body.WriteString(title + "\n" + started.Format(time.RFC3339) + "\n")
	if len(names) > 0 {
		body.WriteString("Attendees: " + strings.Join(names, ", ") + "\n")
	}
	if content.Summary.State == meetingcontent.StateAvailable {
		body.WriteString("\nSummary\n" + content.Summary.Text + "\n")
	}
	if content.Transcript.State == meetingcontent.StateAvailable {
		body.WriteString("\nTranscript\n")
		for _, segment := range content.Transcript.Segments {
			body.WriteString(segment.Speaker + ": " + segment.Text + "\n")
		}
	}
	snapshot.Body = body.String()
	snapshot.Snippet = snippet(content)
	metadata := map[string]any{"provider": SourceType, "recording_id": recording.ID}
	if recording.ApplicationID != "" {
		metadata["application_id"] = recording.ApplicationID
	}
	snapshot.Metadata, err = json.Marshal(metadata, json.Deterministic(true))
	return snapshot, true, err
}

// snippet previews the summary, or else the opening of the transcript,
// without joining a whole long transcript first.
func snippet(content meetingcontent.Content) string {
	const limit = 200
	text := strings.TrimSpace(content.Summary.Text)
	if text == "" {
		var opening strings.Builder
		for _, segment := range content.Transcript.Segments {
			if utf8.RuneCountInString(opening.String()) > limit {
				break
			}
			opening.WriteString(segment.Text)
			opening.WriteByte(' ')
		}
		text = strings.TrimSpace(opening.String())
	}
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit])
	}
	return text
}
