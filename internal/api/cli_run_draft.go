package api

// CLIRunDraftReplyCommand names the daemon CLI command that the daemon runs
// in-process instead of spawning a subprocess.
const CLIRunDraftReplyCommand = "draft-reply"

// IsCLIRunDraftReply reports whether args invoke the in-process draft-reply
// route.
func IsCLIRunDraftReply(args []string) bool {
	return len(args) > 0 && args[0] == CLIRunDraftReplyCommand
}

// CLIRunCodedError carries a fixed code for the client and the underlying
// cause for the daemon log. Clients only ever see Code.
type CLIRunCodedError struct {
	Code string
	Err  error
}

func (e *CLIRunCodedError) Error() string { return e.Code }

func (e *CLIRunCodedError) Unwrap() error { return e.Err }

const (
	CLIRunDraftGetCommand    = "draft-get"
	CLIRunDraftEditCommand   = "draft-edit"
	CLIRunDraftDeleteCommand = "draft-delete"
)

// IsCLIRunDraftGet reports whether args invoke the in-process draft-get route.
func IsCLIRunDraftGet(args []string) bool {
	return len(args) > 0 && args[0] == CLIRunDraftGetCommand
}

// IsCLIRunDraftEdit reports whether args invoke the in-process draft-edit route.
func IsCLIRunDraftEdit(args []string) bool {
	return len(args) > 0 && args[0] == CLIRunDraftEditCommand
}

// IsCLIRunDraftDelete reports whether args invoke the in-process draft-delete route.
func IsCLIRunDraftDelete(args []string) bool {
	return len(args) > 0 && args[0] == CLIRunDraftDeleteCommand
}

// IsCLIRunDraftLifecycle reports whether args invoke any of the three
// draft lifecycle routes (get, edit, delete).
func IsCLIRunDraftLifecycle(args []string) bool {
	return IsCLIRunDraftGet(args) || IsCLIRunDraftEdit(args) || IsCLIRunDraftDelete(args)
}

// IsCLIRunDraftCommand reports whether args invoke any draft-related route,
// including draft-reply and the lifecycle commands.
func IsCLIRunDraftCommand(args []string) bool {
	return IsCLIRunDraftReply(args) || IsCLIRunDraftLifecycle(args)
}
