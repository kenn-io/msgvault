package notionmeetings

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingarchive"
)

type meetingMetadata struct {
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
	MarkdownTranscriptFallback bool           `json:"markdown_transcript_fallback,omitempty"`
	UnresolvedAttendeeIDs      []string       `json:"unresolved_attendee_ids,omitempty"`
	ResolvedUsers              []resolvedUser `json:"resolved_users,omitempty"`
	Warnings                   []string       `json:"warnings,omitempty"`
}

type rawBlockTree struct {
	Root  json.RawMessage   `json:"root,omitempty"`
	Pages []json.RawMessage `json:"pages,omitempty"`
}

type rawEvidence struct {
	SchemaVersion  int               `json:"schema_version"`
	Discovery      json.RawMessage   `json:"discovery"`
	MeetingBlock   json.RawMessage   `json:"meeting_block"`
	Summary        rawBlockTree      `json:"summary,omitzero"`
	Notes          rawBlockTree      `json:"notes,omitzero"`
	Transcript     rawBlockTree      `json:"transcript,omitzero"`
	PageMarkdown   json.RawMessage   `json:"page_markdown"`
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
	metadata, err := json.Marshal(meetingMetadata{
		Platform: SourceType, SourceIdentifier: identifier, MeetingNoteBlockID: h.Discovery.ID,
		PageID: h.Discovery.Parent.PageID, Status: h.Discovery.MeetingNotes.Status,
		Lifecycle:      lifecycleForStatus(h.Discovery.MeetingNotes.Status, h.Transcript),
		CreatorUserID:  h.Discovery.CreatedBy.ID,
		StartedAt:      startedAt.UTC().Format(time.RFC3339Nano),
		EndedAt:        formatOptionalTime(endedAt),
		SummaryBlockID: children.SummaryBlockID, NotesBlockID: children.NotesBlockID,
		TranscriptBlockID: children.TranscriptBlockID,
		HasSummary:        h.Summary != "", HasNotes: h.Notes != "", HasTranscript: h.Transcript != "",
		MarkdownTranscriptFallback: h.MarkdownTranscriptFallback,
		UnresolvedAttendeeIDs:      h.UnresolvedAttendeeIDs,
		ResolvedUsers:              h.ResolvedUsers,
		Warnings:                   h.Warnings,
	})
	if err != nil {
		return meetingarchive.Snapshot{}, fmt.Errorf("marshal Notion meeting metadata: %w", err)
	}
	raw, err := json.Marshal(rawEvidence{
		SchemaVersion: 1, Discovery: h.Discovery.Raw,
		MeetingBlock: rawForBlock(h.MeetingBlock),
		Summary:      rawForTree(h.SummaryTree), Notes: rawForTree(h.NotesTree),
		Transcript: rawForTree(h.TranscriptTree), PageMarkdown: rawForMarkdown(h.PageMarkdown),
		Canonical:      canonicalEvidence{Summary: h.Summary, Notes: h.Notes, Transcript: h.Transcript},
		AttendeeLabels: h.AttendeeLabels, ResolvedUsers: h.ResolvedUsers, Warnings: h.Warnings,
	})
	if err != nil {
		return meetingarchive.Snapshot{}, fmt.Errorf("marshal Notion raw evidence: %w", err)
	}
	return meetingarchive.Snapshot{
		SourceID: sourceID, AccountEmail: accountEmail,
		SourceMessageID: h.Discovery.ID, SourceConversationID: h.Discovery.ID,
		Title: title, StartedAt: startedAt.UTC(), Body: body, Snippet: meetingSnippet(body),
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

func rawForBlock(block *Block) json.RawMessage {
	if block == nil {
		return nil
	}
	return block.Raw
}

func rawForMarkdown(page *MarkdownPage) json.RawMessage {
	if page == nil {
		return nil
	}
	return page.Raw
}

func rawForTree(tree blockTree) rawBlockTree {
	return rawBlockTree{Root: rawForBlock(tree.Root), Pages: tree.Pages}
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

func meetingSnippet(body string) string {
	runes := []rune(strings.TrimSpace(body))
	if len(runes) > 200 {
		runes = runes[:200]
	}
	return string(runes)
}
