package api

// delegatedOperationAllowed admits only operations with a delegated authorization path.
func delegatedOperationAllowed(operationID string) bool {
	switch operationID {
	case "runCLI", "getHealth", "controlCalendar":
		return true
	}
	_, ok := agentReadPermissions[operationID]
	return ok
}
