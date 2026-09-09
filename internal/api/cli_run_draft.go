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
