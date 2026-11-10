package mcp

import "strings"

// ArchiveFamilies describes source and integration families present in an archive.
// A nil snapshot preserves the catalog for embedders without archive discovery.
type ArchiveFamilies struct {
	Calendar, Meetings, Chat, Slack, Drafts, CardDAV, Kata bool
}

func (f *ArchiveFamilies) allows(name string) bool {
	if f == nil {
		return true
	}
	switch {
	case strings.HasPrefix(name, "calendar_"):
		return f.Calendar
	case strings.HasPrefix(name, "draft_"):
		return f.Drafts
	case strings.Contains(name, "_kata_") || name == ToolGetPersonAgenda:
		return f.Kata
	case strings.Contains(name, "_slack_"):
		return f.Slack
	}
	switch name {
	case ToolGetMeetingRecord:
		return f.Meetings || f.Calendar
	case ToolGetMeetingContext, ToolListMeetingActionItems, ToolGetMeetingMetrics:
		return f.Meetings
	case ToolFindChat:
		return f.Chat
	case ToolGetCardDAVPublication, ToolPreviewCardDAVPublication, ToolApproveCardDAVPublication, ToolSyncCardDAV, ToolGetCardDAVSyncStatus:
		return f.CardDAV
	}
	return true
}
