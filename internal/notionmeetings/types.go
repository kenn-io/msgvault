// Package notionmeetings imports Notion AI Meeting Notes into the canonical
// msgvault meeting archive.
package notionmeetings

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"

	"go.kenn.io/msgvault/internal/meetingcontent"
)

const (
	SourceType       = "notion_meetings"
	ConversationType = "meeting"
	MessageType      = "meeting_transcript"
	RawFormat        = "notion_meeting_json"
	APIVersion       = "2026-03-11"
	DefaultBaseURL   = "https://api.notion.com"
)

type RichText struct {
	PlainText string `json:"plain_text"`
}

type UserRef struct {
	Object string `json:"object"`
	ID     string `json:"id"`
}

type Parent struct {
	Type         string `json:"type"`
	PageID       string `json:"page_id,omitempty"`
	DataSourceID string `json:"data_source_id,omitempty"`
}

type MeetingChildren struct {
	SummaryBlockID    string `json:"summary_block_id"`
	NotesBlockID      string `json:"notes_block_id"`
	TranscriptBlockID string `json:"transcript_block_id"`
}

type MeetingCalendarEvent struct {
	EventID   string   `json:"event_id,omitempty"`
	StartTime string   `json:"start_time"`
	EndTime   string   `json:"end_time"`
	Attendees []string `json:"attendees"`
}

type MeetingRecording struct {
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
}

type MeetingNotesData struct {
	IsFullPage    *bool                `json:"is_full_page,omitzero"`
	Title         []RichText           `json:"title"`
	Status        string               `json:"status"`
	Children      MeetingChildren      `json:"children"`
	CalendarEvent MeetingCalendarEvent `json:"calendar_event"`
	Recording     MeetingRecording     `json:"recording"`
}

type MeetingNote struct {
	URL            string                    `json:"url,omitempty"`
	Object         string                    `json:"object"`
	ID             string                    `json:"id"`
	Type           string                    `json:"type"`
	MeetingNotes   MeetingNotesData          `json:"meeting_notes"`
	CreatedTime    string                    `json:"created_time"`
	LastEditedTime string                    `json:"last_edited_time"`
	CreatedBy      UserRef                   `json:"created_by"`
	LastEditedBy   UserRef                   `json:"last_edited_by"`
	Parent         Parent                    `json:"parent"`
	HasChildren    bool                      `json:"has_children"`
	Archived       bool                      `json:"archived,omitzero"`
	InTrash        bool                      `json:"in_trash"`
	Extra          map[string]jsontext.Value `json:"-"`
	Raw            jsontext.Value            `json:"-"`
}

func (m *MeetingNote) UnmarshalJSON(data []byte) error {
	type plain MeetingNote
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var extra map[string]jsontext.Value
	if err := json.Unmarshal(data, &extra); err != nil {
		return err
	}
	for _, key := range []string{
		"object", "id", "type", "meeting_notes", "created_time",
		"last_edited_time", "created_by", "last_edited_by", "parent",
		"has_children", "in_trash", "archived", "url",
	} {
		delete(extra, key)
	}
	*m = MeetingNote(decoded)
	m.Extra = extra
	m.Raw = append(m.Raw[:0], data...)
	return nil
}

func (m MeetingNote) Title() string {
	var title strings.Builder
	for _, part := range m.MeetingNotes.Title {
		title.WriteString(part.PlainText)
	}
	return title.String()
}

type QueryResult struct {
	Results []MeetingNote  `json:"results"`
	HasMore bool           `json:"has_more"`
	Raw     jsontext.Value `json:"-"`
}

type RichTextBlock struct {
	RichText []RichText `json:"rich_text"`
}

type blockContent struct {
	RichText   []RichText     `json:"rich_text"`
	Caption    []RichText     `json:"caption"`
	Title      jsontext.Value `json:"title"`
	Expression string         `json:"expression"`
	Cells      [][]RichText   `json:"cells"`
}

type Block struct {
	Object           string        `json:"object"`
	ID               string        `json:"id"`
	Type             string        `json:"type"`
	Parent           Parent        `json:"parent"`
	HasChildren      bool          `json:"has_children"`
	Paragraph        RichTextBlock `json:"paragraph"`
	Heading1         RichTextBlock `json:"heading_1"`
	Heading2         RichTextBlock `json:"heading_2"`
	Heading3         RichTextBlock `json:"heading_3"`
	BulletedListItem RichTextBlock `json:"bulleted_list_item"`
	NumberedListItem RichTextBlock `json:"numbered_list_item"`
	Quote            RichTextBlock `json:"quote"`
	Callout          RichTextBlock `json:"callout"`
	Toggle           RichTextBlock `json:"toggle"`
	ToDo             RichTextBlock `json:"to_do"`
	content          blockContent
	Raw              jsontext.Value `json:"-"`
}

func (b *Block) UnmarshalJSON(data []byte) error {
	type plain Block
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*b = Block(decoded)
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if payload := fields[b.Type]; len(payload) > 0 {
		if err := json.Unmarshal(payload, &b.content); err != nil {
			return err
		}
	}
	b.Raw = append(b.Raw[:0], data...)
	return nil
}

func (b Block) PlainText() string {
	return meetingcontent.NotionBlockPlainText(b.Raw)
}

func richTextPlainText(parts []RichText) string {
	var text strings.Builder
	for _, part := range parts {
		text.WriteString(part.PlainText)
	}
	return text.String()
}

type BlockPage struct {
	Object     string         `json:"object"`
	Results    []Block        `json:"results"`
	NextCursor string         `json:"next_cursor"`
	HasMore    bool           `json:"has_more"`
	Raw        jsontext.Value `json:"-"`
}

type MarkdownPage struct {
	Object          string         `json:"object"`
	ID              string         `json:"id"`
	Markdown        string         `json:"markdown"`
	Truncated       bool           `json:"truncated"`
	UnknownBlockIDs []string       `json:"unknown_block_ids"`
	Raw             jsontext.Value `json:"-"`
}

type UserPerson struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

type User struct {
	Object string         `json:"object"`
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Person UserPerson     `json:"person"`
	Raw    jsontext.Value `json:"-"`
}

func (u *User) UnmarshalJSON(data []byte) error {
	type plain User
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = User(decoded)
	u.Raw = append(u.Raw[:0], data...)
	return nil
}

type UserPage struct {
	Object     string         `json:"object"`
	Results    []User         `json:"results"`
	NextCursor string         `json:"next_cursor"`
	HasMore    bool           `json:"has_more"`
	Raw        jsontext.Value `json:"-"`
}
