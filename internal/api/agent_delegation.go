package api

// delegatedOperationAllowed admits only operations with a delegated authorization path.
func delegatedOperationAllowed(operationID string) bool {
	if operationID == "runCLI" || operationID == "getHealth" {
		return true
	}
	_, ok := agentReadPermissions[operationID]
	return ok
}
