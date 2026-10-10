package api

// delegatedOperationAllowed is an exact-match allowlist of operation IDs.
func delegatedOperationAllowed(operationID string) bool {
	switch operationID {
	case "previewScopedCardDAVPublication", "approveScopedCardDAVPublication", "reconcileScopedCardDAVPublication", "runCLI", "getHealth", "controlCalendar", "controlInbox", "listInboxCandidates", "getInboxContext", "getInboxTriageMappings", "previewInboxTriage", "applyInboxTriage", "getMCPCapabilities", "previewIdentityOperation", "applyIdentityOperation", "getIdentityOperationReceipt", "getPersonProfile", "patchPerson", "getPersonStructuredProfile", "patchPersonStructuredProfile", "listPersonAttributes", "setPersonAttribute", "clearPersonAttribute", "mergePersons":
		return true
	}
	return false
}
