package api

import "go.kenn.io/msgvault/internal/agentgrant"

var delegatedCommandPermission = map[string]agentgrant.Permission{
	CLIRunDraftReplyCommand: agentgrant.PermissionDraftCreate,
	"draft-get":             agentgrant.PermissionDraftRead,
	"draft-edit":            agentgrant.PermissionDraftEdit,
	"draft-delete":          agentgrant.PermissionDraftDelete,
}

var delegatedExecutableCommands = map[string]bool{
	CLIRunDraftReplyCommand: true,
}

func delegatedCLIRunAdmitted(args []string, grant *agentgrant.Grant) bool {
	if len(args) == 0 || grant == nil {
		return false
	}
	permission, ok := delegatedCommandPermission[args[0]]
	if !ok || !delegatedExecutableCommands[args[0]] {
		return false
	}
	return grant.HasPermission(permission)
}
