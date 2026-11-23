package api

// delegatedOperationAllowed admits only operations with a delegated authorization path.
func delegatedOperationAllowed(operationID string) bool {
	policy := operationPolicies[operationID]
	return policy.Delegated || policy.AgentReadPermission != ""
}
