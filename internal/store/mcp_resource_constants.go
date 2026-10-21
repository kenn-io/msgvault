package store

// Shared native source and resource names keep scope predicates consistent.
const (
	sourceTypeIMAP         = "imap"
	mcpScopeSource         = "source"
	mcpScopeInbox          = "inbox"
	sourceTypeMatrix       = "matrix"
	mcpScopeConversation   = "conversation"
	mcpSourceMessageFamily = "msgvault.source_message_archived"
	mcpInboxMessageFamily  = "msgvault.inbox_message_archived"
)
