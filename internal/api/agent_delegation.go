package api

// delegatedOperationAllowed is an exact-match allowlist of operation IDs.
func delegatedOperationAllowed(operationID string) bool {
	switch operationID {
	case "runCLI", "getHealth", "controlCalendar":
		return true
	}
	return false
}
