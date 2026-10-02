package mcp

import "go.kenn.io/msgvault/pkg/client/generated"

func laneReadinessOperationalDefinitions() []operationalDefinition {
	return []operationalDefinition{
		newOperationalDefinition("get_lane_readiness", "Read sanitized configured and installed lane readiness, credential and consent states, fixed blockers and pending restart from the owning daemon. Does not probe providers; takes no path, environment or command arguments.", OperationFamilySources, closedObject(nil), outputSchemaFor[generated.LaneReadinessResponse](), false, false),
	}
}
