package agentgrant

// ReadToolPermissions lists alternative permissions that admit a source-scoped
// MCP read tool. A tool absent here remains owner-only.
func ReadToolPermissions(name string) []Permission {
	switch name {
	case "search_messages", "search_metadata", "search_message_bodies", "list_messages", "aggregate", "search_by_domains":
		return []Permission{PermissionSearchRead}
	case "get_message", "list_thread", "search_in_message", "timeline":
		return []Permission{PermissionMessageRead}
	case "get_attachment":
		return []Permission{PermissionAttachmentRead}
	case "get_stats":
		return []Permission{PermissionStatsRead}
	case "list_calendar_events", "get_calendar_event":
		return []Permission{PermissionCalendarEventRead, PermissionMessageRead}
	}
	return nil
}
