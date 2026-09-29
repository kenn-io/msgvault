package api

const (
	CLIRunChatDraftCreateCommand = "chat-draft-create"
	CLIRunChatDraftGetCommand    = "chat-draft-get"
	CLIRunChatDraftEditCommand   = "chat-draft-edit"
	CLIRunChatDraftDeleteCommand = "chat-draft-delete"
)

// IsCLIRunChatDraftCreate reports whether args select local chat draft
// creation. Flag order is intentionally irrelevant to command recognition.
func IsCLIRunChatDraftCreate(args []string) bool {
	return len(args) > 0 && args[0] == CLIRunChatDraftCreateCommand
}

// IsCLIRunChatDraft reports whether args select any local chat draft command.
func IsCLIRunChatDraft(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case CLIRunChatDraftCreateCommand, CLIRunChatDraftGetCommand,
		CLIRunChatDraftEditCommand, CLIRunChatDraftDeleteCommand:
		return true
	default:
		return false
	}
}
