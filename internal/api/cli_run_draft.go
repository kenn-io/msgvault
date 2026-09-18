package api

import "go.kenn.io/msgvault/internal/agentgrant"

// CLIRunDraftReplyCommand names the daemon CLI command that the daemon runs
// in-process instead of spawning a subprocess.
const CLIRunDraftReplyCommand = "draft-reply"

const (
	CLIRunDraftGetCommand    = "draft-get"
	CLIRunDraftEditCommand   = "draft-edit"
	CLIRunDraftDeleteCommand = "draft-delete"
)

// IsCLIRunDraftReply reports whether args invoke the in-process draft-reply
// route.
func IsCLIRunDraftReply(args []string) bool {
	return len(args) > 0 && args[0] == CLIRunDraftReplyCommand
}

func delegatedCLIRunAdmitted(args []string, grant *agentgrant.Grant) bool {
	return grant != nil && IsCLIRunDraftReply(args) && grant.HasPermission(agentgrant.PermissionDraftCreate)
}

// IsCLIRunDraftLifecycle reports whether args invoke one of the managed draft
// lifecycle routes that the daemon executes in-process.
func IsCLIRunDraftLifecycle(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case CLIRunDraftGetCommand, CLIRunDraftEditCommand, CLIRunDraftDeleteCommand:
		return true
	default:
		return false
	}
}

// CLIRunCodedError carries a fixed code for the client and the underlying
// cause for the daemon log. Clients only ever see Code.
type CLIRunCodedError struct {
	Code string
	Err  error
}

func (e *CLIRunCodedError) Error() string { return e.Code }

func (e *CLIRunCodedError) Unwrap() error { return e.Err }
