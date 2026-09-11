package api

import "go.kenn.io/msgvault/internal/agentgrant"

type delegatedCLICommand struct {
	permission agentgrant.Permission
	executable bool
}

var delegatedCLICommands = map[string]delegatedCLICommand{
	CLIRunDraftReplyCommand: {agentgrant.PermissionDraftCreate, true},
	"draft-get":             {agentgrant.PermissionDraftRead, false},
	"draft-edit":            {agentgrant.PermissionDraftEdit, false},
	"draft-delete":          {agentgrant.PermissionDraftDelete, false},
}

func lookupDelegatedCLICommand(args []string) (delegatedCLICommand, bool) {
	if len(args) == 0 {
		return delegatedCLICommand{}, false
	}
	cmd, ok := delegatedCLICommands[args[0]]
	return cmd, ok
}
