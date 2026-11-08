package notionmeetings

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
)

type meetingMetadata struct {
	ParentKind         string   `json:"parent_kind,omitempty"`
	ParentID           string   `json:"parent_id,omitempty"`
	EditorUserID       string   `json:"editor_user_id,omitempty"`
	AttendeeIDs        []string `json:"attendee_ids,omitempty"`
	IsFullPage         *bool    `json:"is_full_page,omitempty"`
	CalendarEventID    string   `json:"calendar_event_id,omitempty"`
	CreatedTime        string   `json:"created_time,omitempty"`
	LastEditedTime     string   `json:"last_edited_time,omitempty"`
	RecordingStartTime string   `json:"recording_start_time,omitempty"`
	RecordingEndTime   string   `json:"recording_end_time,omitempty"`
	InTrash            bool     `json:"in_trash"`
	Archived           bool     `json:"archived"`
	SourceURL          string   `json:"source_url,omitempty"`

	Platform                   string         `json:"platform"`
	SourceIdentifier           string         `json:"source_identifier,omitempty"`
	MeetingNoteBlockID         string         `json:"meeting_note_block_id"`
	PageID                     string         `json:"page_id"`
	Status                     string         `json:"status,omitempty"`
	Lifecycle                  string         `json:"lifecycle"`
	CreatorUserID              string         `json:"creator_user_id,omitempty"`
	StartedAt                  string         `json:"started_at"`
	EndedAt                    string         `json:"ended_at,omitempty"`
	SummaryBlockID             string         `json:"summary_block_id,omitempty"`
	NotesBlockID               string         `json:"notes_block_id,omitempty"`
	TranscriptBlockID          string         `json:"transcript_block_id,omitempty"`
	HasSummary                 bool           `json:"has_summary"`
	HasNotes                   bool           `json:"has_notes"`
	HasTranscript              bool           `json:"has_transcript"`
	MarkdownTranscriptFallback bool           `json:"markdown_transcript_fallback,omitzero"`
	UnresolvedAttendeeIDs      []string       `json:"unresolved_attendee_ids,omitempty"`
	ResolvedUsers              []resolvedUser `json:"resolved_users,omitempty"`
	Warnings                   []string       `json:"warnings,omitempty"`
}

type rawBlockTree struct {
	Root                 jsontext.Value   `json:"root,omitzero"`
	Pages                []jsontext.Value `json:"pages,omitempty"` // Legacy v1 only.
	Blocks               []jsontext.Value `json:"blocks,omitempty"`
	Complete             bool             `json:"complete"`
	Filtered             bool             `json:"filtered"`
	SourceLastEditedTime string           `json:"source_last_edited_time,omitempty"`
	Preserved            bool             `json:"preserved,omitzero"`
	SourceMeetingBlock   jsontext.Value   `json:"source_meeting_block,omitzero"`
}

type rawEvidence struct {
	SchemaVersion  int               `json:"schema_version"`
	Discovery      jsontext.Value    `json:"discovery"`
	MeetingBlock   jsontext.Value    `json:"meeting_block"`
	Summary        rawBlockTree      `json:"summary,omitzero"`
	Notes          rawBlockTree      `json:"notes,omitzero"`
	Transcript     rawBlockTree      `json:"transcript,omitzero"`
	PageMarkdown   jsontext.Value    `json:"page_markdown,omitzero"`
	Canonical      canonicalEvidence `json:"canonical"`
	AttendeeLabels []string          `json:"attendee_labels,omitempty"`
	ResolvedUsers  []resolvedUser    `json:"resolved_users,omitempty"`
	Warnings       []string          `json:"warnings,omitempty"`
}

type canonicalEvidence struct {
	Summary    string `json:"summary,omitempty"`
	Notes      string `json:"notes,omitempty"`
	Transcript string `json:"transcript,omitempty"`
}

var errArchiveEvidenceLimit = errors.New("Notion meeting exceeded archive evidence limit")

