package hex

import (
	"encoding/json/v2"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
)

func AttendeeEmail(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value || !strings.Contains(value, "@") {
		return ""
	}
	return strings.ToLower(value)
}
func (m Meeting) ArchiveSnapshot(sourceID int64, identifier, email string) (meetingarchive.Snapshot, error) {
	raw, err := json.Marshal(struct {
		Version int     `json:"version"`
		Meeting Meeting `json:"meeting"`
	}{1, m}, json.Deterministic(true))
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	v := m.Manifest
	lines := []string{v.Title, "Status: " + v.Status}
	if v.Calendar != nil {
		lines = append(lines, "Calendar: "+v.Calendar.Title, "Attendees: "+strings.Join(v.Calendar.Attendees, ", "))
	}
	if v.Notes != nil {
		lines = append(lines, "", "Summary:", string(v.Notes.Summary))
	} else {
		lines = append(lines, "Summary: unavailable")
	}
	for _, chapter := range v.Chapters {
		lines = append(lines, fmt.Sprintf("Chapter [%s]: %s\n%s", (time.Duration(chapter.StartMS)*time.Millisecond).String(), chapter.Title, chapter.Summary))
	}
	lines = append(lines, "", "Transcript ("+m.TranscriptState+"):")
	for _, e := range m.Entries {
		lines = append(lines, meetingarchive.FormatTranscriptLine(time.Duration(e.StartMS)*time.Millisecond, e.Speaker(), e.Text))
	}
	body := strings.Join(lines, "\n")
	metadata, err := json.Marshal(map[string]any{"hex_id": v.ID, "status": v.Status, "source_identifier": identifier})
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	snapshot := meetingarchive.Snapshot{SourceID: sourceID, AccountEmail: email, SourceMessageID: "meeting:" + v.ID, Title: v.Title, StartedAt: time.UnixMilli(v.CreatedAtMS).UTC(), Body: body, Snippet: meetingarchive.Snippet(body), Raw: raw, RawFormat: RawFormat, Metadata: metadata, Organizer: &meetingarchive.Person{Email: email}}
	if v.Calendar != nil {
		for _, value := range v.Calendar.Attendees {
			if address := AttendeeEmail(value); address != "" {
				snapshot.Attendees = append(snapshot.Attendees, meetingarchive.Person{Email: address})
			}
		}
	}
	return snapshot, nil
}
