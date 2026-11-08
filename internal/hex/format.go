package hex

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/hex/evidence"
	"go.kenn.io/msgvault/internal/meetingarchive"
)

func (m Meeting) ArchiveSnapshot(sourceID int64, identifier, email string) (meetingarchive.Snapshot, error) {
	raw, err := json.Marshal(evidence.Envelope{Version: evidence.Version, Meeting: evidence.Meeting(m)}, json.Deterministic(true))
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	v := m.Manifest
	lines := []string{}
	shownTitles := map[string]bool{strings.TrimSpace(v.DisplayTitle()): true}
	appendTitle := func(label, value string) {
		title := strings.TrimSpace(value)
		if !shownTitles[title] {
			lines = append(lines, label+value)
			shownTitles[title] = true
		}
	}
	appendTitle("Original: ", v.Title)
	lines = append(lines, "Status: "+v.Status)
	if v.App != "" {
		lines = append(lines, "Platform: "+v.App)
	}
	if v.ProvisionalTitle != "" {
		appendTitle("Provisional title: ", v.ProvisionalTitle)
	}
	if v.Notes != nil {
		appendTitle("Notes title: ", v.Notes.Title)
	}
	if v.Calendar != nil {
		appendTitle("Calendar: ", v.Calendar.Title)
		lines = append(lines, "Attendees: "+strings.Join(v.Calendar.Attendees, ", "))
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
	metadata, err := json.Marshal(struct {
		HexID            string `json:"hex_id"`
		Status           string `json:"status"`
		SourceIdentifier string `json:"source_identifier"`
		MeetingPlatform  string `json:"meeting_platform,omitempty"`
	}{
		HexID: v.ID, Status: v.Status, SourceIdentifier: identifier, MeetingPlatform: v.App,
	})
	if err != nil {
		return meetingarchive.Snapshot{}, err
	}
	snapshot := meetingarchive.Snapshot{SourceID: sourceID, AccountEmail: email, SourceMessageID: "meeting:" + v.ID, Title: v.DisplayTitle(), StartedAt: time.UnixMilli(v.CreatedAtMS).UTC(), Body: body, Snippet: meetingarchive.Snippet(body), Raw: raw, RawFormat: RawFormat, Metadata: metadata, Organizer: &meetingarchive.Person{Email: email}}
	if v.Calendar != nil {
		for _, value := range v.Calendar.Attendees {
			if address := evidence.AttendeeEmail(value); address != "" {
				snapshot.Attendees = append(snapshot.Attendees, meetingarchive.Person{Email: address})
			}
		}
	}
	return snapshot, nil
}