func (h *HydratedMeeting) ArchiveSnapshot(sourceID int64, identifier, accountEmail string) (meetingarchive.Snapshot, error) {
	startedAt, endedAt := h.times()
	if startedAt.IsZero() {
		return meetingarchive.Snapshot{}, fmt.Errorf("notion meeting %s has no usable start or creation time", h.Discovery.ID)
	}
	title := strings.TrimSpace(h.Discovery.Title())
	if title == "" {
		title = "Meeting on " + startedAt.UTC().Format(time.DateOnly)
	}
	body := h.body(title, startedAt, endedAt)
	children := h.Discovery.MeetingNotes.Children
	pageID := ""
	if h.Discovery.Parent.Type == "page_id" {
		pageID = h.Discovery.Parent.ID()
	}
	metadata, err := json.Marshal(meetingMetadata{
		ParentKind: h.Discovery.Parent.Type, ParentID: h.Discovery.Parent.ID(),
		EditorUserID: h.Discovery.LastEditedBy.ID, AttendeeIDs: h.Discovery.MeetingNotes.CalendarEvent.Attendees,
		IsFullPage: h.Discovery.MeetingNotes.IsFullPage, CalendarEventID: h.Discovery.MeetingNotes.CalendarEvent.EventID,
		CreatedTime: h.Discovery.CreatedTime, LastEditedTime: h.Discovery.LastEditedTime,
		RecordingStartTime: h.Discovery.MeetingNotes.Recording.StartTime, RecordingEndTime: h.Discovery.MeetingNotes.Recording.EndTime,
		InTrash: h.Discovery.InTrash, Archived: h.Discovery.Archived, SourceURL: h.Discovery.URL,
		Platform: SourceType, SourceIdentifier: identifier, MeetingNoteBlockID: h.Discovery.ID,
		PageID: pageID, Status: h.Discovery.MeetingNotes.Status,
		Lifecycle:      lifecycleForStatus(h.Discovery.MeetingNotes.Status, h.Transcript),
		CreatorUserID:  h.Discovery.CreatedBy.ID,
		StartedAt:      startedAt.UTC().Format(time.RFC3339Nano),
		EndedAt:        formatOptionalTime(endedAt),
		SummaryBlockID: children.SummaryBlockID, NotesBlockID: children.NotesBlockID,
		TranscriptBlockID: children.TranscriptBlockID,
		HasSummary:        h.Summary != "", HasNotes: h.Notes != "", HasTranscript: h.Transcript != "",
		UnresolvedAttendeeIDs: h.UnresolvedAttendeeIDs,
		ResolvedUsers:         h.ResolvedUsers,
		Warnings:              h.Warnings,
	}, json.Deterministic(true))
	if err != nil {
		return meetingarchive.Snapshot{}, fmt.Errorf("marshal Notion meeting metadata: %w", err)
	}
	raw, err := json.Marshal(rawEvidence{
		SchemaVersion: notionSnapshotVersion, Discovery: h.Discovery.Raw,
		MeetingBlock: rawForBlock(h.MeetingBlock),
		Summary:      rawForTree(h.SummaryTree), Notes: rawForTree(h.NotesTree),
		Transcript:     rawForTree(h.TranscriptTree),
		Canonical:      canonicalEvidence{Summary: h.Summary, Notes: h.Notes, Transcript: h.Transcript},
		AttendeeLabels: h.AttendeeLabels, ResolvedUsers: h.ResolvedUsers, Warnings: h.Warnings,
	}, json.Deterministic(true))
	if err != nil {
		return meetingarchive.Snapshot{}, fmt.Errorf("marshal Notion raw evidence: %w", err)
	}
	// The shared meetingcontent reader accepts at most 64 MiB. Raw roots and
	// canonical text duplicate admitted blocks, so the source byte budget alone
	// cannot guarantee a readable archive. Fail before replacing stored content.
	if len(raw) > 64<<20 {
		return meetingarchive.Snapshot{}, errArchiveEvidenceLimit
	}
	return meetingarchive.Snapshot{
		SourceID: sourceID, AccountEmail: accountEmail,
		SourceMessageID: h.Discovery.ID, SourceConversationID: h.Discovery.ID,
		Title: title, StartedAt: startedAt.UTC(), Body: body, Snippet: meetingarchive.Snippet(body),
		Metadata: metadata, Raw: raw, RawFormat: RawFormat,
		Attendees: append([]meetingarchive.Person(nil), h.Attendees...),
	}, nil
}

func (h *HydratedMeeting) times() (time.Time, time.Time) {
	calendar := h.Discovery.MeetingNotes.CalendarEvent
	recording := h.Discovery.MeetingNotes.Recording
	start := parseNotionTime(calendar.StartTime)
	if start.IsZero() {
		start = parseNotionTime(recording.StartTime)
	}
	if start.IsZero() {
		start = parseNotionTime(h.Discovery.CreatedTime)
	}
	end := parseNotionTime(calendar.EndTime)
	if end.IsZero() {
		end = parseNotionTime(recording.EndTime)
	}
	if !end.After(start) {
		end = time.Time{}
	}
	return start, end
}

func (h *HydratedMeeting) body(title string, start, end time.Time) string {
	lines := []string{title}
	when := "When: " + start.UTC().Format("2006-01-02 15:04")
	if !end.IsZero() {
		when += " - " + end.UTC().Format("15:04")
	}
	lines = append(lines, when)
	if len(h.AttendeeLabels) > 0 {
		lines = append(lines, "Attendees: "+strings.Join(h.AttendeeLabels, ", "))
	}
	sections := []struct{ label, content string }{
		{"Summary", h.Summary}, {"Notes", h.Notes}, {"Transcript", h.Transcript},
	}
	for _, section := range sections {
		if strings.TrimSpace(section.content) != "" {
			lines = append(lines, "", section.label+":", strings.TrimSpace(section.content))
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func rawForBlock(block *Block) jsontext.Value {
	if block == nil {
		return nil
	}
	return block.Raw
}

func rawForTree(tree blockTree) rawBlockTree {
	blocks := make([]jsontext.Value, 0, len(tree.Blocks))
	for i := range tree.Blocks {
		blocks = append(blocks, rawForBlock(&tree.Blocks[i]))
	}
	return rawBlockTree{Root: rawForBlock(tree.Root), Blocks: blocks, Complete: tree.Complete, Filtered: tree.Filtered, SourceLastEditedTime: tree.SourceLastEditedTime, Preserved: tree.Preserved, SourceMeetingBlock: tree.SourceMeetingBlock}
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func lifecycleForStatus(status, transcript string) string {
	normalized := strings.ToLower(strings.TrimSpace(status))
	switch {
	case strings.Contains(normalized, "fail"):
		return "failed"
	case strings.Contains(normalized, "delete"):
		return "deleted"
	case transcript != "" || strings.Contains(normalized, "ready") || strings.Contains(normalized, "complete"):
		return "ready"
	case normalized == "" || strings.Contains(normalized, "not_started") || strings.Contains(normalized, "progress") || strings.Contains(normalized, "processing"):
		return "pending"
	default:
		return "unknown"
	}
}

// userAnchor is the stable identity of a Notion user. The provider verifies
// the user's email, so every email a user has had links to the same person.
func userAnchor(userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ""
	}
	return meetingarchive.Anchor("notion-user", userID)
}
