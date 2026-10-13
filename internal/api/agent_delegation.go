package api

// delegatedOperationAllowed admits only operations with a delegated authorization path.
func delegatedOperationAllowed(operationID string) bool {
	switch operationID {
	case "getMCPCapabilities", "previewIdentityOperation", "applyIdentityOperation", "getIdentityOperationReceipt":
		return true
	case "runCLI", "getHealth", "controlCalendar", "getAgentTokenSelf", "previewScopedCardDAVPublication", "approveScopedCardDAVPublication", "reconcileScopedCardDAVPublication":
		return true
	case "getPersonProfile", "patchPerson", "getPersonStructuredProfile", "patchPersonStructuredProfile",
		"listPersonAttributes", "setPersonAttribute", "clearPersonAttribute", "mergePersons":
		return true
	}
	_, ok := agentReadPermissions[operationID]
	return ok
}
